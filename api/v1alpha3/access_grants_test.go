// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// consumer is the object asking for access, by namespace and name.
func consumer(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func TestMatchBranchPattern(t *testing.T) {
	tests := []struct {
		pattern   BranchPattern
		namespace string
		name      string
		branch    string
		matches   bool
	}{
		{"teams/{consumer.namespace}/*", "team-a", "live", "teams/team-a/live", true},
		{"teams/{consumer.namespace}/*", "team-a", "live", "teams/team-ab/live", false},
		{"teams/{consumer.namespace}/*", "team-a", "live", "teams/team-a", false},
		{"teams/{consumer.namespace}/*", "team-a", "live", "teams/team-a/", true},
		{"feature/*", "x", "live", "feature/checkout/v2", true},
		{"feature/*", "x", "live", "feature", false},
		{"*", "x", "live", "main", true},
		{"*", "x", "live", "teams/a/b", true},
		{"teams/*", "x", "live", "teams/a/b", true},
		{"main", "x", "live", "main", true},
		{"main", "x", "live", "main-old", false},
		{"main", "x", "live", "old-main", false},
		{"release/*/hotfix", "x", "live", "release/1.2/hotfix", true},
		{"release/*/hotfix", "x", "live", "release/1.2/hotfix/2", false},
		{"*-old", "x", "live", "main-old", true},
		{"a*a", "x", "live", "a", false},
		{"a*a", "x", "live", "aa", true},
		{"{consumer.namespace}", "team-a", "live", "team-a", true},
		{"{consumer.namespace}/*", "team-a", "live", "team-b/x", false},
		{"teams/{consumer.namespace}/{consumer.name}", "team-a", "checkout", "teams/team-a/checkout", true},
		{"teams/{consumer.namespace}/{consumer.name}", "team-a", "checkout", "teams/team-a/payments", false},
		{"{consumer.namespace}/{consumer.name}/*", "team-a", "checkout", "team-a/checkout/feature", true},
		{"{consumer.namespace}/{consumer.name}/*", "team-a", "checkout", "team-a/payments/feature", false},
		{"{namespace}", "team-a", "live", "team-a", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.pattern)+"~"+tt.branch, func(t *testing.T) {
			assert.Equal(t, tt.matches, MatchBranchPattern(tt.pattern, consumer(tt.namespace, tt.name), tt.branch))
		})
	}
}

func TestConsumerNamespaces_Admits(t *testing.T) {
	tenant := map[string]string{"tenant": "true"}
	tests := []struct {
		name      string
		grant     ConsumerNamespaces
		namespace string
		labels    map[string]string
		admits    bool
	}{
		{"Same admits the holder's namespace", ConsumerNamespaces{From: GrantFromSame}, "platform", nil, true},
		{"Same admits nothing else", ConsumerNamespaces{From: GrantFromSame}, "team-a", tenant, false},
		{"Names admits a listed namespace",
			ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "team-a", nil, true},
		{"Names admits nothing else",
			ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "team-b", nil, false},
		// An exact allow-list, never a prefix: otherwise "team-a-evil" would inherit team-a's grant.
		{"Names is no prefix match",
			ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "team-a-evil", nil, false},
		{"Names is case-sensitive",
			ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "Team-A", nil, false},
		{"Selector admits a matching namespace", ConsumerNamespaces{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{MatchLabels: tenant}}, "team-a", tenant, true},
		{"Selector admits nothing else", ConsumerNamespaces{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{MatchLabels: tenant}}, "team-a", nil, false},
		{"an empty selector admits nobody", ConsumerNamespaces{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{}}, "team-a", tenant, false},
		{"All admits every namespace", ConsumerNamespaces{From: GrantFromAll}, "anything", nil, true},
		{"an unknown from admits nothing", ConsumerNamespaces{From: "Everyone"}, "platform", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admits, err := tt.grant.Admits("platform", tt.namespace, tt.labels)
			require.NoError(t, err)
			assert.Equal(t, tt.admits, admits)
		})
	}
}

// TestConsumerNamespaces_SameFollowsTheHolder: Same names no namespace, so a kustomize overlay that moves
// the object holding the grant moves what it admits with it.
func TestConsumerNamespaces_SameFollowsTheHolder(t *testing.T) {
	branch := &GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec: GitBranchSpec{
			AllowedGitTargets: []GitBranchGitTargetGrant{{ConsumerNamespaces: ConsumerNamespaces{From: GrantFromSame}}},
		},
	}
	admits, err := branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.True(t, admits)

	branch.Namespace = "team-a-staging" // the overlay's namespace:
	admits, err = branch.AdmitsGitTarget("team-a-staging", nil)
	require.NoError(t, err)
	assert.True(t, admits, "Same admits the namespace the object is in now")
	admits, err = branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.False(t, admits, "and nothing else")
}

func TestConsumerNamespaces_InvalidSelectorIsAnError(t *testing.T) {
	grant := ConsumerNamespaces{From: GrantFromSelector, Selector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tenant", Operator: "Bogus"}},
	}}
	_, err := grant.Admits("platform", "team-a", nil)
	require.Error(t, err)

	// One malformed grant refuses nobody another grant admits, and admits nobody itself.
	branch := &GitBranch{ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: GitBranchSpec{AllowedGitTargets: []GitBranchGitTargetGrant{
			{ConsumerNamespaces: grant}, {ConsumerNamespaces: ConsumerNamespaces{From: GrantFromSame}},
		}}}
	admits, err := branch.AdmitsGitTarget("platform", nil)
	require.NoError(t, err)
	assert.True(t, admits)
	admits, err = branch.AdmitsGitTarget("team-a", nil)
	require.Error(t, err)
	assert.False(t, admits)
}

func TestGitBranch_AdmitsGitTarget_DefaultsToSameNamespace(t *testing.T) {
	branch := &GitBranch{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	admits, err := branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.True(t, admits, "an omitted list is the default grant for the GitBranch's own namespace")
	admits, err = branch.AdmitsGitTarget("team-b", nil)
	require.NoError(t, err)
	assert.False(t, admits, "the default admits only the GitBranch's own namespace")
	assert.Equal(t, "admits team-a", branch.DescribeAllowedGitTargets())

	branch.Spec.AllowedGitTargets = []GitBranchGitTargetGrant{}
	admits, err = branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.False(t, admits, "[] admits nobody, the GitBranch's own namespace included")
	assert.Equal(t, "admits no namespace", branch.DescribeAllowedGitTargets())

	branch.Spec.AllowedGitTargets = []GitBranchGitTargetGrant{
		{ConsumerNamespaces: ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-b"}}},
	}
	admits, err = branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.False(t, admits, "a written list is the whole policy: the default is not added to it")
	assert.Equal(t, "admits team-b", branch.DescribeAllowedGitTargets())
}

func TestKubeProvider_DefaultsToSameNamespace(t *testing.T) {
	provider := &KubeProvider{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	scope, err := provider.AdmitsGitTarget(consumer("team-a", "apps"), nil)
	require.NoError(t, err)
	assert.True(t, scope.Admitted)
	assert.Equal(t, []string{"team-a"}, scope.SourceNamespaces,
		"an omitted list mirrors the namespace of the same name")
	assert.Empty(t, scope.ClusterResources, "and no cluster-scoped resource")
	scope, err = provider.AdmitsGitTarget(consumer("team-b", "apps"), nil)
	require.NoError(t, err)
	assert.False(t, scope.Admitted)

	claim, err := provider.AdmitsApplierClaim(consumer("team-a", "checkout"), nil)
	require.NoError(t, err)
	assert.Equal(t, KubeProviderApplierClaimScope{Admitted: true, ApplierNamespaces: []string{"team-a"}}, claim)

	provider.Spec.AllowedGitTargets = []KubeProviderGitTargetGrant{}
	provider.Spec.AllowedApplierClaims = []KubeProviderApplierClaimGrant{}
	scope, err = provider.AdmitsGitTarget(consumer("team-a", "apps"), nil)
	require.NoError(t, err)
	assert.False(t, scope.Admitted, "[] admits nobody")
	claim, err = provider.AdmitsApplierClaim(consumer("team-a", "checkout"), nil)
	require.NoError(t, err)
	assert.False(t, claim.Admitted, "[] admits nobody")
}

func TestGitProvider_AdmitsGitBranch(t *testing.T) {
	provider := &GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "shared-repo"},
		Spec: GitProviderSpec{AllowedGitBranches: []GitProviderGitBranchGrant{
			{
				ConsumerNamespaces: ConsumerNamespaces{From: GrantFromSame},
				BranchPatterns:     []BranchPattern{"main", "release/*"},
			},
			{
				ConsumerNamespaces: ConsumerNamespaces{
					From:     GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "true"}},
				},
				BranchPatterns: []BranchPattern{"teams/{consumer.namespace}/*", "feature/*"},
			},
		}},
	}
	tenant := map[string]string{"tenant": "true"}

	got, err := provider.AdmitsGitBranch(consumer("team-a", "live"), tenant, "teams/team-a/live")
	require.NoError(t, err)
	assert.Equal(t, GitBranchAdmission{Admitted: true, Allowed: true,
		BranchPatterns: []string{"feature/*", "teams/team-a/*"}}, got)

	got, err = provider.AdmitsGitBranch(consumer("team-a", "live"), tenant, "main")
	require.NoError(t, err)
	assert.True(t, got.Admitted)
	assert.False(t, got.Allowed, "main is the platform's")

	got, err = provider.AdmitsGitBranch(consumer("team-a", "live"), tenant, "teams/team-b/live")
	require.NoError(t, err)
	assert.False(t, got.Allowed, "another team's prefix")

	got, err = provider.AdmitsGitBranch(consumer("team-c", "live"), nil, "teams/team-c/live")
	require.NoError(t, err)
	assert.False(t, got.Admitted, "no grant admits an unlabelled namespace")

	got, err = provider.AdmitsGitBranch(consumer("platform", "live"), nil, "release/1.0")
	require.NoError(t, err)
	assert.True(t, got.Allowed)

	provider.Spec.AllowedGitBranches = nil
	got, err = provider.AdmitsGitBranch(consumer("platform", "live"), nil, "main")
	require.NoError(t, err)
	assert.False(t, got.Admitted, "allowedGitBranches has no default, the provider's own namespace included")
}

func TestKubeProvider_AdmitsGitTarget(t *testing.T) {
	tenant := map[string]string{"tenant": "true"}
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "production"},
		Spec: KubeProviderSpec{AllowedGitTargets: []KubeProviderGitTargetGrant{
			{
				ConsumerNamespaces: ConsumerNamespaces{
					From:     GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: tenant},
				},
				SourceNamespaces: []ScopeNamespace{"{consumer.namespace}"},
			},
			{
				ConsumerNamespaces: ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"platform-audit"}},
				SourceNamespaces:   []ScopeNamespace{"*"},
				ClusterResources:   []ClusterResourceIdentity{"*"},
			},
			{
				ConsumerNamespaces: ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"rbac-mirror"}},
				ClusterResources:   []ClusterResourceIdentity{"clusterroles.rbac.authorization.k8s.io", "nodes"},
			},
		}},
	}

	scope, err := provider.AdmitsGitTarget(consumer("team-a", "mirror"), tenant)
	require.NoError(t, err)
	assert.True(t, scope.Admitted)
	assert.True(t, scope.AllowsSourceNamespace("team-a"))
	assert.False(t, scope.AllowsSourceNamespace("kube-system"))
	assert.False(t, scope.AllowsSourceNamespace("*"), "{consumer.namespace} does not allow a wildcard")
	assert.False(t, scope.AllowsClusterResource("", "nodes"))

	scope, err = provider.AdmitsGitTarget(consumer("platform-audit", "mirror"), nil)
	require.NoError(t, err)
	assert.True(t, scope.AllowsSourceNamespace("*"))
	assert.True(t, scope.AllowsSourceNamespace("kube-system"))
	assert.True(t, scope.AllowsAllClusterResources())

	scope, err = provider.AdmitsGitTarget(consumer("rbac-mirror", "mirror"), nil)
	require.NoError(t, err)
	assert.True(t, scope.Admitted)
	assert.Empty(t, scope.SourceNamespaces, "clusterResources alone grants no namespace")
	assert.True(t, scope.AllowsClusterResource("rbac.authorization.k8s.io", "clusterroles"))
	assert.True(t, scope.AllowsClusterResource("", "nodes"))
	assert.False(t, scope.AllowsClusterResource("", "clusterroles"), "a bare name is the core group only")
	assert.False(t, scope.AllowsAllClusterResources())

	scope, err = provider.AdmitsGitTarget(consumer("platform", "mirror"), nil)
	require.NoError(t, err)
	assert.False(t, scope.Admitted, "the provider's own namespace needs a grant")
}

// TestKubeProvider_AdmitsGitTarget_UnionOfScopes: a namespace two grants admit gets both scopes.
func TestKubeProvider_AdmitsGitTarget_UnionOfScopes(t *testing.T) {
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: KubeProviderSpec{AllowedGitTargets: []KubeProviderGitTargetGrant{
			{
				ConsumerNamespaces: ConsumerNamespaces{From: GrantFromAll},
				SourceNamespaces:   []ScopeNamespace{"{consumer.namespace}"},
			},
			{
				ConsumerNamespaces: ConsumerNamespaces{
					From:     GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"audit": "true"}},
				},
				SourceNamespaces: []ScopeNamespace{"*"},
			},
		}},
	}
	scope, err := provider.AdmitsGitTarget(consumer("team-a", "mirror"), map[string]string{"audit": "true"})
	require.NoError(t, err)
	assert.Equal(t, []string{"*", "team-a"}, scope.SourceNamespaces)

	scope, err = provider.AdmitsGitTarget(consumer("team-a", "mirror"), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, scope.SourceNamespaces, "losing the label narrows the scope")
}

func TestKubeProvider_AdmitsApplierClaim(t *testing.T) {
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: KubeProviderSpec{
			AllowedGitTargets: []KubeProviderGitTargetGrant{
				{
					ConsumerNamespaces: ConsumerNamespaces{From: GrantFromAll},
					SourceNamespaces:   []ScopeNamespace{"{consumer.namespace}"},
				},
			},
			AllowedApplierClaims: []KubeProviderApplierClaimGrant{
				{
					ConsumerNamespaces: ConsumerNamespaces{From: GrantFromNames, Names: []GrantNamespace{"team-a"}},
					ApplierNamespaces:  []ScopeNamespace{"flux-system"},
				},
			},
		},
	}
	scope, err := provider.AdmitsApplierClaim(consumer("team-a", "checkout"), nil)
	require.NoError(t, err)
	assert.True(t, scope.AllowsApplierNamespace("flux-system"))
	assert.False(t, scope.AllowsApplierNamespace("team-a"), "an explicit list is the whole scope")

	scope, err = provider.AdmitsApplierClaim(consumer("team-b", "checkout"), nil)
	require.NoError(t, err)
	assert.False(t, scope.Admitted, "admission to mirror does not admit to apply")
	assert.Equal(t, "admits team-a", provider.DescribeAllowedApplierClaims())
}

func TestGitBranch_GitProviderKey(t *testing.T) {
	branch := &GitBranch{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	branch.Spec.GitProviderRef.Name = "shared-repo"
	assert.Equal(t, "team-a/shared-repo", branch.GitProviderKey().String(), "omitted is the own namespace")
	branch.Spec.GitProviderRef.Namespace = "platform"
	assert.Equal(t, "platform/shared-repo", branch.GitProviderKey().String())
}
