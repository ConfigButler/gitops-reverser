// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

const teamA = "team in (a)"

func selectorTable(gitDest types.ResourceReference, selectors map[string]string) WatchedTypeTable {
	scopes := map[string]struct{}{}
	for ns := range selectors {
		scopes[ns] = struct{}{}
	}
	return WatchedTypeTable{
		GitDest: gitDest,
		Types:   []WatchedType{{GVR: configmapsGVR, NamespaceScopes: scopes, LabelSelectors: selectors}},
	}
}

// The selector is part of the collection, so each namespace scope streams with its own.
func TestTargetWatchKeys_CarryEachScopesSelector(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	keys := targetWatchKeys(selectorTable(gitDest, map[string]string{"apps": teamA, "ops": ""}))

	require.Equal(t, []targetWatchKey{
		{GVR: configmapsGVR, Namespace: "apps", LabelSelector: teamA},
		{GVR: configmapsGVR, Namespace: "ops"},
	}, keys)
	assert.Equal(t, teamA, keys[0].Collection().LabelSelector)
	assert.Equal(t, "configmaps in apps selecting team in (a)", keys[0].Collection().String())
}

// A selector edit is a different collection: the old stream stops and a new one replays under the
// new selector. An unchanged selector keeps the running stream.
func TestReplaceGitTargetWatches_ASelectorEditStartsANewCollection(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, opened := planTestManager(t, gitDest)

	require.NoError(t, manager.replaceGitTargetWatches(ctx, selectorTable(gitDest, map[string]string{"apps": teamA})))
	first := receiveOpenedWatch(t, opened)
	assert.Equal(t, teamA, first.opts.LabelSelector)

	require.NoError(t, manager.replaceGitTargetWatches(ctx, selectorTable(gitDest, map[string]string{"apps": teamA})))
	assertNoOpenedWatch(t, opened)
	assertStillRunning(t, first)

	require.NoError(t, manager.replaceGitTargetWatches(ctx, selectorTable(gitDest, map[string]string{"apps": ""})))
	widened := receiveOpenedWatch(t, opened)
	assert.Empty(t, widened.opts.LabelSelector)
	assert.True(t, *widened.opts.SendInitialEvents, "a new selection initializes from a complete snapshot")
	assertStopped(t, first)

	set := manager.targetWatches[gitDest.Key()]
	require.Len(t, set.streams, 1)
	_, running := set.streams[types.CollectionKeyFor(configmapsGVR, "apps")]
	assert.True(t, running)
}

// Every request that observes the collection carries the selector: the initial-events watch, the
// fallback watch and LIST, and a resume. One unselected request would observe another collection,
// and its snapshot would feed a sweep.
func TestTargetWatchReplayAndStream_EveryRequestCarriesTheSelector(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: teamA}

	var seen []string
	listed := make(chan struct{})
	manager := &Manager{
		Log: logr.Discard(),
		targetWatchOpen: func(
			_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
		) (watch.Interface, error) {
			seen = append(seen, "watch:"+opts.LabelSelector)
			if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
				return nil, errors.New("sendInitialEvents: Forbidden: sendInitialEvents is forbidden")
			}
			return watch.NewFake(), nil
		},
		targetWatchList: func(
			_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
		) (*unstructured.UnstructuredList, error) {
			seen = append(seen, "list:"+opts.LabelSelector)
			list := &unstructured.UnstructuredList{}
			list.SetResourceVersion("9")
			close(listed)
			return list, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(ctx, logr.Discard(), gitDest, testStream(key), false)
	}()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("expected the fallback LIST to run")
	}
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, []string{"watch:" + teamA, "watch:" + teamA, "list:" + teamA}, seen)
}

func TestTargetWatchResume_LooksUpAndOpensUnderTheSelectedCollection(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: teamA}
	store := &fakeWatchCursorStore{rv: "41", ok: true}
	opened := make(chan metav1.ListOptions, 1)
	manager := &Manager{
		Log:              logr.Discard(),
		WatchCursorStore: store,
		targetWatchOpen: func(
			_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions,
		) (watch.Interface, error) {
			opened <- opts
			return watch.NewFake(), nil
		},
	}
	manager.rememberGitTargetUID(gitDest.WithUID("uid-1"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.targetWatchReplayAndStream(ctx, logr.Discard(), gitDest, testStream(key), true)
	}()
	opts := <-opened
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, "41", opts.ResourceVersion)
	assert.Equal(t, teamA, opts.LabelSelector)
	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, key.Collection(), store.lookedUpCollection, "never resume a cursor another selection recorded")
}

// The resync a selected snapshot queues is keyed by its whole collection, so two selections of one
// boundary never coalesce, while its sweep boundary stays structural.
func TestResyncScopeForWatchKey_CarriesTheSelectorButSweepsStructurally(t *testing.T) {
	selected := resyncScopeForWatchKey(targetWatchKey{GVR: configmapsGVR, Namespace: "apps", LabelSelector: teamA})
	unselected := resyncScopeForWatchKey(targetWatchKey{GVR: configmapsGVR, Namespace: "apps"})

	assert.Equal(t, teamA, selected.Collection.LabelSelector)
	assert.NotEqual(t, selected.String(), unselected.String(), "the coalescing key is the scope's string")

	neverSelected := types.NewResourceIdentifier("", "v1", "configmaps", "apps", "unlabelled")
	assert.True(t, selected.Matches(neverSelected), "a selected snapshot owns every document in its scope")
	assert.False(t, selected.Matches(types.NewResourceIdentifier("", "v1", "configmaps", "ops", "x")))
}

func selectorRule(name, namespace string, created time.Time, selector *metav1.LabelSelector,
	sourceNamespace string,
) configv1alpha3.WatchRule {
	rule := watchRuleForTarget(name, "sel-target", namespace)
	rule.CreationTimestamp = metav1.NewTime(created)
	rule.Spec.Rules[0].ObjectSelector = selector
	rule.Spec.Rules[0].SourceNamespace = sourceNamespace
	return rule
}

func addSelectorRule(store *rulestore.RuleStore, rule configv1alpha3.WatchRule, namespaces ...string) {
	store.AddOrUpdateWatchRule(rule, [][]string{namespaces},
		"sel-target", "test-ns", "test-provider", "test-ns", "main", "test-path")
}

func labels(kv ...string) *metav1.LabelSelector {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		selector.MatchLabels[kv[i]] = kv[i+1]
	}
	return selector
}

// Overlapping collections with different selectors: the older rule keeps its collection, the newer
// one is refused as a whole and contributes nothing to the table.
func TestResolveWatchedTypeTables_RefusesTheNewerOfTwoOverlappingSelectors(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	older := time.Unix(1000, 0)
	addSelectorRule(store, selectorRule("cluster-wide", "test-ns", older, labels("team", "a"), "*"), "")
	addSelectorRule(store, selectorRule("named", "team-b", older.Add(time.Minute), labels("team", "b"), ""), "team-b")

	manager.refreshWatchedTypeTables()
	table, ok := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	require.True(t, ok)
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{""}, table.Types[0].WatchScopes())
	assert.Equal(t, map[string]string{"": teamA}, table.Types[0].LabelSelectors)

	refused, message := manager.ObjectSelectorConflictForWatchRule(
		selectorRule("named", "team-b", older.Add(time.Minute), labels("team", "b"), ""))
	assert.True(t, refused)
	assert.Contains(t, message, "WatchRule test-ns/cluster-wide")
	assert.Contains(t, message, `objectSelector "team in (b)"`)

	refused, _ = manager.ObjectSelectorConflictForWatchRule(
		selectorRule("cluster-wide", "test-ns", older, labels("team", "a"), "*"))
	assert.False(t, refused, "the older rule keeps its collection")
}

func TestResolveWatchedTypeTables_DisjointNamespacesMayUseDifferentSelectors(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	now := time.Unix(1000, 0)
	addSelectorRule(store, selectorRule("a", "team-a", now, labels("team", "a"), ""), "team-a")
	addSelectorRule(store, selectorRule("b", "team-b", now, labels("team", "b"), ""), "team-b")

	manager.refreshWatchedTypeTables()
	table, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	require.Len(t, table.Types, 1)
	assert.Equal(t, map[string]string{"team-a": teamA, "team-b": "team in (b)"}, table.Types[0].LabelSelectors)
	for _, name := range []string{"a", "b"} {
		refused, message := manager.ObjectSelectorConflictForWatchRule(
			selectorRule(name, "team-"+name, now, nil, ""))
		assert.False(t, refused, message)
	}
}

// Equivalent selectors written differently are one canonical selector, so they never conflict.
func TestResolveWatchedTypeTables_EquivalentSelectorsDoNotConflict(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	now := time.Unix(1000, 0)
	addSelectorRule(store, selectorRule("labels", "test-ns", now, labels("team", "a"), "*"), "")
	addSelectorRule(store, selectorRule("expression", "team-a", now.Add(time.Minute), &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "a"}},
		},
	}, ""), "team-a")

	manager.refreshWatchedTypeTables()
	table, _ := manager.watchedTypeTableForGitDest(gitDestRef("sel-target"))
	require.Len(t, table.Types, 1)
	assert.Equal(t, []string{"", "team-a"}, table.Types[0].WatchScopes())
	refused, message := manager.ObjectSelectorConflictForWatchRule(
		selectorRule("expression", "team-a", now.Add(time.Minute), nil, ""))
	assert.False(t, refused, message)
}

func TestRefuseSelectorConflicts(t *testing.T) {
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
		admitted, refused := refuseSelectorConflicts([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "b=1", rule: newer},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: older},
		})
		require.Len(t, admitted, 1)
		assert.Equal(t, older, admitted[0].rule)
		assert.Contains(t, refused, newer)
	})
	t.Run("equal age falls back to the lower name", func(t *testing.T) {
		_, refused := refuseSelectorConflicts([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "b=1", rule: older},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: sameAgeLowerName},
		})
		assert.Contains(t, refused, older)
		assert.NotContains(t, refused, sameAgeLowerName)
	})
	t.Run("a rule conflicting with itself is refused", func(t *testing.T) {
		admitted, refused := refuseSelectorConflicts([]watchSelection{
			{record: rec, namespace: "", labelSelector: "a=1", rule: older},
			{record: rec, namespace: "x", labelSelector: "", rule: older},
		})
		assert.Empty(t, admitted)
		assert.Contains(t, refused[older], "its items select overlapping collections differently")
	})
	t.Run("the same selector over overlapping scopes is allowed", func(t *testing.T) {
		admitted, refused := refuseSelectorConflicts([]watchSelection{
			{record: rec, namespace: "", labelSelector: "a=1", rule: older},
			{record: rec, namespace: "x", labelSelector: "a=1", rule: newer},
		})
		assert.Len(t, admitted, 2)
		assert.Empty(t, refused)
	})
	t.Run("other types never conflict", func(t *testing.T) {
		admitted, refused := refuseSelectorConflicts([]watchSelection{
			{record: rec, namespace: "x", labelSelector: "a=1", rule: older},
			{record: typesetRecordForTest(resolverGVR), namespace: "x", labelSelector: "", rule: newer},
		})
		assert.Len(t, admitted, 2)
		assert.Empty(t, refused)
	})
}

func TestCompileWatchRule_RefusesAnInvalidObjectSelector(t *testing.T) {
	store := rulestore.NewStore()
	rule := watchRuleForTarget("bad", "sel-target", "team-a")
	addSelectorRule(store, rule, "team-a")
	rule.Spec.Rules[0].ObjectSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "team", Operator: metav1.LabelSelectorOpIn}},
	}

	_, err := CompileWatchRule(context.Background(), nil, store, rule,
		configv1alpha3.GitTarget{}, configv1alpha3.GitProvider{})

	var selectorErr *ObjectSelectorError
	require.ErrorAs(t, err, &selectorErr)
	assert.Contains(t, selectorErr.Message, "spec.rules[0].objectSelector")
	_, compiled := store.GetWatchRule(ruleKey(rule))
	assert.False(t, compiled, "a refused rule leaves the store before its status says so")
}

func TestCompileClusterWatchRule_RefusesAnInvalidObjectSelector(t *testing.T) {
	store := rulestore.NewStore()
	rule := clusterRuleForResource("bad", "namespaces")
	store.AddOrUpdateClusterWatchRule(rule, "t", "ns", "p", "ns", "main", "path")
	rule.Spec.Rules[0].ObjectSelector = labels("bad key", "x")

	_, err := CompileClusterWatchRule(context.Background(), nil, store, rule,
		configv1alpha3.GitTarget{}, configv1alpha3.GitProvider{})

	var selectorErr *ObjectSelectorError
	require.ErrorAs(t, err, &selectorErr)
	assert.Empty(t, store.SnapshotClusterWatchRules())
}

func typesetRecordForTest(gvr schema.GroupVersionResource) typeset.TypeRecord {
	return typeset.TypeRecord{Identity: typeset.Identity{GVR: gvr, Scope: typeset.ScopeNamespaced}}
}

func ruleKey(rule configv1alpha3.WatchRule) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}
}
