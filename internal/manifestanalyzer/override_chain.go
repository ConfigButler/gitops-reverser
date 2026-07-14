// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file derives the kustomize override chain governing each document from
// kustomize's own provenance, instead of walking the resources graph by hand.
//
// Every rendered object carries alpha.config.kubernetes.io/transformations: the
// ordered list of which kustomization configured which builtin transformer. Filtered
// to the two transformers behind the edit-through channels, that IS the chain — read
// off the renderer that applies it rather than reconstructed from a DFS that has to
// re-derive kustomize's build order, its cycle rules, and its diamond behaviour.
//
// The chain is attributed per OBJECT, not per file. kustomize records the
// transformers that touched each resource, so a Deployment and a Service sharing a
// file are attributed separately and correctly.
//
// See docs/design/support-boundary/kustomize-support-boundary.md §7.

// reasonRenderFailed marks a render root kustomize could not build. The folder is
// refused: if the build fails, Flux cannot deploy it either.
const reasonRenderFailed manifestedit.DiagReason = "kustomize-build-failed"

// chainKey identifies a rendered document: the source file kustomize says produced
// it, plus its kind and name. Namespace is deliberately absent — a namespace
// transformer rewrites it, and the source document may not carry one at all.
type chainKey struct {
	originPath string
	kind       string
	name       string
}

// renderRoots returns the kustomization directories no other kustomization in the
// subtree references — the directories a build would be invoked on — in sorted order
// for deterministic walks.
//
// This is the one piece of graph reasoning we keep: kustomize renders the root you
// hand it, so something has to decide WHICH directories are roots. Everything the
// walk used to do beyond that — build order, the override chain, the diamond — now
// comes from the renderer itself.
func renderRoots(kusts map[string]*kustomizationDoc) []string {
	referenced := map[string]struct{}{}
	for dir, k := range kusts {
		for _, entry := range k.resources {
			target := cleanJoin(dir, entry)
			if target == "" {
				continue
			}
			if _, ok := kusts[target]; ok {
				referenced[target] = struct{}{}
			}
		}
	}
	roots := make([]string, 0, len(kusts))
	for dir := range kusts {
		if _, ok := referenced[dir]; !ok {
			roots = append(roots, dir)
		}
	}
	sort.Strings(roots)
	return roots
}

// renderChains renders every render root and returns, per rendered document, the
// override chain governing it — or an ambiguity marker when more than one render
// root reaches the same document with DIFFERENT chains, which is the fan-in > 1 case
// we refuse to route through.
//
// It also returns, per kustomization path, the roots that FAILED to build. Those
// must refuse the folder, and it is important to see why a silent skip would be
// unsafe rather than merely unhelpful: a root that does not build yields no chain,
// so no ambiguity is recorded, so the write-fan-in guard never fires — and the
// writer would then write straight through into a base shared by two render paths.
// A diamond (one root reaching a base through two overlays) is exactly this shape:
// kustomize refuses it with "may not add resource with an already registered id",
// which means Flux cannot deploy the folder either. If kustomize cannot build it, we
// cannot reason about it, and we say so.
func renderChains(
	files []manifestedit.FileContent,
	kusts map[string]*kustomizationDoc,
) (map[chainKey]*overrideAssignment, map[string]string) {
	out := map[chainKey]*overrideAssignment{}
	failed := map[string]string{}

	for _, rootDir := range renderRoots(kusts) {
		rendered, err := renderRoot(files, rootDir)
		if err != nil {
			if doc := kusts[rootDir]; doc != nil {
				failed[doc.path] = err.Error()
			}
			continue
		}
		for _, ro := range rendered {
			if ro.OriginPath == "" {
				continue // a generated resource: it has no source document to edit
			}
			key := chainKey{
				originPath: ro.OriginPath,
				kind:       ro.Object.GetKind(),
				name:       ro.Object.GetName(),
			}
			record(out, key, chainOf(ro, kusts))
		}
	}
	return out, failed
}

// chainOf reads the override chain kustomize applied to one object: the images: and
// replicas: entries of every kustomization whose ImageTagTransformer or
// ReplicaCountTransformer touched it, in the order kustomize ran them (innermost
// base first).
func chainOf(ro renderedObject, kusts map[string]*kustomizationDoc) *KustomizeOverrides {
	ov := &KustomizeOverrides{}
	for _, tr := range ro.TransformedBy {
		doc := kusts[slashDir(tr.ConfiguredIn)]
		if doc == nil {
			continue
		}
		switch tr.Kind {
		case imageTagTransformer:
			ov.Images = append(ov.Images, doc.images...)
		case replicaCountTransformer:
			ov.Replicas = append(ov.Replicas, doc.replicas...)
		}
	}
	if len(ov.Images) == 0 && len(ov.Replicas) == 0 {
		return nil
	}
	return ov
}

// record accumulates one root's view of a document. Two roots agreeing on the chain
// is fine — the same document rendered the same way twice. Two roots DISAGREEING is
// the diamond: the file is shared context, and an edit routed through either chain
// would change what the other root renders.
//
// anyOverrides preserves the existing narrowness of the fan-in refusal: a base
// document reached by two roots that declare no images:/replicas: at all is shared
// context, but nothing is at stake in it, so it is not refused.
func record(out map[chainKey]*overrideAssignment, key chainKey, ov *KustomizeOverrides) {
	fp := fingerprint(ov)
	prev, seen := out[key]
	if !seen {
		out[key] = &overrideAssignment{
			chainKeys:    map[string]struct{}{fp: {}},
			overrides:    ov,
			anyOverrides: ov != nil,
		}
		return
	}
	if _, same := prev.chainKeys[fp]; same {
		return // the same chain, reached twice; not an ambiguity
	}
	prev.chainKeys[fp] = struct{}{}
	prev.anyOverrides = prev.anyOverrides || ov != nil
	prev.overrides = nil // more than one distinct chain: route through none of them
}

// fingerprint reduces a chain to a comparable string, so two roots reaching one
// document can be compared for agreement.
func fingerprint(ov *KustomizeOverrides) string {
	if ov == nil {
		return ""
	}
	var b strings.Builder
	for _, e := range ov.Images {
		b.WriteString(e.Source)
		b.WriteByte('\x00')
		b.WriteString(e.Name)
		b.WriteByte('\x00')
		b.WriteString(e.NewName + "|" + e.NewTag + "|" + e.Digest)
		b.WriteByte('\x01')
	}
	b.WriteByte('\x02')
	for _, e := range ov.Replicas {
		b.WriteString(e.Source)
		b.WriteByte('\x00')
		b.WriteString(e.Name)
		b.WriteByte('\x01')
	}
	return b.String()
}
