// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestRefuseCollectionOverlaps(t *testing.T) {
	rec := typesetRecordForTest(configmapsGVR)
	older := selectingRule{
		kind:      ruleKindWatchRule,
		namespace: "ns",
		name:      "b",
		createdAt: metav1.NewTime(time.Unix(1, 0)),
	}
	newer := selectingRule{
		kind:      ruleKindWatchRule,
		namespace: "ns",
		name:      "a",
		createdAt: metav1.NewTime(time.Unix(2, 0)),
	}
	sameAgeLowerName := selectingRule{kind: ruleKindWatchRule, namespace: "ns", name: "a", createdAt: older.createdAt}

	t.Run("the older rule wins regardless of order", func(t *testing.T) {
		admitted, refused := refuseCollectionOverlaps([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "b=1", rule: newer},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: older},
		})
		require.Len(t, admitted, 1)
		assert.Equal(t, older, admitted[0].rule)
		assert.Contains(t, refused, newer)
	})
	t.Run("equal age falls back to the lower name", func(t *testing.T) {
		_, refused := refuseCollectionOverlaps([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "b=1", rule: older},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: sameAgeLowerName},
		})
		assert.Contains(t, refused, older)
		assert.NotContains(t, refused, sameAgeLowerName)
	})
	t.Run("a rule overlapping itself is refused whatever its age", func(t *testing.T) {
		for _, selector := range []string{"", "a=1"} {
			admitted, refused := refuseCollectionOverlaps([]watchSelection{
				{record: rec, namespace: "", labelSelector: selector, rule: older},
				{record: rec, namespace: "x", labelSelector: selector, rule: older},
			})
			assert.Empty(t, admitted)
			assert.Contains(t, refused[older], "its items select overlapping collections")
			assert.Contains(t, refused[older], "configmaps in all namespaces")
			assert.Contains(t, refused[older], "configmaps in x")
		}
	})
	// The structural rule: a selector never makes two overlapping scopes disjoint, because a
	// snapshot owns its whole scope and a later relabel can move an object between them.
	for name, selectors := range map[string][2]string{
		"equal selectors":     {"a=1", "a=1"},
		"no selectors":        {"", ""},
		"different selectors": {"a=1", "b=1"},
	} {
		t.Run("all namespaces and a named namespace overlap with "+name, func(t *testing.T) {
			admitted, refused := refuseCollectionOverlaps([]watchSelection{
				{record: rec, namespace: "", labelSelector: selectors[0], rule: older},
				{record: rec, namespace: "x", labelSelector: selectors[1], rule: newer},
			})
			require.Len(t, admitted, 1)
			assert.Equal(t, older, admitted[0].rule)
			assert.Contains(t, refused[newer], "configmaps in x")
			assert.Contains(t, refused[newer], "configmaps in all namespaces")
			assert.Contains(t, refused[newer], "the older WatchRule ns/b")
		})
	}
	t.Run("an exact duplicate is one collection", func(t *testing.T) {
		admitted, refused := refuseCollectionOverlaps([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "a=1", rule: older},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: newer},
		})
		assert.Len(t, admitted, 2)
		assert.Empty(t, refused)
	})
	t.Run("disjoint namespaces may use different selectors", func(t *testing.T) {
		admitted, refused := refuseCollectionOverlaps([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "a=1", rule: older},
			{record: rec, namespace: "y", labelSelector: "b=1", rule: newer},
		})
		assert.Len(t, admitted, 2)
		assert.Empty(t, refused)
	})
	t.Run("other types never overlap", func(t *testing.T) {
		admitted, refused := refuseCollectionOverlaps([]watchSelection{
			{record: rec, namespace: "", labelSelector: "a=1", rule: older},
			{record: typesetRecordForTest(resolverGVR), namespace: "x", labelSelector: "", rule: newer},
		})
		assert.Len(t, admitted, 2)
		assert.Empty(t, refused)
	})
}

// Equivalent selectors written differently canonicalize to one collection, so two rules asking for
// it are an exact duplicate: neither is refused, and the target streams it once.
func TestResolveWatchedTypeTables_ExactDuplicatesShareOneStream(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	now := time.Unix(1000, 0)
	addSelectorRule(store, selectorRule("labels", "team-a", now, labels("team", "a"), ""), "team-a")
	addSelectorRule(store, selectorRule("expression", "team-a", now.Add(time.Minute), &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "a"}},
		},
	}, ""), "team-a")

	manager.refreshWatchedTypeTables()
	table, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	for _, name := range []string{"labels", "expression"} {
		refused, message := manager.CollectionOverlapForWatchRule(selectorRule(name, "team-a", now, nil, ""))
		assert.False(t, refused, message)
	}
	assert.Equal(t, []targetWatchKey{{GVR: configmapsGVR, Namespace: "team-a", LabelSelector: teamA}},
		targetWatchKeys(table), "one collection, one stream")
}

// The overlap is per GitTarget. Another target's streams are its own, so the same all-namespaces
// and named-namespace pair is admitted when each half belongs to a different target.
func TestResolveWatchedTypeTables_SeparateGitTargetsMayOverlap(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	now := time.Unix(1000, 0)
	wide := selectorRule("wide", "test-ns", now, nil, configv1alpha3.SourceNamespaceWildcard)
	wide.Spec.GitTargetRef.Name = "wide-target"
	store.AddOrUpdateWatchRule(wide, [][]string{{""}},
		"wide-target", "test-ns", "test-provider", "test-ns", "main", "wide")
	addSelectorRule(store, selectorRule("named", "team-a", now.Add(time.Minute), nil, ""), "team-a")

	manager.refreshWatchedTypeTables()
	wideTable, _ := manager.watchedTypeTableForGitDest(gitDestRef("wide-target"))
	namedTable, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	assert.Equal(t, []string{""}, wideTable.Types[0].WatchScopes())
	assert.Equal(t, []string{"team-a"}, namedTable.Types[0].WatchScopes())
	for _, rule := range []configv1alpha3.WatchRule{wide, selectorRule("named", "team-a", now, nil, "")} {
		refused, message := manager.CollectionOverlapForWatchRule(rule)
		assert.False(t, refused, message)
	}
}

// A refusal follows the configuration: removing the older rule admits the newer one, and the
// running plan swaps the collections with the old stream retired before the new one starts, so the
// target never has two producers over one object.
func TestCollectionOverlap_RemovingTheOlderRuleAdmitsTheNewer(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	older := time.Unix(1000, 0)
	named := selectorRule("named", "team-a", older, nil, "")
	wide := selectorRule("wide", "test-ns", older.Add(time.Minute), nil, configv1alpha3.SourceNamespaceWildcard)
	addSelectorRule(store, named, "team-a")
	addSelectorRule(store, wide, "")

	manager.refreshWatchedTypeTables()
	refused, message := manager.CollectionOverlapForWatchRule(wide)
	require.True(t, refused)
	assert.Contains(t, message, "WatchRule test-ns/wide is refused")
	assert.Contains(t, message, "configmaps in all namespaces with no objectSelector")
	assert.Contains(t, message, "configmaps in team-a with no objectSelector already selected by the older "+
		"WatchRule team-a/named")
	before, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))

	store.Delete(ruleKey(named))
	manager.refreshWatchedTypeTables()
	refused, _ = manager.CollectionOverlapForWatchRule(wide)
	assert.False(t, refused, "the refusal clears once the overlapping rule is gone")
	after, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	assert.Equal(t, []string{""}, after.Types[0].WatchScopes())

	gitDest := gitDestRef("sel-target")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running, opened := planTestManager(t, gitDest)
	require.NoError(t, running.replaceGitTargetWatches(ctx, before))
	first := receiveOpenedWatch(t, opened)
	assertNoOpenedWatch(t, opened) // the refused all-namespaces subscription never started
	retiring := running.targetWatches[gitDest.Key()].streams[types.CollectionKeyFor(configmapsGVR, "team-a")]
	require.NotNil(t, retiring)

	require.NoError(t, running.replaceGitTargetWatches(ctx, after))
	retiring.gate.mu.Lock()
	retired := retiring.gate.retired
	retiring.gate.mu.Unlock()
	assert.True(t, retired, "the named stream is retired before the all-namespaces stream is launched")
	second := receiveOpenedWatch(t, opened)
	assert.Empty(t, second.namespace)
	assertStopped(t, first)
	assert.Len(t, running.targetWatches[gitDest.Key()].streams, 1)
}

// A catalog change can create an overlap no rule edit made: a type the older rule names starts
// being served, and the newer rule's collection for it overlaps. Resolution re-runs on the registry
// change and refuses the newer rule then.
func TestCollectionOverlap_ACatalogChangeCanCreateOne(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	withoutWidgets := newCommonTestDiscovery()
	withWidgets := newCommonTestDiscovery()
	listWatch := metav1.Verbs{"get", "list", "watch"}
	withWidgets.groups = append(withWidgets.groups, testAPIGroup("example.com", "v1"))
	withWidgets.resources = append(withWidgets.resources, &metav1.APIResourceList{
		GroupVersion: "example.com/v1",
		APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: listWatch}},
	})
	_, err := manager.resourceCatalog.Refresh(withoutWidgets)
	require.NoError(t, err)

	older := time.Unix(1000, 0)
	wide := selectorRule("wide", "test-ns", older, nil, configv1alpha3.SourceNamespaceWildcard)
	wide.Spec.Rules[0].APIGroups = []string{"example.com"}
	wide.Spec.Rules[0].APIVersions = nil
	wide.Spec.Rules[0].Resources = []string{"widgets"}
	named := selectorRule("named", "team-a", older.Add(time.Minute), nil, "")
	named.Spec.Rules[0].APIGroups = []string{"", "example.com"}
	named.Spec.Rules[0].APIVersions = nil
	named.Spec.Rules[0].Resources = []string{"configmaps", "widgets"}
	addSelectorRule(store, wide, "")
	addSelectorRule(store, named, "team-a")

	manager.refreshWatchedTypeTables()
	refused, message := manager.CollectionOverlapForWatchRule(named)
	require.False(t, refused, message)

	_, err = manager.resourceCatalog.Refresh(withWidgets)
	require.NoError(t, err)
	manager.refreshClusterTypeRegistry(manager.cluster(configPlaneClusterID))
	manager.refreshWatchedTypeTables()

	refused, message = manager.CollectionOverlapForWatchRule(named)
	assert.True(t, refused)
	assert.Contains(t, message, "widgets.example.com in team-a")
	assert.Contains(t, message, "widgets.example.com in all namespaces")
	table, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	keys := targetWatchKeys(table)
	require.Len(t, keys, 1, "the refused rule's configmaps stream goes with it: a refusal is whole-rule")
	assert.Equal(t, "widgets", keys[0].GVR.Resource)
	assert.Empty(t, keys[0].Namespace)
}
