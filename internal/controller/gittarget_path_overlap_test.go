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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
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
	mk := func(name string, created time.Time) *configbutleraiv1alpha1.GitTarget {
		return &configbutleraiv1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         "default",
				Name:              name,
				CreationTimestamp: metav1.NewTime(created),
			},
		}
	}

	tests := []struct {
		name     string
		target   *configbutleraiv1alpha1.GitTarget
		existing *configbutleraiv1alpha1.GitTarget
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
