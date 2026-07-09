// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func TestNormalizeGitTargetPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty is root", "", "/"},
		{"dot is root", ".", "/"},
		{"slash is root", "/", "/"},
		{"whitespace is root", "   ", "/"},
		{"leading slash stripped to canonical", "/a/b", "/a/b"},
		{"trailing slash removed", "a/b/", "/a/b"},
		{"no leading slash", "a/b", "/a/b"},
		{"dot segment removed", "a/./b", "/a/b"},
		{"redundant separators collapsed", "a//b", "/a/b"},
		{"surrounding whitespace trimmed", "  a/b  ", "/a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeGitTargetPath(tt.in); got != tt.want {
				t.Errorf("normalizeGitTargetPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGitTargetPathIsAncestor(t *testing.T) {
	tests := []struct {
		name       string
		ancestor   string
		descendant string
		want       bool
	}{
		{"equal is not ancestor", "/a", "/a", false},
		{"root contains child", "/", "/a", true},
		{"root contains deep child", "/", "/a/b/c", true},
		{"root is not ancestor of root", "/", "/", false},
		{"direct parent", "/a", "/a/b", true},
		{"grandparent", "/a", "/a/b/c", true},
		{"sibling is not ancestor", "/a", "/b", false},
		{"prefix without segment boundary", "/a", "/ab", false},
		{"descendant is not ancestor of ancestor", "/a/b", "/a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitTargetPathIsAncestor(tt.ancestor, tt.descendant); got != tt.want {
				t.Errorf("gitTargetPathIsAncestor(%q, %q) = %v, want %v",
					tt.ancestor, tt.descendant, got, tt.want)
			}
		})
	}
}

func TestGitTargetPathsOverlap(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"identical", "a/b", "a/b", true},
		{"identical after normalization", "a/b/", "/a/b", true},
		{"both root overlap", "", ".", true},
		{"root overlaps any path", "", "team/app", true},
		{"any path overlaps root (reversed)", "team/app", "/", true},
		{"parent nests child", "team", "team/app", true},
		{"child nests parent (reversed)", "team/app", "team", true},
		{"deep nesting", "a", "a/b/c/d", true},
		{"siblings do not overlap", "a", "b", false},
		{"sibling subfolders do not overlap", "team/a", "team/b", false},
		{"prefix without boundary does not overlap", "team/app", "team/app-staging", false},
		{"disjoint trees", "infra/network", "apps/web", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitTargetPathsOverlap(tt.a, tt.b); got != tt.want {
				t.Errorf("gitTargetPathsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// Overlap is symmetric.
			if got := gitTargetPathsOverlap(tt.b, tt.a); got != tt.want {
				t.Errorf("gitTargetPathsOverlap(%q, %q) [reversed] = %v, want %v", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestGitTargetLosesConflict(t *testing.T) {
	base := time.Date(2026, time.June, 4, 12, 0, 0, 0, time.UTC)
	mk := func(name string, created time.Time) *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         "default",
				Name:              name,
				CreationTimestamp: metav1.NewTime(created),
			},
		}
	}

	tests := []struct {
		name     string
		target   *configbutleraiv1alpha3.GitTarget
		existing *configbutleraiv1alpha3.GitTarget
		want     bool
	}{
		{
			name:     "later target loses",
			target:   mk("b", base.Add(time.Second)),
			existing: mk("a", base),
			want:     true,
		},
		{
			name:     "earlier target wins",
			target:   mk("a", base),
			existing: mk("b", base.Add(time.Second)),
			want:     false,
		},
		{
			name:     "tie broken by identity: higher key loses",
			target:   mk("b", base),
			existing: mk("a", base),
			want:     true,
		},
		{
			name:     "tie broken by identity: lower key wins",
			target:   mk("a", base),
			existing: mk("b", base),
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitTargetLosesConflict(tt.target, tt.existing); got != tt.want {
				t.Errorf("gitTargetLosesConflict() = %v, want %v", got, tt.want)
			}
			// Exactly one of an overlapping pair must lose — never both, never
			// neither — when their identities differ.
			reverse := gitTargetLosesConflict(tt.existing, tt.target)
			if got := gitTargetLosesConflict(tt.target, tt.existing); got == reverse {
				t.Errorf("both/neither lose: target=%v existing=%v", got, reverse)
			}
		})
	}
}

// established builds a GitTarget whose materialization already lives where its spec says.
func established(name, path string, created time.Time) *configbutleraiv1alpha3.GitTarget {
	t := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "team-a", CreationTimestamp: metav1.NewTime(created),
		},
		Spec: configbutleraiv1alpha3.GitTargetSpec{Branch: "main", Path: path},
	}
	t.Status.ObservedDestination = &configbutleraiv1alpha3.GitTargetDestination{Branch: "main", Path: path}
	return t
}

// retargeting builds a GitTarget asking to move to path, still materialized at from.
func retargeting(name, from, to string, created time.Time) *configbutleraiv1alpha3.GitTarget {
	t := established(name, from, created)
	t.Spec.Path = to
	return t
}

// pending builds a GitTarget that has never materialized anywhere.
func pending(name, path string, created time.Time) *configbutleraiv1alpha3.GitTarget {
	t := established(name, path, created)
	t.Status.ObservedDestination = nil
	return t
}

// Once spec.path became mutable, creation time stopped being a faithful "who claimed this
// folder first". An OLDER target retargeting onto a YOUNGER incumbent's folder is still the
// newcomer, and must not evict the target that is already writing there.
func TestGitTargetLosesConflict_EstablishedBeatsRetargeting(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	younger := older.Add(time.Hour)

	mover := retargeting("mover", "apps", "clusters", older)
	incumbent := established("incumbent", "clusters", younger)

	if !gitTargetLosesConflict(mover, incumbent) {
		t.Error("the older target retargeting onto an occupied folder must lose: it is the newcomer")
	}
	if gitTargetLosesConflict(incumbent, mover) {
		t.Error("the incumbent, which never moved, must keep its folder")
	}
}

// A target that has never materialized is also a pending claim.
func TestGitTargetLosesConflict_EstablishedBeatsNeverMaterialized(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	younger := older.Add(time.Hour)

	fresh := pending("fresh", "clusters", older)
	incumbent := established("incumbent", "clusters", younger)

	if !gitTargetLosesConflict(fresh, incumbent) {
		t.Error("a target that has never materialized must not evict one that has")
	}
}

// Two claims of the same strength still fall back to creation time, as before.
func TestGitTargetLosesConflict_EqualStrengthFallsBackToCreationTime(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	younger := older.Add(time.Hour)

	if !gitTargetLosesConflict(pending("b", "apps", younger), pending("a", "apps", older)) {
		t.Error("two fresh targets: the later-created one loses, as before")
	}
	if !gitTargetLosesConflict(established("b", "apps", younger), established("a", "apps", older)) {
		t.Error("two established targets: the later-created one loses")
	}
	// Both retargeting onto the same folder: neither holds it, so creation time decides.
	if !gitTargetLosesConflict(
		retargeting("b", "x", "apps", younger), retargeting("a", "y", "apps", older),
	) {
		t.Error("two movers: the later-created one loses")
	}
}

func TestGitTargetIsEstablished(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !gitTargetIsEstablished(established("a", "apps", created)) {
		t.Error("spec and observedDestination agree: established")
	}
	if gitTargetIsEstablished(retargeting("a", "apps", "clusters", created)) {
		t.Error("a target asking to move is not established at its new destination")
	}
	if gitTargetIsEstablished(pending("a", "apps", created)) {
		t.Error("a target that never materialized is not established")
	}
	// A trailing slash is the same folder, so it is still established.
	settled := established("a", "apps", created)
	settled.Spec.Path = "apps/"
	if !gitTargetIsEstablished(settled) {
		t.Error("a trailing slash must not make a settled target look like a mover")
	}
}
