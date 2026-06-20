/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"path"
	"strings"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
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
// against existing. The later-created target loses so the earlier owner keeps its
// folder. When both carry the same creationTimestamp (the API server stamps at
// second precision, so concurrent applies can tie) the loser is chosen
// deterministically by identity — otherwise neither would lose and both could go
// Ready over the same subtree. Both targets share a namespace here, so the
// namespace/name key is unique and stable across every reconcile.
func gitTargetLosesConflict(target, existing *configbutleraiv1alpha2.GitTarget) bool {
	switch {
	case target.CreationTimestamp.Time.After(existing.CreationTimestamp.Time):
		return true
	case target.CreationTimestamp.Time.Equal(existing.CreationTimestamp.Time):
		return gitTargetIdentityKey(target) > gitTargetIdentityKey(existing)
	default:
		return false
	}
}

// gitTargetIdentityKey returns a stable, unique ordering key for a GitTarget.
func gitTargetIdentityKey(t *configbutleraiv1alpha2.GitTarget) string {
	return t.Namespace + "/" + t.Name
}
