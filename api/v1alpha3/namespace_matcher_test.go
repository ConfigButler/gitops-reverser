// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestNamespaceMatcher_DenyByDefault pins the semantics accessFrom depends on. The fail-open
// reading is the catastrophic one, so the nil and empty cases get their own assertions rather than
// riding along with a general case.
func TestNamespaceMatcher_DenyByDefault(t *testing.T) {
	var nilMatcher *NamespaceMatcher

	allowed, err := nilMatcher.Matches("anything", nil)
	require.NoError(t, err)
	assert.False(t, allowed, "a nil matcher admits nothing")

	empty := &NamespaceMatcher{}
	allowed, err = empty.Matches("anything", map[string]string{"a": "b"})
	require.NoError(t, err)
	assert.False(t, allowed, "an EMPTY declared policy admits nothing: empty is not unrestricted")
}

// TestNamespaceMatcher_NamesAndSelectorAreOred covers the OR contract and that the NAME half never
// consults labels, so a listed namespace is admitted whatever it is labelled.
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

// A present-but-EMPTY selector admits EVERY namespace, while a nil one admits none.
// LabelSelectorAsSelector returns labels.Everything() for the first and labels.Nothing() for the
// second, and the chart's own default `default` ClusterProvider ships `accessFrom: {selector: {}}`,
// so if this ever flipped every install would stop admitting anything.
func TestNamespaceMatcher_EmptySelectorMatchesEverything(t *testing.T) {
	declared := &NamespaceMatcher{Selector: &metav1.LabelSelector{}}
	admits, err := declared.Matches("any", nil)
	require.NoError(t, err)
	assert.True(t, admits, "a present-but-empty selector admits EVERY namespace, labels or not")

	admits, err = declared.Matches("any", map[string]string{"anything": "at-all"})
	require.NoError(t, err)
	assert.True(t, admits)

	absent := &NamespaceMatcher{Names: []string{"repo-config"}}
	admits, err = absent.Matches("other", map[string]string{"anything": "at-all"})
	require.NoError(t, err)
	assert.False(t, admits, "a nil selector admits nothing beyond the listed names")
}

// TestResourceRule_EffectiveSourceNamespace pins the per-item defaulting every consumer keys on.
// Getting this wrong produces a stale watch, not a visible failure.
func TestResourceRule_EffectiveSourceNamespace(t *testing.T) {
	const own = "tenant-acme"
	item := &ResourceRule{Resources: []string{"configmaps"}}

	assert.Equal(t, own, item.EffectiveSourceNamespace(own), "omitted means the rule's own")
	assert.False(t, item.OverridesSourceNamespace(own))
	assert.False(t, item.IsSourceNamespaceWildcard())

	item.SourceNamespace = "repo-config"
	assert.Equal(t, "repo-config", item.EffectiveSourceNamespace(own))
	assert.True(t, item.OverridesSourceNamespace(own))
	assert.False(t, item.IsSourceNamespaceWildcard())

	// Restating the rule's own namespace is NOT an override: it needs no delegation flag.
	item.SourceNamespace = own
	assert.Equal(t, own, item.EffectiveSourceNamespace(own))
	assert.False(t, item.OverridesSourceNamespace(own),
		"naming your own namespace explicitly must behave exactly like omitting it")

	// "*" is ALWAYS an override: it reaches every namespace the source credential can read, which
	// is the widest request in the API and the last one that should slip past the delegation flag.
	item.SourceNamespace = SourceNamespaceWildcard
	assert.True(t, item.IsSourceNamespaceWildcard())
	assert.True(t, item.OverridesSourceNamespace(own))
}

// TestClusterWatchRuleSpec_DeclaresNamespacedScope covers the other half of decision 10. The
// refusal keys on the STORED value, never on what the selector happens to resolve.
func TestClusterWatchRuleSpec_DeclaresNamespacedScope(t *testing.T) {
	clusterOnly := ClusterWatchRuleSpec{Rules: []ClusterResourceRule{
		{Resources: []string{"customresourcedefinitions"}, Scope: ResourceScopeCluster},
		{Resources: []string{"*"}},
	}}
	assert.False(t, clusterOnly.DeclaresNamespacedScope(),
		"an omitted scope defaults to Cluster and a wildcard selector is not itself a refusal")

	stored := ClusterWatchRuleSpec{Rules: []ClusterResourceRule{
		{Resources: []string{"nodes"}, Scope: ResourceScopeCluster},
		{Resources: []string{"configmaps"}, Scope: ResourceScopeNamespaced},
	}}
	assert.True(t, stored.DeclaresNamespacedScope(), "one namespaced item refuses the whole rule")
}

// The security default in one line: the delegation flag must be false on a provider that never
// mentions it, so a WatchRule may watch only its own namespace until a platform admin says
// otherwise.
func TestClusterProvider_DelegationFlagDefaultsClosed(t *testing.T) {
	provider := &ClusterProvider{}
	assert.False(t, provider.AllowsAnySourceNamespace(),
		"source-namespace delegation must never be on by default")
}
