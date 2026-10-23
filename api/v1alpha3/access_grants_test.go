// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMatchBranchPattern(t *testing.T) {
	tests := []struct {
		pattern   BranchPattern
		namespace string
		branch    string
		matches   bool
	}{
		{"teams/{namespace}/*", "team-a", "teams/team-a/live", true},
		{"teams/{namespace}/*", "team-a", "teams/team-ab/live", false},
		{"teams/{namespace}/*", "team-a", "teams/team-a", false},
		{"teams/{namespace}/*", "team-a", "teams/team-a/", true},
		{"feature/*", "x", "feature/checkout/v2", true},
		{"feature/*", "x", "feature", false},
		{"*", "x", "main", true},
		{"*", "x", "teams/a/b", true},
		{"teams/*", "x", "teams/a/b", true},
		{"main", "x", "main", true},
		{"main", "x", "main-old", false},
		{"main", "x", "old-main", false},
		{"release/*/hotfix", "x", "release/1.2/hotfix", true},
		{"release/*/hotfix", "x", "release/1.2/hotfix/2", false},
		{"*-old", "x", "main-old", true},
		{"a*a", "x", "a", false},
		{"a*a", "x", "aa", true},
		{"{namespace}", "team-a", "team-a", true},
		{"{namespace}/*", "team-a", "team-b/x", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.pattern)+"~"+tt.branch, func(t *testing.T) {
			assert.Equal(t, tt.matches, MatchBranchPattern(tt.pattern, tt.namespace, tt.branch))
		})
	}
}

func TestNamespaceGrant_Admits(t *testing.T) {
	tenant := map[string]string{"tenant": "true"}
	tests := []struct {
		name      string
		grant     NamespaceGrant
		namespace string
		labels    map[string]string
		admits    bool
	}{
		{"Same admits the holder's namespace", NamespaceGrant{From: GrantFromSame}, "platform", nil, true},
		{"Same admits nothing else", NamespaceGrant{From: GrantFromSame}, "team-a", tenant, false},
		{"Names admits a listed namespace",
			NamespaceGrant{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "team-a", nil, true},
		{"Names admits nothing else",
			NamespaceGrant{From: GrantFromNames, Names: []GrantNamespace{"team-a"}}, "team-b", nil, false},
		{"Selector admits a matching namespace", NamespaceGrant{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{MatchLabels: tenant}}, "team-a", tenant, true},
		{"Selector admits nothing else", NamespaceGrant{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{MatchLabels: tenant}}, "team-a", nil, false},
		{"an empty selector admits nobody", NamespaceGrant{From: GrantFromSelector,
			Selector: &metav1.LabelSelector{}}, "team-a", tenant, false},
		{"All admits every namespace", NamespaceGrant{From: GrantFromAll}, "anything", nil, true},
		{"an unknown from admits nothing", NamespaceGrant{From: "Everyone"}, "platform", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admits, err := tt.grant.Admits("platform", tt.namespace, tt.labels)
			require.NoError(t, err)
			assert.Equal(t, tt.admits, admits)
		})
	}
}

// TestNamespaceGrant_SameFollowsTheHolder: Same names no namespace, so a kustomize overlay that moves
// the object holding the grant moves what it admits with it.
func TestNamespaceGrant_SameFollowsTheHolder(t *testing.T) {
	branch := &GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       GitBranchSpec{AllowedGitTargets: []NamespaceGrant{{From: GrantFromSame}}},
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

func TestNamespaceGrant_InvalidSelectorIsAnError(t *testing.T) {
	grant := NamespaceGrant{From: GrantFromSelector, Selector: &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tenant", Operator: "Bogus"}},
	}}
	_, err := grant.Admits("platform", "team-a", nil)
	require.Error(t, err)

	// One malformed grant refuses nobody another grant admits, and admits nobody itself.
	branch := &GitBranch{ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: GitBranchSpec{AllowedGitTargets: []NamespaceGrant{grant, {From: GrantFromSame}}}}
	admits, err := branch.AdmitsGitTarget("platform", nil)
	require.NoError(t, err)
	assert.True(t, admits)
	admits, err = branch.AdmitsGitTarget("team-a", nil)
	require.Error(t, err)
	assert.False(t, admits)
}

func TestGitBranch_AdmitsGitTarget_NothingAutomatic(t *testing.T) {
	branch := &GitBranch{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	admits, err := branch.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.False(t, admits, "an omitted list admits nobody, the GitBranch's own namespace included")
	assert.Equal(t, "admits no namespace", branch.DescribeAllowedGitTargets())

	branch.Spec.AllowedGitTargets = []NamespaceGrant{{From: GrantFromSame},
		{From: GrantFromNames, Names: []GrantNamespace{"team-b"}}}
	assert.Equal(t, "admits team-a; team-b", branch.DescribeAllowedGitTargets())
}

func TestGitProvider_AdmitsGitBranch(t *testing.T) {
	provider := &GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "shared-repo"},
		Spec: GitProviderSpec{AllowedGitBranches: []GitBranchGrant{
			{From: GrantFromSame, Branches: []BranchPattern{"main", "release/*"}},
			{From: GrantFromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "true"}},
				Branches: []BranchPattern{"teams/{namespace}/*", "feature/*"}},
		}},
	}
	tenant := map[string]string{"tenant": "true"}

	got, err := provider.AdmitsGitBranch("team-a", tenant, "teams/team-a/live")
	require.NoError(t, err)
	assert.Equal(t, GitBranchAdmission{Admitted: true, Allowed: true,
		Branches: []string{"feature/*", "teams/team-a/*"}}, got)

	got, err = provider.AdmitsGitBranch("team-a", tenant, "main")
	require.NoError(t, err)
	assert.True(t, got.Admitted)
	assert.False(t, got.Allowed, "main is the platform's")

	got, err = provider.AdmitsGitBranch("team-a", tenant, "teams/team-b/live")
	require.NoError(t, err)
	assert.False(t, got.Allowed, "another team's prefix")

	got, err = provider.AdmitsGitBranch("team-c", nil, "teams/team-c/live")
	require.NoError(t, err)
	assert.False(t, got.Admitted, "no grant admits an unlabelled namespace")

	got, err = provider.AdmitsGitBranch("platform", nil, "release/1.0")
	require.NoError(t, err)
	assert.True(t, got.Allowed)

	provider.Spec.AllowedGitBranches = nil
	got, err = provider.AdmitsGitBranch("platform", nil, "main")
	require.NoError(t, err)
	assert.False(t, got.Admitted, "nothing is automatic, the provider's own namespace included")
}

func TestKubeProvider_AdmitsGitTarget(t *testing.T) {
	tenant := map[string]string{"tenant": "true"}
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "production"},
		Spec: KubeProviderSpec{AllowedGitTargets: []SourceGrant{
			{From: GrantFromSelector, Selector: &metav1.LabelSelector{MatchLabels: tenant},
				SourceNamespaces: []ScopeNamespace{"{namespace}"}},
			{From: GrantFromNames, Names: []GrantNamespace{"platform-audit"},
				SourceNamespaces: []ScopeNamespace{"*"}, ClusterResources: []ClusterResourceIdentity{"*"}},
			{From: GrantFromNames, Names: []GrantNamespace{"rbac-mirror"},
				ClusterResources: []ClusterResourceIdentity{"clusterroles.rbac.authorization.k8s.io", "nodes"}},
		}},
	}

	scope, err := provider.AdmitsGitTarget("team-a", tenant)
	require.NoError(t, err)
	assert.True(t, scope.Admitted)
	assert.True(t, scope.AllowsSourceNamespace("team-a"))
	assert.False(t, scope.AllowsSourceNamespace("kube-system"))
	assert.False(t, scope.AllowsSourceNamespace("*"), "{namespace} does not allow a wildcard")
	assert.False(t, scope.AllowsClusterResource("", "nodes"))

	scope, err = provider.AdmitsGitTarget("platform-audit", nil)
	require.NoError(t, err)
	assert.True(t, scope.AllowsSourceNamespace("*"))
	assert.True(t, scope.AllowsSourceNamespace("kube-system"))
	assert.True(t, scope.AllowsAllClusterResources())

	scope, err = provider.AdmitsGitTarget("rbac-mirror", nil)
	require.NoError(t, err)
	assert.True(t, scope.Admitted)
	assert.Empty(t, scope.SourceNamespaces, "clusterResources alone grants no namespace")
	assert.True(t, scope.AllowsClusterResource("rbac.authorization.k8s.io", "clusterroles"))
	assert.True(t, scope.AllowsClusterResource("", "nodes"))
	assert.False(t, scope.AllowsClusterResource("", "clusterroles"), "a bare name is the core group only")
	assert.False(t, scope.AllowsAllClusterResources())

	scope, err = provider.AdmitsGitTarget("platform", nil)
	require.NoError(t, err)
	assert.False(t, scope.Admitted, "the provider's own namespace needs a grant")
}

// TestKubeProvider_AdmitsGitTarget_UnionOfScopes: a namespace two grants admit gets both scopes.
func TestKubeProvider_AdmitsGitTarget_UnionOfScopes(t *testing.T) {
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: KubeProviderSpec{AllowedGitTargets: []SourceGrant{
			{From: GrantFromAll, SourceNamespaces: []ScopeNamespace{"{namespace}"}},
			{From: GrantFromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"audit": "true"}},
				SourceNamespaces: []ScopeNamespace{"*"}},
		}},
	}
	scope, err := provider.AdmitsGitTarget("team-a", map[string]string{"audit": "true"})
	require.NoError(t, err)
	assert.Equal(t, []string{"*", "team-a"}, scope.SourceNamespaces)

	scope, err = provider.AdmitsGitTarget("team-a", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, scope.SourceNamespaces, "losing the label narrows the scope")
}

func TestKubeProvider_AdmitsApplierClaim(t *testing.T) {
	provider := &KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform"},
		Spec: KubeProviderSpec{
			AllowedGitTargets: []SourceGrant{{From: GrantFromAll, SourceNamespaces: []ScopeNamespace{"{namespace}"}}},
			AllowedApplierClaims: []ApplierGrant{
				{
					From:              GrantFromNames,
					Names:             []GrantNamespace{"team-a"},
					ApplierNamespaces: []ScopeNamespace{"flux-system"},
				},
			},
		},
	}
	scope, err := provider.AdmitsApplierClaim("team-a", nil)
	require.NoError(t, err)
	assert.True(t, scope.AllowsApplierNamespace("flux-system"))
	assert.False(t, scope.AllowsApplierNamespace("team-a"), "an explicit list is the whole scope")

	scope, err = provider.AdmitsApplierClaim("team-b", nil)
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
