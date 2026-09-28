// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// An object an overlay INHERITS from a read-only base is removed by authoring a `$patch: delete`
// in the overlay, never by editing the base: the base is out of the write jail and shared by other
// environments. The operator OWNS that patch. Ownership is the file's exact bytes at its
// deterministic path, so it lives in Git and survives a restart, and an edit to the file ends it:
//
//   - removing an object whose owned patch is already in place is a no-op;
//   - when the object comes back, the owned patch and its patches: entry are retired, and the
//     render is verified to hold the object again;
//   - a path holding anything else (a human-edited patch, or an unrelated file of the same name)
//     refuses the batch, since overwriting it could lose work and retiring it could keep a deletion
//     the operator never made.

// inheritedDeletePatchMarker is the first line of every delete patch the operator writes. It tells
// a reader who owns the file, and it is part of the bytes ownership is judged by.
const inheritedDeletePatchMarker = "# Written by gitops-reverser: removes an object this overlay inherits. " +
	"Editing this file hands it back to you.\n"

// inheritedDeletePatchName is the deterministic file name for an inherited-object delete patch:
// kind and name are DNS-safe, and the overlay resolves one namespace, so kind+name is unique in
// its render.
func inheritedDeletePatchName(id manifestedit.Identity) string {
	return strings.ToLower(id.Kind) + "-" + id.Name + "-delete.yaml"
}

// inheritedDeletePatchDocument is the strategic-merge `$patch: delete` document that removes the
// object from the overlay's render. kustomize matches a strategic-merge patch by full identity —
// apiVersion/kind/namespace/name — so the patch pins the object's namespace when it has one
// (a base that declares the namespace, or the live object's namespace). If the proposed patch
// does not match the render, the oracle refuses the flush; nothing is committed on a bad match.
func inheritedDeletePatchDocument(id manifestedit.Identity) []byte {
	return append([]byte(inheritedDeletePatchMarker), unmarkedInheritedDeletePatch(id)...)
}

// unmarkedInheritedDeletePatch is the patch body, and the whole file as releases before the
// ownership marker wrote it. Those files were written by the operator too, so they stay owned.
func unmarkedInheritedDeletePatch(id manifestedit.Identity) []byte {
	meta := "  name: " + id.Name + "\n"
	if id.Namespace != "" {
		meta = "  namespace: " + id.Namespace + "\n" + meta
	}
	return []byte(fmt.Sprintf(
		"apiVersion: %s\nkind: %s\nmetadata:\n%s$patch: delete\n",
		id.APIVersion, id.Kind, meta))
}

// ownsInheritedDeletePatch reports whether content is a delete patch the operator wrote for id.
func ownsInheritedDeletePatch(content []byte, id manifestedit.Identity) bool {
	return bytes.Equal(content, inheritedDeletePatchDocument(id)) ||
		bytes.Equal(content, unmarkedInheritedDeletePatch(id))
}

// inheritedDelete is where one inherited object's delete patch lives: the overlay kustomization
// that lists it, the patch file, and the entry naming it in the overlay's patches:.
type inheritedDelete struct {
	overlay   string
	patchPath string
	entry     string
}

// inheritedDeleteFor returns where the delete patch for the document at filePath with raw identity
// id lives, and false when the document is not inherited through an overlay: an in-jail document
// is removed from its own file instead.
func (wb *writeBatch) inheritedDeleteFor(filePath string, id manifestedit.Identity) (inheritedDelete, bool) {
	overlay := wb.overlayAuthorKustomization(filePath)
	if overlay == "" {
		return inheritedDelete{}, false
	}
	entry := inheritedDeletePatchName(id)
	return inheritedDelete{
		overlay:   overlay,
		patchPath: cleanSlash(path.Join(path.Dir(overlay), entry)),
		entry:     entry,
	}, true
}

// suppressed reports whether the owned delete patch is in force: the operator's file is at the
// path and the overlay lists it, as the batch's store read the folder.
func (wb *writeBatch) suppressed(d inheritedDelete, id manifestedit.Identity) bool {
	info := wb.store.Kustomizations[wb.writeSubdir]
	if info == nil || !slices.Contains(info.Patches, d.patchPath) {
		return false
	}
	buf := wb.buffer(d.patchPath)
	return buf.current != nil && ownsInheritedDeletePatch(buf.current, id)
}

// suppressedResources is every managed document the overlay's owned delete patches already take out
// of the render. They are not part of what the target's render holds, so a snapshot sweep neither
// removes them again nor counts them as retained.
func (wb *writeBatch) suppressedResources() map[types.ResourceIdentifier]bool {
	var out map[types.ResourceIdentifier]bool
	for dm, ref := range wb.docLoc {
		if dm.ResourceIdentity == nil {
			continue
		}
		id := rawManifestIDForCurrentBytes(dm.ManifestIdentity, dm)
		if d, inherited := wb.inheritedDeleteFor(ref.FilePath, id); inherited && wb.suppressed(d, id) {
			if out == nil {
				out = map[types.ResourceIdentifier]bool{}
			}
			out[*dm.ResourceIdentity] = true
		}
	}
	return out
}

// unownedDeletePatchRefusal refuses the batch over a delete patch path the operator does not own.
func unownedDeletePatchRefusal(d inheritedDelete, id manifestedit.Identity) error {
	return &manifestanalyzer.AcceptanceRefusedError{Issues: []manifestanalyzer.AcceptanceIssue{{
		Kind:     manifestanalyzer.IssueUnownedDeletePatch,
		Path:     d.patchPath,
		Solvable: true,
		Actor:    manifestanalyzer.ActorRepositoryAuthor,
		Message: fmt.Sprintf("%s/%s is inherited from a base, and %s holds content the operator did not write",
			id.Kind, id.Name, d.patchPath),
	}}}
}

// authorInheritedDelete removes an inherited object by writing its owned `$patch: delete` inside
// spec.path and naming it in the overlay's patches:, and declares the Removed intent so the
// re-render oracle proves the object leaves the render — a patch that fails to match is refused
// there, and the base is never touched. It reports whether it removed anything: an object its owned
// patch already removes is a no-op.
func (wb *writeBatch) authorInheritedDelete(
	ctx context.Context,
	identifier types.ResourceIdentifier,
	target deleteTarget,
	d inheritedDelete,
) (bool, error) {
	if wb.suppressed(d, target.id) {
		return false, nil
	}
	patchBuf := wb.buffer(d.patchPath)
	if patchBuf.current != nil && !ownsInheritedDeletePatch(patchBuf.current, target.id) {
		return false, unownedDeletePatchRefusal(d, target.id)
	}
	patchBuf.current = inheritedDeletePatchDocument(target.id)

	buf := wb.buffer(d.overlay)
	res, diags := manifestedit.AppendKustomizationPatch(d.overlay, buf.current, d.entry)
	switch res.Mode {
	case manifestedit.EditPatched:
		buf.current = res.Content
		log.FromContext(ctx).Info("Authored $patch: delete for an inherited object",
			"kustomization", d.overlay, "patch", d.entry, "resource", identifier.String())
	case manifestedit.EditNoChange:
	case manifestedit.EditSkipped, manifestedit.EditWholeReplace, manifestedit.EditDeleted:
		// The overlay cannot name the patch, so the object would stay rendered. The oracle refuses
		// the batch on the Removed intent below; the patch file is left for that refusal to name.
		logManifestDiagnostics(ctx, diags)
	}
	wb.intend(manifestanalyzer.WriteIntent{
		SourcePath: target.filePath,
		Kind:       target.id.Kind,
		Name:       target.id.Name,
		Removed:    true,
	})
	// A $patch: delete leaves every file in place and still takes the object out of the render,
	// so it counts as a removal for spec.onRefusal exactly as an outright deletion does: if this
	// flush is refused, re-applying what Git holds puts the object back.
	wb.recordDocumentRemoval(patchBuf)
	wb.putToKustomize = true
	return true, nil
}

// retireInheritedDelete undoes an owned delete patch for an inherited object that is present in the
// cluster again: the patch file goes, and so does its patches: entry. It returns the base document's
// path when it retired one, in which case the caller owes the oracle an intent that the object
// renders again, and "" otherwise.
//
// It runs before the upsert, so the upsert edits the base document the render will hold rather
// than one it hides.
func (wb *writeBatch) retireInheritedDelete(ctx context.Context, event Event) (string, error) {
	id, ok := manifestIdentity(event.Object)
	if !ok {
		return "", nil
	}
	dm := wb.store.ByManifestIdentity[id]
	if dm == nil {
		return "", nil
	}
	raw := rawManifestIDForCurrentBytes(id, dm)
	basePath := wb.docLoc[dm].FilePath
	d, inherited := wb.inheritedDeleteFor(basePath, raw)
	if !inherited {
		return "", nil
	}
	patchBuf := wb.buffer(d.patchPath)
	if patchBuf.current == nil {
		return "", nil
	}
	if !ownsInheritedDeletePatch(patchBuf.current, raw) {
		return "", unownedDeletePatchRefusal(d, raw)
	}
	patchBuf.current = nil

	buf := wb.buffer(d.overlay)
	res, diags := manifestedit.RemoveKustomizationPatch(d.overlay, buf.current, d.entry)
	switch res.Mode {
	case manifestedit.EditPatched:
		buf.current = res.Content
	case manifestedit.EditNoChange:
	case manifestedit.EditSkipped, manifestedit.EditWholeReplace, manifestedit.EditDeleted:
		// The entry would outlive its file and the overlay would not build; the oracle refuses the
		// batch on that rather than committing it.
		logManifestDiagnostics(ctx, diags)
	}
	log.FromContext(ctx).Info("Retired $patch: delete for an inherited object that is back",
		"kustomization", d.overlay, "patch", d.entry, "resource", event.Identifier.String())
	wb.putToKustomize = true
	return basePath, nil
}
