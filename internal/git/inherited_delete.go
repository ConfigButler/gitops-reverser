// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"fmt"
	"path"
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
//   - removing an object the folder already does not render is a no-op (removeDocument);
//   - when the object comes back, the owned patch and its patches: entry are retired, and the
//     render is verified to hold the object again;
//   - a path holding anything else (a human-edited patch, or an unrelated file of the same name)
//     refuses the batch, since overwriting it could lose work and retiring it could keep a deletion
//     the operator never made.

// inheritedDeletePatchMarker is the first line of every delete patch the operator writes. It tells
// a reader who owns the file, and it is part of the bytes ownership is judged by. It is the only
// ownership signal: a patch without it may be a human's that happens to match the template, and
// generic matching bytes cannot prove who wrote them.
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
	meta := "  name: " + id.Name + "\n"
	if id.Namespace != "" {
		meta = "  namespace: " + id.Namespace + "\n" + meta
	}
	return []byte(fmt.Sprintf(
		"%sapiVersion: %s\nkind: %s\nmetadata:\n%s$patch: delete\n",
		inheritedDeletePatchMarker, id.APIVersion, id.Kind, meta))
}

// ownsInheritedDeletePatch reports whether content is exactly the delete patch the operator writes
// for id, marker included.
func ownsInheritedDeletePatch(content []byte, id manifestedit.Identity) bool {
	return bytes.Equal(content, inheritedDeletePatchDocument(id))
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

// unrenderedResources is every managed document the folder holds but does not render: a
// `$patch: delete` takes it out, whether the operator wrote that patch or someone else did. The
// target's render is what the mirror is compared against, so a snapshot sweep neither removes such a
// document again nor counts it as retained. Rendered membership is the question here; whether the
// operator may edit the patch doing the hiding is a separate one, answered at re-entry.
func (wb *writeBatch) unrenderedResources() map[types.ResourceIdentifier]bool {
	var out map[types.ResourceIdentifier]bool
	for dm, ref := range wb.docLoc {
		if dm.ResourceIdentity == nil ||
			wb.store.Renders(ref.FilePath, dm.ManifestIdentity.Kind, dm.ManifestIdentity.Name) {
			continue
		}
		if out == nil {
			out = map[types.ResourceIdentifier]bool{}
		}
		out[*dm.ResourceIdentity] = true
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

// hiddenByUnownedPatchRefusal refuses the upsert of an object whose document is in the folder but out
// of its render, hidden by a `$patch: delete` the operator does not own. The operator cannot tell
// which patch that is or whether a human meant it, so it neither edits nor retires it.
func hiddenByUnownedPatchRefusal(docPath string, id manifestedit.Identity, overlay string) error {
	where := ""
	if overlay != "" {
		where = " through " + overlay
	}
	return &manifestanalyzer.AcceptanceRefusedError{Issues: []manifestanalyzer.AcceptanceIssue{{
		Kind:     manifestanalyzer.IssueUnownedDeletePatch,
		Path:     docPath,
		Solvable: true,
		Actor:    manifestanalyzer.ActorRepositoryAuthor,
		Message: fmt.Sprintf("%s/%s is in the cluster, but a $patch: delete the operator did not write "+
			"keeps %s out of the render%s", id.Kind, id.Name, docPath, where),
	}}}
}

// authorInheritedDelete removes an inherited object by writing its owned `$patch: delete` inside
// spec.path and naming it in the overlay's patches:, and declares the Removed intent so the
// re-render oracle proves the object leaves the render — a patch that fails to match is refused
// there, and the base is never touched.
func (wb *writeBatch) authorInheritedDelete(
	ctx context.Context,
	identifier types.ResourceIdentifier,
	target deleteTarget,
	d inheritedDelete,
) (bool, error) {
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
	wb.setRenderedInBatch(target.filePath, target.id.Kind, target.id.Name, false)
	return true, nil
}

// retireInheritedDelete undoes an owned delete patch for an inherited object that is present in the
// cluster again: the patch file goes, and so does its patches: entry. It returns the base document's
// path when this batch has brought the object back into the render, now or by an earlier event, so
// the caller plans the upsert against the staged render; retired reports that it happened now, in
// which case the caller also owes the oracle an intent that the object renders again.
//
// It runs before the upsert, so the upsert edits the base document the render will hold rather
// than one it hides.
func (wb *writeBatch) retireInheritedDelete(ctx context.Context, event Event) (string, bool, error) {
	id, ok := manifestIdentity(event.Object)
	if !ok {
		return "", false, nil
	}
	dm := wb.store.ByManifestIdentity[id]
	if dm == nil {
		return "", false, nil
	}
	raw := rawManifestIDForCurrentBytes(id, dm)
	basePath := wb.docLoc[dm].FilePath
	kind, name := dm.ManifestIdentity.Kind, dm.ManifestIdentity.Name
	rendered := wb.rendersInBatch(basePath, kind, name)
	d, inherited := wb.inheritedDeleteFor(basePath, raw)
	if !inherited {
		if !rendered {
			return "", false, hiddenByUnownedPatchRefusal(basePath, raw, "")
		}
		return "", false, nil
	}
	patchBuf := wb.buffer(d.patchPath)
	if patchBuf.current == nil {
		// No owned patch hides the object, so if it is out of the render, a patch the operator did
		// not write keeps it out. Upserting the document would edit bytes the render never shows
		// and report the object mirrored while the folder still builds without it.
		if !rendered {
			return "", false, hiddenByUnownedPatchRefusal(basePath, raw, d.overlay)
		}
		if wb.rendered[renderKey{path: basePath, kind: kind, name: name}] {
			// An earlier event in this batch retired the patch; the pre-batch store still has the
			// object hidden, so this upsert too is planned against the staged render.
			return basePath, false, nil
		}
		return "", false, nil
	}
	if !ownsInheritedDeletePatch(patchBuf.current, raw) {
		return "", false, unownedDeletePatchRefusal(d, raw)
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
	wb.setRenderedInBatch(basePath, kind, name, true)
	return basePath, true, nil
}
