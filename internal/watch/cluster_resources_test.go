// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// granting is the boot KubeProvider admitting the boot GitTarget's namespace with clusterResources
// identities; none at all gives it its own namespace and no cluster-scoped resource.
func granting(identities ...configv1alpha3.ClusterResourceIdentity) *configv1alpha3.KubeProvider {
	grant := configv1alpha3.KubeProviderGitTargetGrant{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
			From:  configv1alpha3.GrantFromNames,
			Names: []configv1alpha3.GrantNamespace{bootTargetNS},
		},
		ClusterResources: identities,
	}
	if len(identities) == 0 {
		grant.SourceNamespaces = []configv1alpha3.ScopeNamespace{configv1alpha3.ConsumerNamespaceVariable}
	}
	return bootKubeProvider([]configv1alpha3.KubeProviderGitTargetGrant{grant})
}

// clusterRuleFor is a ClusterWatchRule on the boot GitTarget with items.
func clusterRuleFor(name string, items ...configv1alpha3.ClusterResourceRule) configv1alpha3.ClusterWatchRule {
	return configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: bootTargetName, Namespace: bootTargetNS},
			Rules:        items,
		},
	}
}

func item(groups []string, resources ...string) configv1alpha3.ClusterResourceRule {
	return configv1alpha3.ClusterResourceRule{APIGroups: groups, Resources: resources}
}

var (
	rbacGroup = []string{"rbac.authorization.k8s.io"}
	coreGroup = []string{""}
)

// TestCompileClusterWatchRule_ClusterResources is decided on the rule's text: named pairs are checked
// pair by pair, a wildcard needs "*", and one forbidden type refuses the whole rule.
func TestCompileClusterWatchRule_ClusterResources(t *testing.T) {
	clusterRoles := item(rbacGroup, "clusterroles")
	tests := []struct {
		name     string
		provider *configv1alpha3.KubeProvider
		items    []configv1alpha3.ClusterResourceRule
		refused  string
	}{
		{"no clusterResources refuses clusterroles", granting(), []configv1alpha3.ClusterResourceRule{clusterRoles},
			`asks for "clusterroles.rbac.authorization.k8s.io"`},
		{"another identity refuses clusterroles",
			granting("customresourcedefinitions.apiextensions.k8s.io"),
			[]configv1alpha3.ClusterResourceRule{clusterRoles}, `"clusterroles.rbac.authorization.k8s.io"`},
		{"a bare name is the core group only", granting("clusterroles"),
			[]configv1alpha3.ClusterResourceRule{clusterRoles}, `"clusterroles.rbac.authorization.k8s.io"`},
		{"its identity compiles clusterroles", granting("clusterroles.rbac.authorization.k8s.io"),
			[]configv1alpha3.ClusterResourceRule{clusterRoles}, ""},
		{"* compiles clusterroles", granting("*"), []configv1alpha3.ClusterResourceRule{clusterRoles}, ""},
		{"a bare name compiles a core resource", granting("nodes"),
			[]configv1alpha3.ClusterResourceRule{item(coreGroup, "nodes")}, ""},
		{"resources: * needs *", granting("clusterroles.rbac.authorization.k8s.io"),
			[]configv1alpha3.ClusterResourceRule{item(rbacGroup, "*")}, `needs clusterResources: ["*"]`},
		{"apiGroups: * needs *", granting("clusterroles.rbac.authorization.k8s.io"),
			[]configv1alpha3.ClusterResourceRule{item([]string{"*"}, "clusterroles")}, `needs clusterResources: ["*"]`},
		{"a wildcard compiles under *", granting("*"),
			[]configv1alpha3.ClusterResourceRule{item([]string{"*"}, "*")}, ""},
		{"one forbidden type refuses the whole rule, and is named",
			granting("clusterroles.rbac.authorization.k8s.io"),
			[]configv1alpha3.ClusterResourceRule{clusterRoles, item(coreGroup, "nodes", "namespaces")},
			`spec.rules[1] asks for "nodes", "namespaces"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := bootManager(t, bootGitTarget(), tt.provider)
			rule := clusterRuleFor("rule", tt.items...)
			decision, err := CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, rule,
				*bootGitTarget(), nil)
			require.NoError(t, err)
			if tt.refused == "" {
				assert.True(t, decision.Admitted, decision.Message)
				assert.Equal(t, []string{"rule"}, bootCompiledNames(m))
				return
			}
			assert.False(t, decision.Admitted)
			assert.True(t, decision.ResourcesRefused)
			assert.Equal(t, ClusterWatchRuleReasonClusterResourceNotAllowed, decision.Reason)
			assert.Contains(t, decision.Message, tt.refused)
			assert.Contains(t, decision.Message, "KubeProvider platform/prod-eu-1 lets namespace \"team-a\" mirror "+
				"cluster-scoped resources")
			assert.Empty(t, bootCompiledNames(m), "a refused rule is never compiled")
		})
	}
}

// TestCompileClusterWatchRule_AnOmittedGroupIsCheckedByItsResolvedPair: with apiGroups omitted, the
// item is checked by the types the planner selects for it.
func TestCompileClusterWatchRule_AnOmittedGroupIsCheckedByItsResolvedPair(t *testing.T) {
	m := bootManager(t, bootGitTarget(), granting("nodes"))
	rule := clusterRuleFor("rule", item(nil, "clusterroles"))
	rbac := func(configv1alpha3.ClusterResourceRule) []schema.GroupResource {
		return []schema.GroupResource{{Group: rbacGroup[0], Resource: "clusterroles"}}
	}

	decision, err := CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, rule, *bootGitTarget(), rbac)
	require.NoError(t, err)
	assert.Equal(t, ClusterWatchRuleReasonClusterResourceNotAllowed, decision.Reason)
	assert.Contains(t, decision.Message, `"clusterroles.rbac.authorization.k8s.io"`)

	// Selecting nothing today, it compiles; the planner keeps it to the grant.
	decision, err = CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, rule, *bootGitTarget(), nil)
	require.NoError(t, err)
	assert.True(t, decision.Admitted)
	compiled := m.RuleStore.SnapshotClusterWatchRules()
	require.Len(t, compiled, 1)
	assert.Equal(t, []string{"nodes"}, compiled[0].ClusterResources)
	assert.False(t, compiled[0].AllowsType("rbac.authorization.k8s.io", "clusterroles"))
}

// TestCompileClusterWatchRule_RemovingAnIdentityRefusesOnlyTheRulesNamingIt: the GitTarget stays
// admitted, and its other rules keep compiling.
func TestCompileClusterWatchRule_RemovingAnIdentityRefusesOnlyTheRulesNamingIt(t *testing.T) {
	m := bootManager(t, bootGitTarget(), granting("nodes", "clusterroles.rbac.authorization.k8s.io"))
	nodes := clusterRuleFor("nodes", item(coreGroup, "nodes"))
	roles := clusterRuleFor("roles", item(rbacGroup, "clusterroles"))
	for _, rule := range []configv1alpha3.ClusterWatchRule{nodes, roles} {
		decision, err := CompileClusterWatchRule(
			context.Background(),
			m.Client,
			m.RuleStore,
			rule,
			*bootGitTarget(),
			nil,
		)
		require.NoError(t, err)
		require.True(t, decision.Admitted, decision.Message)
	}

	stored := &configv1alpha3.KubeProvider{}
	require.NoError(t, m.Client.Get(context.Background(), granting().Key(), stored))
	stored.Spec.AllowedGitTargets[0].ClusterResources = []configv1alpha3.ClusterResourceIdentity{"nodes"}
	require.NoError(t, m.Client.Update(context.Background(), stored))

	decision, err := CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, roles, *bootGitTarget(), nil)
	require.NoError(t, err)
	assert.False(t, decision.Admitted)
	decision, err = CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, nodes, *bootGitTarget(), nil)
	require.NoError(t, err)
	assert.True(t, decision.Admitted)
	assert.Equal(t, []string{"nodes"}, bootCompiledNames(m))
	admitted, err := authz.GitTargetAdmitted(context.Background(), m.Client, bootGitTarget())
	require.NoError(t, err)
	assert.True(t, admitted.Allowed, "the GitTarget stays admitted for its other rules")
}

// TestRefreshWatchedTypeTables_DiscoveryNeverWidensAGrant: a compiled rule's matches outside the
// clusterResources it compiled under are never planned, so a type discovery serves later stays out.
func TestRefreshWatchedTypeTables_DiscoveryNeverWidensAGrant(t *testing.T) {
	for _, tc := range []struct {
		granted []string
		types   int
	}{{[]string{"*"}, 1}, {[]string{"namespaces"}, 1}, {[]string{"nodes"}, 0}, {nil, 0}} {
		manager, store := makeWatchedTypeManager(t)
		store.AddOrUpdateClusterWatchRule(clusterRuleForResource("rule-1", "namespaces"), tc.granted,
			"test-target", "test-ns", "test-provider", "test-ns", "test-path")
		manager.refreshWatchedTypeTables()
		table, _ := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
		assert.Len(t, table.Types, tc.types, "granted %v", tc.granted)
	}
}

// TestRefreshWatchedTypeTables_ANarrowerGrantRebuildsThePlan: the granted clusterResources are part of
// the plan's fingerprint, so recompiling a rule under a narrower grant drops what it no longer covers.
func TestRefreshWatchedTypeTables_ANarrowerGrantRebuildsThePlan(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	rule := clusterRuleForResource("rule-1", "namespaces")
	store.AddOrUpdateClusterWatchRule(rule, []string{"*"}, "test-target", "test-ns", "test-provider", "test-ns",
		"test-path")
	manager.refreshWatchedTypeTables()
	table, _ := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.Len(t, table.Types, 1)

	store.AddOrUpdateClusterWatchRule(rule, []string{"nodes"}, "test-target", "test-ns", "test-provider", "test-ns",
		"test-path")
	manager.refreshWatchedTypeTables()
	table, _ = manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	assert.Empty(t, table.Types, "a type the grant no longer covers leaves the plan")
}

// TestCompileClusterWatchRule_TheCompileChecksWhatThePlannerSelects: an item omitting apiGroups whose
// resource two groups serve, but only one at the version it names, is no ambiguity to the planner. The
// compile checks the type the planner selects, so withdrawing its grant refuses the rule and empties
// the plan.
func TestCompileClusterWatchRule_TheCompileChecksWhatThePlannerSelects(t *testing.T) {
	m, _ := makeWatchedTypeManager(t)
	target := bootGitTarget()
	m.Client = bootManager(t, target, granting("*")).Client
	discovery := oneScopedResourceDiscovery("", "namespaces", "Namespace", false)
	discovery.groups = append(discovery.groups, testAPIGroup("other.example", "v2"))
	discovery.resources = append(discovery.resources, &metav1.APIResourceList{
		GroupVersion: "other.example/v2", APIResources: []metav1.APIResource{
			{Name: "namespaces", Kind: "Namespace", Verbs: metav1.Verbs{"get", "list", "watch"}},
		},
	})
	seedClusterCatalog(t, m, configPlaneClusterID, discovery)
	dest := types.NewResourceReference(target.Name, target.Namespace)
	m.rememberGitTargetCluster(dest, configPlaneClusterID)
	rule := clusterRuleFor("rule", configv1alpha3.ClusterResourceRule{
		Resources: []string{"namespaces"}, APIVersions: []string{"v1"},
	})

	decision, err := CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, rule, *target,
		m.ClusterResourcesSelectedFor(*target))
	require.NoError(t, err)
	require.True(t, decision.Admitted, decision.Message)
	m.refreshWatchedTypeTables()
	before, _ := m.watchedTypeTableForGitDest(dest)
	require.Len(t, before.Types, 1)

	stored := &configv1alpha3.KubeProvider{}
	require.NoError(t, m.Client.Get(context.Background(), granting().Key(), stored))
	stored.Spec.AllowedGitTargets = granting().Spec.AllowedGitTargets
	require.NoError(t, m.Client.Update(context.Background(), stored))
	decision, err = CompileClusterWatchRule(context.Background(), m.Client, m.RuleStore, rule, *target,
		m.ClusterResourcesSelectedFor(*target))
	require.NoError(t, err)
	assert.Equal(t, ClusterWatchRuleReasonClusterResourceNotAllowed, decision.Reason)
	assert.Contains(t, decision.Message, `spec.rules[0] asks for "namespaces"`)
	m.refreshWatchedTypeTables()
	after, _ := m.watchedTypeTableForGitDest(dest)
	assert.Empty(t, after.Types, "the revoked type leaves the plan")
}
