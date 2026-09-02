// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the post-scan layout resolution: what the folder's shape implies about where
// new documents go and whether they carry their own namespace, computed from a scan and from
// nothing else.
//
// It reports the ladder rung placement WILL take rather than the one it took, so a refusal or a
// surprising destination can be explained from status instead of from the logs. It is not a
// preview of the target's output: to see that, point a target at a scratch branch and read the
// commits (docs/layout/model.md § "Previewing a target: point it at a scratch branch").
//
// Nothing here depends on a placement having happened. See
// docs/layout/model.md § "status.placement, and the post-scan pass".

// LayoutReason is the resolved shape of a GitTarget folder, and it is a condition reason on
// LayoutResolved rather than a status field: every consumer in this ecosystem already reads
// reasons from conditions, and a bespoke field would be a second place to look.
type LayoutReason string

const (
	// LayoutSingleKustomization is a folder governed by exactly one supported, writable
	// kustomization. A new document lands beside it and joins its resources:.
	LayoutSingleKustomization LayoutReason = "SingleKustomization"
	// LayoutAmbiguous is a folder covering more than one render root. Placement declines to
	// pick one rather than guessing, so the folder is not a write partition: point the target
	// at a leaf instead. See test/fixtures/layout-corpus/shapes/README.md § "Why only a leaf can be a
	// kustomize target".
	LayoutAmbiguous LayoutReason = "Ambiguous"
	// LayoutNone is a folder with no supported kustomization at all. New documents land at a
	// declared template's path, or at the built-in canonical path.
	LayoutNone LayoutReason = "None"
)

// LayoutResolution is what one scan resolved about a folder's layout.
type LayoutResolution struct {
	// Reason is the verdict, projected onto the LayoutResolved condition.
	Reason LayoutReason
	// Mode is how the folder is written — plain files, a self-contained kustomize root, or an
	// overlay over a base outside the write scope. Empty under Ambiguous, which has no single
	// answer.
	Mode LayoutMode
	// RenderRoot is the governing kustomization's directory relative to the write scope, "."
	// for the scope itself. Empty for every reason but LayoutSingleKustomization.
	RenderRoot string
	// RenderRoots is every writable render root the scan found, sorted. It is what makes an
	// Ambiguous message able to name the folders it actually covers instead of only counting
	// them.
	RenderRoots []string
	// ReadOnlyBases is every kustomization the scan holds that lies OUTSIDE the write scope,
	// relative to it, sorted. Non-empty exactly when Mode is LayoutModeKustomizeOverlay, and it
	// is what a WriteBoundaryRefused message is predictable from.
	ReadOnlyBases []string
}

// LayoutMode is how a folder is written. It mirrors v1alpha3.PlacementMode, which is the
// published form; the duplication is the usual one-way dependency rule — the analyzer does not
// import the API types.
type LayoutMode string

const (
	// LayoutModePlain is a folder no kustomization governs.
	LayoutModePlain LayoutMode = "Plain"
	// LayoutModeKustomizeRoot is a self-contained folder governed by exactly one kustomization.
	LayoutModeKustomizeRoot LayoutMode = "KustomizeRoot"
	// LayoutModeKustomizeOverlay is a folder governed by one kustomization that renders a base
	// outside the write scope.
	LayoutModeKustomizeOverlay LayoutMode = "KustomizeOverlay"
)

// ResolveLayout resolves a folder's layout from a scan.
//
// writeScope is the write jail relative to the scanned root — empty for a self-contained
// subtree, non-empty only when render-root scoping anchored the scan past spec.path into a base
// the folder renders. Read scope is wider than write scope, always, so a base above the jail is
// read to render the folder and is never a candidate root for it.
func ResolveLayout(store *ManifestStore, writeScope string) LayoutResolution {
	roots := writableRenderRoots(store, writeScope)
	bases := readOnlyBases(store, writeScope)
	resolution := LayoutResolution{RenderRoots: roots, ReadOnlyBases: bases}

	switch {
	case len(roots) == 0:
		resolution.Reason = LayoutNone
		resolution.Mode = LayoutModePlain
	case len(roots) == 1:
		resolution.Reason = LayoutSingleKustomization
		resolution.RenderRoot = relativeToScope(roots[0], writeScope)
		// The presence of a base OUTSIDE the write scope is what separates an overlay from a
		// self-contained root, and it is the same condition every write-boundary refusal turns
		// on — so the two cannot disagree about which folder is which.
		if len(bases) > 0 {
			resolution.Mode = LayoutModeKustomizeOverlay
		} else {
			resolution.Mode = LayoutModeKustomizeRoot
		}
	default:
		resolution.Reason = LayoutAmbiguous
		// Mode is deliberately empty: with several roots there is no single answer, and asserting
		// a folder-wide one would be the guess the Ambiguous verdict exists to refuse.
	}

	return resolution
}

// readOnlyBases is every supported kustomization the scan holds that lies outside the write
// jail, relative to it and sorted.
//
// It is the complement of writableRenderRoots over the same store, which is what makes
// "non-empty exactly when the folder is an overlay" true by construction rather than by
// agreement between two rules.
func readOnlyBases(store *ManifestStore, writeScope string) []string {
	if writeScope == "" {
		return nil // nothing was scanned above the jail, so nothing can be outside it
	}
	var bases []string
	for dir, k := range store.Kustomizations {
		if k.Unsupported || pathWithin(slashDir(k.Path), writeScope) {
			continue
		}
		bases = append(bases, relativeFromScope(dir, writeScope))
	}
	sort.Strings(bases)
	return bases
}

// writableRenderRoots is the predicate resolveKustomizeRoot resolves against, lifted out so the
// reported layout and the taken layout cannot drift: LayoutResolved must describe the rung
// placement WILL take, and the only way to guarantee that is for both to ask one function.
//
// A root is a supported kustomization inside the write jail. Under render-root scoping the scan
// also holds the read-only bases the folder renders; those are not candidates, which is what
// lets an overlay resolve to its own single root instead of counting its base as a second one.
func writableRenderRoots(store *ManifestStore, writeScope string) []string {
	roots := make([]string, 0, len(store.Kustomizations))
	for dir, k := range store.Kustomizations {
		if k.Unsupported {
			continue
		}
		if writeScope != "" && !pathWithin(slashDir(k.Path), writeScope) {
			continue
		}
		roots = append(roots, dir)
	}
	sort.Strings(roots)
	return roots
}

// relativeToScope expresses a scanned-root-relative directory relative to the write jail, so a
// reported renderRoot is relative to spec.path exactly as placement paths are documented to be.
func relativeToScope(dir, writeScope string) string {
	if writeScope == "" {
		return orDot(dir)
	}
	if dir == writeScope {
		return "."
	}
	return orDot(trimScopePrefix(dir, writeScope))
}

func trimScopePrefix(dir, writeScope string) string {
	prefix := writeScope + "/"
	if len(dir) > len(prefix) && dir[:len(prefix)] == prefix {
		return dir[len(prefix):]
	}
	return dir
}

// relativeFromScope expresses a directory OUTSIDE the write jail relative to it, so a base the
// folder renders reads as "../../base" — the same way it is spelled in the overlay's own
// resources:, and the same way the write-boundary refusal names it. A base is always above the
// jail, never beside it, because the jail is what the scan was anchored past to reach it.
func relativeFromScope(dir, writeScope string) string {
	scopeParts := strings.Split(writeScope, "/")
	dirParts := strings.Split(dir, "/")
	common := 0
	for common < len(scopeParts) && common < len(dirParts) && scopeParts[common] == dirParts[common] {
		common++
	}
	up := strings.Repeat("../", len(scopeParts)-common)
	return orDot(up + strings.Join(dirParts[common:], "/"))
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// IssueAmbiguousLayout marks a GitTarget folder covering more than one render root. It is a
// property of the OBSERVED folder rather than of the spec, so no CEL rule and no admission
// check can reach it — the folder is only ambiguous once the operator has read it.
const IssueAmbiguousLayout IssueKind = "ambiguous-layout"

// AmbiguousLayoutRefusal is the write-path form of the Ambiguous verdict: a folder covering
// several render roots is refused rather than silently placed into whichever one an arbitrary
// rule picks.
//
// It refuses at the WRITE rather than at the GitTarget's Validated gate, and the difference is
// recoverability. Validated is evaluated before the data plane exists, so a target failing it
// never registers a worker, never scans, and could therefore never observe that the folder had
// been fixed — the refusal would be permanent, and for a target that had never scanned it could
// never fire in the first place. Refusing here keeps the target declared and scanning: the
// periodic resync re-reads the folder, so splitting the target down to a leaf overlay clears the
// refusal the same way fixing any other unsupported content does.
//
// It returns no issue for a folder that is not ambiguous, so the caller can raise it
// unconditionally.
func AmbiguousLayoutRefusal(resolution LayoutResolution, specPath string) []AcceptanceIssue {
	if resolution.Reason != LayoutAmbiguous {
		return nil
	}
	// Every path in a refusal is relative to the write jail, so the folder's own name is "." —
	// which reads as nothing at all in a message. Say what "." IS instead; the roots below are
	// relative to it and are what makes the message actionable.
	return []AcceptanceIssue{{
		Kind: IssueAmbiguousLayout,
		Path: orDot(specPath),
		Message: fmt.Sprintf(
			"the GitTarget path covers %d kustomize render roots (%s), so there is no single one "+
				"to place new documents into; point the GitTarget at one of them instead",
			len(resolution.RenderRoots), strings.Join(resolution.RenderRoots, ", ")),
		// The PLATFORM OPERATOR fixes it, because the remedy is a GitTarget edit rather than a
		// repository one: the folder is a perfectly good base-plus-overlays tree, and what is
		// wrong is the scope pointed at it. That is this actor's definition — the GitTarget's
		// scope and path — and misfiling it would send the one actionable instruction we have
		// ("point the GitTarget at one of them") to someone who does not own the object it
		// names. It is solvable and it is not a support boundary. See
		// test/fixtures/layout-corpus/shapes/README.md, "Why only a leaf can be a kustomize target".
		Solvable: true,
		Actor:    ActorPlatformOperator,
	}}
}
