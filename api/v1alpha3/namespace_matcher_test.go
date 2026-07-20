// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestNamespaceMatcher_DenyByDefault pins the semantics both policies depend on. The fail-open
// reading is the catastrophic one, so the nil and empty cases get their own assertions rather than
// riding along with a general case.
func TestNamespaceMatcher_DenyByDefault(t *testing.T) {
	var nilMatcher *NamespaceMatcher

	allowed, err := nilMatcher.Matches("anything", nil)
	require.NoError(t, err)
	assert.False(t, allowed, "a nil matcher admits nothing")
	assert.False(t, nilMatcher.Declared(), "a nil matcher declares no policy")

	empty := &NamespaceMatcher{}
	allowed, err = empty.Matches("anything", map[string]string{"a": "b"})
	require.NoError(t, err)
	assert.False(t, allowed, "an EMPTY declared policy admits nothing — empty is not unrestricted")
	assert.True(t, empty.Declared(), "but it IS declared, which is what makes it exhaustive")
}

// TestNamespaceMatcher_NamesAndSelectorAreOred covers the OR contract and, more importantly, that
// the NAME half never consults labels — the property that keeps exact-name policies working
// against a cluster whose Namespace reads are denied.
func TestNamespaceMatcher_NamesAndSelectorAreOred(t *testing.T) {
	matcher := &NamespaceMatcher{
		Names:    []string{"repo-config"},
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"mirrorable": "true"}},
	}

	tests := []struct {
		name    string
		nsName  string
		labels  map[string]string
		allowed bool
	}{
		{"listed by name, no labels at all", "repo-config", nil, true},
		{"matched by selector", "other", map[string]string{"mirrorable": "true"}, true},
		{"neither", "other", map[string]string{"mirrorable": "false"}, false},
		{"listed by name despite non-matching labels", "repo-config",
			map[string]string{"mirrorable": "false"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, err := matcher.Matches(tt.nsName, tt.labels)
			require.NoError(t, err)
			assert.Equal(t, tt.allowed, allowed)
		})
	}

	assert.True(t, matcher.MatchesName("repo-config"))
	assert.False(t, matcher.MatchesName("other"))
	assert.True(t, matcher.HasSelector())
}

// TestNamespaceMatcher_InvalidSelectorIsAnError: a malformed selector must surface, not silently
// allow or silently deny. Both silent outcomes are configuration bugs an operator never sees.
func TestNamespaceMatcher_InvalidSelectorIsAnError(t *testing.T) {
	matcher := &NamespaceMatcher{
		Selector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "x", Operator: "NotARealOperator", Values: []string{"y"},
			}},
		},
	}

	_, err := matcher.Matches("any", map[string]string{"x": "y"})

	require.Error(t, err)
}

// TestWatchRule_EffectiveSourceNamespace pins the defaulting every consumer keys on. Getting this
// wrong produces a stale watch, not a visible failure.
func TestWatchRule_EffectiveSourceNamespace(t *testing.T) {
	rule := &WatchRule{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "tenant-acme"}}

	assert.Equal(t, "tenant-acme", rule.EffectiveSourceNamespace(), "omitted means the rule's own")
	assert.False(t, rule.OverridesSourceNamespace())

	rule.Spec.SourceNamespace = "repo-config"
	assert.Equal(t, "repo-config", rule.EffectiveSourceNamespace())
	assert.True(t, rule.OverridesSourceNamespace())

	// Restating the rule's own namespace is NOT an override: it needs no delegation flag.
	rule.Spec.SourceNamespace = "tenant-acme"
	assert.Equal(t, "tenant-acme", rule.EffectiveSourceNamespace())
	assert.False(t, rule.OverridesSourceNamespace(),
		"naming your own namespace explicitly must behave exactly like omitting it")
}

// TestGitTarget_SourceNamespacePolicy checks the two thin wrappers stay thin: a declared policy is
// distinguishable from an absent one, and the source-side predicate matches the shared shape.
func TestGitTarget_SourceNamespacePolicy(t *testing.T) {
	target := &GitTarget{}
	assert.False(t, target.DeclaresSourceNamespacePolicy())

	allowed, err := target.AllowsSourceNamespace("repo-config", nil)
	require.NoError(t, err)
	assert.False(t, allowed, "an undeclared policy admits nothing; the legacy rule is the caller's")

	target.Spec.AllowedSourceNamespaces = &NamespaceMatcher{Names: []string{"repo-config"}}
	assert.True(t, target.DeclaresSourceNamespacePolicy())

	allowed, err = target.AllowsSourceNamespace("repo-config", nil)
	require.NoError(t, err)
	assert.True(t, allowed)
}

// TestClusterProvider_DelegationFlagDefaultsClosed is the security default in one line: the flag
// must be false on a provider that never mentions it.
func TestClusterProvider_DelegationFlagDefaultsClosed(t *testing.T) {
	provider := &ClusterProvider{}
	assert.False(t, provider.AllowsWatchRuleSourceNamespaceOverride(),
		"source-namespace override must never be on by default")
}
