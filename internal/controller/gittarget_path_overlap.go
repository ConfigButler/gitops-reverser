// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"path"
	"strings"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// normalizeGitTargetPath canonicalizes a GitTarget spec.path into a clean,
// slash-rooted form so two paths can be compared segment-by-segment. Git paths
// are always slash-separated, so this uses path (not filepath, which is
// OS-specific). The repository root — an empty path, ".", or "/" — normalizes to
// "/". Leading/trailing slashes, "." segments, and redundant separators are
// removed (e.g. "a/b/", "/a/b", and "a/./b" all become "/a/b").
func normalizeGitTargetPath(p string) string {
	return path.Clean("/" + strings.TrimSpace(p))
}

// gitTargetPathIsAncestor reports whether ancestor strictly contains descendant
// in the folder tree (ancestor is a proper prefix on a segment boundary). The
// repository root "/" contains every other path. Equal paths are not ancestors
// of one another — callers test equality separately. Both arguments must already
// be normalized via normalizeGitTargetPath.
func gitTargetPathIsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return false
	}
	if ancestor == "/" {
		// Root contains everything except itself (handled above).
		return true
	}
	// Segment-boundary prefix: "/a" contains "/a/b" but not "/ab".
	return strings.HasPrefix(descendant, ancestor+"/")
}

// gitTargetPathsOverlap reports whether two GitTarget paths fight over the same
// folder subtree: they are equal, or one nests inside the other. Sibling folders
// (e.g. "/a" and "/b") do not overlap. This enforces the "GitTargets never
// overlap" invariant from the manifest materialization design — every
// materialized folder must have exactly one owner.
func gitTargetPathsOverlap(a, b string) bool {
	na := normalizeGitTargetPath(a)
	nb := normalizeGitTargetPath(b)
	if na == nb {
		return true
	}
	return gitTargetPathIsAncestor(na, nb) || gitTargetPathIsAncestor(nb, na)
}

// gitTargetLosesConflict reports whether target should lose an overlap conflict
// against existing, so that every materialized folder keeps exactly one owner.
//
// An ESTABLISHED claim beats a PENDING one. A target is established when its
// materialization already lives where its spec says (status.observedDestination
// agrees with spec); it is pending when it is asking to move somewhere, or has never
// materialized at all. This is the rule that survives spec.path becoming mutable: a
// target that retargets ONTO an occupied folder is the newcomer, whatever its age, and
// it must not evict the incumbent that is already writing there.
//
// Creation time only breaks a tie between two claims of the same strength — two fresh
// targets racing to claim a folder, as before. When both carry the same
// creationTimestamp (the API server stamps at second precision, so concurrent applies
// can tie) the loser is chosen deterministically by identity, otherwise neither would
// lose and both could go Ready over the same subtree. Both targets share a namespace
// here, so the namespace/name key is unique and stable across every reconcile.
func gitTargetLosesConflict(target, existing *configbutleraiv1alpha3.GitTarget) bool {
	targetEstablished := gitTargetIsEstablished(target)
	if targetEstablished != gitTargetIsEstablished(existing) {
		// Exactly one of them holds the folder. The one that does not, loses.
		return !targetEstablished
	}

	switch {
	case target.CreationTimestamp.Time.After(existing.CreationTimestamp.Time):
		return true
	case target.CreationTimestamp.Time.Equal(existing.CreationTimestamp.Time):
		return gitTargetIdentityKey(target) > gitTargetIdentityKey(existing)
	default:
		return false
	}
}

// gitTargetIsEstablished reports whether a GitTarget's materialization already lives at
// the destination its spec names. A target that has never materialized, or one that is
// retargeting, is not established: it is asking for a folder rather than holding one.
func gitTargetIsEstablished(target *configbutleraiv1alpha3.GitTarget) bool {
	observed := target.Status.ObservedDestination
	return observed != nil && sameDestination(*observed, specDestination(target))
}

// gitTargetIdentityKey returns a stable, unique ordering key for a GitTarget.
func gitTargetIdentityKey(t *configbutleraiv1alpha3.GitTarget) string {
	return t.Namespace + "/" + t.Name
}
