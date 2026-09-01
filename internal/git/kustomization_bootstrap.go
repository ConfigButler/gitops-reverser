// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// createdKustomizationHeader is the root the operator writes when spec.placement.useKustomize asks
// for one. It is deliberately the smallest thing kustomize will build: an apiVersion, a kind, the
// namespace when there is one to write, and the resources: list the new document joins.
const (
	createdKustomizationAPIVersion = "kustomize.config.k8s.io/v1beta1"
	createdKustomizationKind       = "Kustomization"
)

// bootstrapKustomization creates the folder's kustomization.yaml when nothing governs the path a
// new document is landing at and the GitTarget asked for one. It returns the root the caller must
// register the document in, or nil when this write needs no such root.
//
// This is the only place the operator writes a file nobody asked for by name, which is why it is
// narrow on every axis:
//
//   - Only when spec.placement.useKustomize is true. Registering into a root that already exists is
//     an invariant and happens regardless (#319); this is exclusively the empty case.
//   - Only when NO kustomization governs the resolved path. The walk is bounded by the write jail,
//     so a root above spec.path is a read-only ancestor and its absence from the answer is not a
//     licence to write a competing root inside the jail.
//   - Only when the folder has no render root AT ALL (LayoutNone). A folder that already has one,
//     with this document landing outside it, must not gain a second: two render roots is
//     Ambiguous, and the target would stop placing new documents entirely. That placement is
//     REFUSED instead of written unrendered — see unrenderedPlacementRefusal.
//   - Only once per batch. The second new document in the same flush joins the root the first one
//     created, through the ordinary resources: append.
//
// The root it writes ADOPTS the folder, rather than listing only the document that triggered it.
// Enabling kustomize on a folder that already holds manifests and then writing a root that names
// one file would make every other file stop rendering the moment a consumer ran kustomize build:
// the files would still be in Git, and nothing would apply them. See adoptableEntries.
//
// The created root carries NO namespace:, ever. spec.serializeNamespace: false means the artifact
// does not encode its deployment namespace, and creating a root must not quietly change that
// contract by pinning the namespace one file up, where it is harder to see and impossible to
// override. The namespace comes from the documents when serializeNamespace is unset or true, and
// from the installer — a Flux Kustomization's targetNamespace, an Argo Application's
// destination.namespace — when it is false. See docs/design/created-root-namespace.md.
func (wb *writeBatch) bootstrapKustomization(
	ctx context.Context,
	placement manifestanalyzer.PlacementResult,
) *manifestanalyzer.KustomizationInfo {
	if wb.policy == nil || !wb.policy.UseKustomize || placement.Kustomization != nil {
		return nil
	}
	if wb.createdRoot != nil {
		return wb.createdRoot
	}
	if manifestanalyzer.GoverningKustomization(wb.store, wb.writeSubdir, placement.Path) != nil {
		// A root governs the path and simply already lists it, which is the case
		// PlacementResult.Kustomization cannot be told apart from "no root at all".
		return nil
	}
	if wb.layout.Reason != manifestanalyzer.LayoutNone {
		// The folder HAS a render root; this document just landed somewhere that root does not
		// govern, which a declared template can do. Writing a second root beside the first would
		// make the folder cover two render roots — Ambiguous — and the target would stop placing
		// new documents at all. One unregistered file is a smaller fault than a folder that has
		// stopped accepting writes, and the ancestor walk (#319) already registers everything the
		// existing root does govern.
		return nil
	}

	rootPath := path.Join(orRootDir(wb.writeSubdir), "kustomization.yaml")
	buf := wb.buffer(rootPath)
	if buf.current != nil {
		// Something is already at the path we would write. Leave it alone: overwriting a file the
		// scan did not model as a kustomization would destroy content on a guess.
		return nil
	}
	entries := wb.adoptableEntries(rootPath, placement.Path)
	buf.current = []byte(renderCreatedKustomization(entries))

	created := &manifestanalyzer.KustomizationInfo{Path: rootPath, Resources: entries}
	wb.createdRoot = created
	for range entries {
		recordKustomizationEntry(ctx, wb.target, kustomizationEntryAdded)
	}
	log.FromContext(ctx).Info("Created kustomization.yaml for a folder that had none",
		"kustomization", rootPath, "entries", entries, "adopted", len(entries)-1)
	// The document is registered in the bytes just written, so the caller must not append it
	// again; returning nil says "no further registration needed" for this first document.
	return nil
}

// adoptableEntries is the created root's resources: list — the document being placed, plus every
// managed document already in the folder that no kustomization governs.
//
// The adoption is the point. A root that listed only the new document would silently unrender every
// file already in the folder: they stay in Git, they look mirrored, and the moment a consumer runs
// kustomize build against the folder they are gone from the output. Turning a folder into a
// kustomize folder means the folder, not the one document that happened to arrive first.
//
// A file some OTHER kustomization already governs is left out. Listing it here would render it
// twice, once through each root, which kustomize reports as a duplicate resource and refuses to
// build. The caller only creates a root for a folder with no render root at all, so this is
// defensive rather than routine — a kustomization inside the jail that is not a render root of it
// (a base something above spec.path reads) is the shape that reaches it.
//
// Paths are relative to the created root and sorted, so the file reads in a stable order and two
// runs of the same flush produce the same bytes.
func (wb *writeBatch) adoptableEntries(rootPath, documentPath string) []string {
	entries := []string{kustomizationEntryFor(rootPath, documentPath)}
	for filePath := range wb.store.FilesByPath {
		if filePath == documentPath || !pathWithinWriteScope(wb.writeSubdir, filePath) {
			continue
		}
		if manifestanalyzer.GoverningKustomization(wb.store, wb.writeSubdir, filePath) != nil {
			continue
		}
		entries = append(entries, kustomizationEntryFor(rootPath, filePath))
	}
	sort.Strings(entries)
	return entries
}

// pathWithinWriteScope reports whether a scanned path is inside the write jail. With no jail the
// whole scanned subtree is the target's own.
func pathWithinWriteScope(writeSubdir, filePath string) bool {
	if writeSubdir == "" {
		return true
	}
	return strings.HasPrefix(filePath, writeSubdir+"/")
}

// renderCreatedKustomization is the created root's bytes: an apiVersion, a kind, and the resources:
// list. Nothing else — no namespace:, and no stanza for a field the operator does not set. They are
// assembled as text rather than marshalled from a struct so the file reads the way a person would
// have written it: block sequence, two-space indent.
func renderCreatedKustomization(entries []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\n", createdKustomizationAPIVersion, createdKustomizationKind)
	b.WriteString("resources:\n")
	for _, entry := range entries {
		fmt.Fprintf(&b, "  - %s\n", entry)
	}
	return b.String()
}

// kustomizationEntryFor expresses a document's path relative to the kustomization that lists it,
// which is how kustomize reads a resources: entry.
func kustomizationEntryFor(rootPath, documentPath string) string {
	dir := path.Dir(rootPath)
	if dir == "." {
		return documentPath
	}
	return strings.TrimPrefix(documentPath, dir+"/")
}

// orRootDir reads the write jail as a directory: an empty jail is the scan root itself.
func orRootDir(writeSubdir string) string {
	if writeSubdir == "" {
		return "."
	}
	return writeSubdir
}
