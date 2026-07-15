// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

// This file exposes the two facts the live writer needs to resolve a GitTarget
// subtree that renders through a base OUTSIDE its write scope (the classic
// base/ + overlays/{env} shape reached via `../../base`), plus the store-side
// signal the generalised write-fan-in precondition reads.
//
// The writer re-roots its scan at the lowest common ancestor of spec.path and every
// base it reaches (renderBase), so the whole store — materialisation, attribution,
// and the render oracle — runs in one coordinate system with no `..`-escaping paths,
// exactly as a self-contained render root already does. The write JAIL stays at
// spec.path: a planned write outside it is refused. Reads may reach shared context;
// writes never leave it. See docs/design/support-boundary/render-root-scoping.md §4.

// IsRemoteBaseEntry reports whether a resources/bases entry points outside the
// repository (a URL or a git/host-qualified path). The writer skips such an entry when
// resolving read scope: a remote base is refused before any build (the operator never
// fetches one), so it never becomes a directory to scan. It wraps the same predicate
// the acceptance gate and the pre-build refusal use, so the two cannot drift.
func IsRemoteBaseEntry(entry string) bool {
	return isRemoteResource(entry)
}

// ReachedByMultipleRenderRoots reports whether the managed file at the given
// render-scope-relative (slash) path is reached by more than one render root through the
// resources graph — the generalised write-fan-in signal. Writing a live change into such
// a file in place would change what every root that reads it renders, which is the one
// edit the write-fan-in = 1 invariant forbids. It generalises OverridesAmbiguousAt (which
// only fired when override entries were at stake) to any shared source file, so the guard
// no longer leans on the emergent side effect that a namespace-ambiguous base never
// becomes dirty. See docs/design/support-boundary/render-root-scoping.md §4.
func (s *ManifestStore) ReachedByMultipleRenderRoots(rel string) bool {
	_, ok := s.reachedByMultipleRoots[filepathToSlash(rel)]
	return ok
}

// renderRootFanIn returns the set of resource-file paths (slash) that more than one
// render root reaches through the resources graph. Roots and reachability are read off
// the kustomization graph exactly as renderRoots/reachedResourceFilesFrom compute them,
// so this shares one definition of "render root" with the rest of the analyzer.
func renderRootFanIn(kusts map[string]*kustomizationDoc) map[string]struct{} {
	counts := map[string]int{}
	for _, root := range renderRoots(kusts) {
		for file := range reachedResourceFilesFrom(root, kusts) {
			counts[file]++
		}
	}
	out := map[string]struct{}{}
	for file, n := range counts {
		if n > 1 {
			out[file] = struct{}{}
		}
	}
	return out
}
