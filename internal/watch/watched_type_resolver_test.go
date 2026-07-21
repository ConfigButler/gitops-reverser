// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// makeWatchedTypeManager builds a Manager with a real RuleStore and the common test
// catalog, with no informers or workers — enough to exercise watched-type resolution
// and the resident store in isolation.
func makeWatchedTypeManager(t *testing.T) (*Manager, *rulestore.RuleStore) {
	t.Helper()
	store := rulestore.NewStore()
	manager := &Manager{
		Log:             logr.Discard(),
		RuleStore:       store,
		resourceCatalog: newCommonTestCatalog(t),
		discoveryClient: commonTestDiscoveryClient(),
	}
	return manager, store
}

func gitDestRef(name string) types.ResourceReference {
	return types.NewResourceReference(name, "test-ns")
}

func TestRefreshWatchedTypeTables_ClusterWatchRuleResolvesClusterWideType(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	wt := table.Types[0]
	assert.Equal(t, "Namespace", wt.GVK.Kind)
	assert.True(t, wt.ClusterWide(), "a ClusterWatchRule streams its cluster-scoped types cluster-wide")
	assert.Equal(t, []string{""}, wt.WatchScopes())
	assert.Equal(t, `provider=test-ns/test-provider|branch="main"|path="test-path"`, table.Dest)
}

func TestRefreshWatchedTypeTables_WatchRuleScopesTypeToItsNamespace(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-a", "wt-ns-target", "ns-a"),
		ownNamespaceScope(watchRuleForTarget("rule-a", "wt-ns-target", "ns-a")),
		"wt-ns-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	store.AddOrUpdateWatchRule(
		watchRuleForTarget("rule-b", "wt-ns-target", "ns-b"),
		ownNamespaceScope(watchRuleForTarget("rule-b", "wt-ns-target", "ns-b")),
		"wt-ns-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("wt-ns-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"ns-a", "ns-b"}, table.Types[0].WatchScopes())
	assert.False(t, table.Types[0].ClusterWide())
}

func TestRefreshWatchedTypeTables_RuleChangeReResolves(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	first, _ := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.Len(t, first.Types, 1)

	// A second rule selecting a different resource is reflected on the next refresh.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-2", "nodes"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	second, _ := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	kinds := []string{second.Types[0].GVK.Kind, second.Types[1].GVK.Kind}
	assert.ElementsMatch(t, []string{"Namespace", "Node"}, kinds)
}

func TestResolveWatchedTypeTables_NilRuleStoreIsEmpty(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	assert.Empty(t, m.resolveWatchedTypeTables())
}

func TestRefreshWatchedTypeTables_NoChangeReusesResolvedTables(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()
	manager.watchedTypes.mu.Lock()
	firstRegFP := manager.watchedTypes.registriesFP
	firstFP := manager.watchedTypes.rulesFP
	manager.watchedTypes.mu.Unlock()

	// A second refresh with no rule or registry change is a no-op gate hit.
	manager.refreshWatchedTypeTables()
	manager.watchedTypes.mu.Lock()
	assert.Equal(t, firstRegFP, manager.watchedTypes.registriesFP)
	assert.Equal(t, firstFP, manager.watchedTypes.rulesFP)
	manager.watchedTypes.mu.Unlock()
}

func TestRulesFingerprint_StableUntilRuleChanges(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "namespaces"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	fp1 := manager.rulesFingerprint()
	assert.Equal(t, fp1, manager.rulesFingerprint(), "fingerprint must be stable for unchanged rules")

	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-2", "nodes"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	assert.NotEqual(t, fp1, manager.rulesFingerprint(), "a new rule must move the fingerprint")
}

func TestRefreshWatchedTypeTables_KeepsTargetWithUnresolvableRulesAsEmptyTable(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	// "ghosts" is not served by the common catalog: the rule resolves to nothing,
	// but the GitTarget must still appear as an empty table, not vanish.
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-1", "ghosts"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.True(t, ok, "a GitTarget with unresolvable rules must remain a (empty) table")
	assert.Empty(t, table.Types)
}

// A GVK served by more than one resource is refused globally by the registry
// (gvk-not-unique), so it never reaches a GitTarget's table even when a wildcard rule
// selects both resources.
func TestRefreshWatchedTypeTables_ExcludesAmbiguousGVK(t *testing.T) {
	store := rulestore.NewStore()
	manager := &Manager{Log: logr.Discard(), RuleStore: store, resourceCatalog: newWidgetConflictCatalog(t)}
	store.AddOrUpdateClusterWatchRule(
		configv1alpha3.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "rule-widgets"},
			Spec: configv1alpha3.ClusterWatchRuleSpec{
				TargetRef: configv1alpha3.NamespacedTargetReference{Name: "test-target", Namespace: "test-ns"},
				Rules: []configv1alpha3.ClusterResourceRule{{
					APIGroups:   []string{"example.com"},
					APIVersions: []string{"v1"},
					Resources:   []string{"*"},
				}},
			},
		},
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)

	manager.refreshWatchedTypeTables()

	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("test-target"))
	require.True(t, ok)
	assert.Empty(t, table.Types, "a kind served by >1 resource is refused, not watched")
}

// newWidgetConflictCatalog builds a pathological catalog where one group/version serves
// the same kind from two distinct resources, violating the GVK<->GVR 1:1 assumption.
func newWidgetConflictCatalog(t *testing.T) *APIResourceCatalog {
	t.Helper()
	listWatch := metav1.Verbs{"get", "list", "watch"}
	disco := staticCatalogDiscovery{
		groups: []*metav1.APIGroup{testAPIGroup("example.com", "v1")},
		resources: []*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{
					{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: listWatch},
					{Name: "widgetslegacy", Kind: "Widget", Namespaced: true, Verbs: listWatch},
				},
			},
		},
	}
	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(disco)
	require.NoError(t, err)
	return catalog
}
