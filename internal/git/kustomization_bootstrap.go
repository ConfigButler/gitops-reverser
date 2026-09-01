// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"path"
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
//   - Only once per batch. The second new document in the same flush joins the root the first one
//     created, through the ordinary resources: append.
//
// The created root carries namespace: only when exactly one source namespace reaches the target.
// That is what makes it MEANINGFUL rather than an empty file, and it is the half that makes an
// accompanying serializeNamespace: false provable: the operator owns the file the omission depends
// on. With two namespaces there is no namespace to write, and such a target is refused before it
// reaches here anyway.
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

	rootPath := path.Join(orRootDir(wb.writeSubdir), "kustomization.yaml")
	entry := kustomizationEntryFor(rootPath, placement.Path)
	buf := wb.buffer(rootPath)
	if buf.current != nil {
		// Something is already at the path we would write. Leave it alone: overwriting a file the
		// scan did not model as a kustomization would destroy content on a guess.
		return nil
	}
	buf.current = []byte(renderCreatedKustomization(wb.namespaces.declaredFolderNamespace(), entry))

	created := &manifestanalyzer.KustomizationInfo{Path: rootPath, Resources: []string{entry}}
	wb.createdRoot = created
	recordKustomizationEntry(ctx, wb.target, kustomizationEntryAdded)
	log.FromContext(ctx).Info("Created kustomization.yaml for a folder that had none",
		"kustomization", rootPath, "entry", entry, "namespace", wb.namespaces.declaredFolderNamespace())
	// The document is registered in the bytes just written, so the caller must not append it
	// again; returning nil says "no further registration needed" for this first document.
	return nil
}

// renderCreatedKustomization is the created root's bytes. They are assembled as text rather than
// marshalled from a struct so the file reads the way a person would have written it: block
// sequence, two-space indent, no empty stanzas for the fields we do not set.
func renderCreatedKustomization(namespace, entry string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\n", createdKustomizationAPIVersion, createdKustomizationKind)
	if namespace != "" {
		fmt.Fprintf(&b, "namespace: %s\n", namespace)
	}
	fmt.Fprintf(&b, "resources:\n  - %s\n", entry)
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
