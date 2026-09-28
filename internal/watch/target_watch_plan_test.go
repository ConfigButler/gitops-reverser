// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

var (
	planV1     = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	planV1beta = schema.GroupVersionResource{Version: "v1beta1", Resource: "configmaps"}
	planApps   = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

func planCollection(gvr schema.GroupVersionResource, namespace string) types.CollectionKey {
	return types.CollectionKeyFor(gvr, namespace)
}

func planOf(collections map[types.CollectionKey]watchSpec) targetWatchPlan {
	return targetWatchPlan{Collections: collections}
}

func TestTargetWatchPlanFor_ReKeysByCollectionAndCarriesTheVersion(t *testing.T) {
	plan, err := targetWatchPlanFor([]targetWatchKey{
		{GVR: planV1, Namespace: "team-a"},
		{GVR: planApps},
	})

	require.NoError(t, err)
	require.Len(t, plan.Collections, 2)
	assert.Equal(t, watchSpec{Version: "v1"}, plan.Collections[planCollection(planV1, "team-a")])
	assert.Equal(t, watchSpec{Version: "v1"}, plan.Collections[planCollection(planApps, "")])
}

// The whole point of re-keying: one storage-version bump is ONE collection, so it is a restart of that
// collection rather than a stop of one key plus a start of another.
func TestTargetWatchPlanFor_AServedVersionBumpKeepsTheSameCollection(t *testing.T) {
	before, err := targetWatchPlanFor([]targetWatchKey{{GVR: planV1, Namespace: "team-a"}})
	require.NoError(t, err)
	after, err := targetWatchPlanFor([]targetWatchKey{{GVR: planV1beta, Namespace: "team-a"}})
	require.NoError(t, err)

	diff := diffTargetWatchPlans(before, after, false)

	assert.Equal(t, []types.CollectionKey{planCollection(planV1, "team-a")}, diff.Restart)
	assert.Empty(t, diff.Start)
	assert.Empty(t, diff.Stop)
	assert.Empty(t, diff.Keep)
}

// targetWatchStreams guarantees one stream per collection, so this is an assertion on an invariant
// rather than a case the renderer can produce.
func TestTargetWatchPlanFor_RejectsTwoStreamsOnOneCollection(t *testing.T) {
	_, err := targetWatchPlanFor([]targetWatchKey{
		{GVR: planV1, Namespace: "team-a"},
		{GVR: planV1beta, Namespace: "team-a"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "configmaps in team-a")
	assert.Contains(t, err.Error(), "v1beta1")
}

func TestTargetWatchPlanFor_EmptySpecsIsAnEmptyPlan(t *testing.T) {
	plan, err := targetWatchPlanFor(nil)

	require.NoError(t, err)
	assert.Empty(t, plan.Collections)
}

func TestDiffTargetWatchPlans(t *testing.T) {
	teamA := planCollection(planV1, "team-a")
	teamB := planCollection(planV1, "team-b")
	clusterWide := planCollection(planApps, "")
	v1 := watchSpec{Version: "v1"}
	v1beta := watchSpec{Version: "v1beta1"}

	tests := []struct {
		name     string
		previous targetWatchPlan
		desired  targetWatchPlan
		force    bool
		keep     []types.CollectionKey
		start    []types.CollectionKey
		restart  []types.CollectionKey
		stop     []types.CollectionKey
	}{
		{
			name:     "an unchanged plan keeps every collection",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			keep:     []types.CollectionKey{teamA, teamB},
		},
		{
			name:     "adding a rule starts one collection and keeps the rest",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			keep:     []types.CollectionKey{teamA},
			start:    []types.CollectionKey{teamB},
		},
		{
			name:     "removing one of several rules stops one collection",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1}),
			keep:     []types.CollectionKey{teamA},
			stop:     []types.CollectionKey{teamB},
		},
		{
			name:     "a served-version change restarts that collection alone",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1beta, teamB: v1}),
			keep:     []types.CollectionKey{teamB},
			restart:  []types.CollectionKey{teamA},
		},
		{
			name:     "a forced recheck restarts every desired collection, new ones included",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			force:    true,
			restart:  []types.CollectionKey{teamA, teamB},
		},
		{
			name:     "a forced recheck still stops a dropped collection",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1, teamB: v1}),
			desired:  planOf(map[types.CollectionKey]watchSpec{teamA: v1}),
			force:    true,
			restart:  []types.CollectionKey{teamA},
			stop:     []types.CollectionKey{teamB},
		},
		{
			name:    "a cold start starts everything",
			desired: planOf(map[types.CollectionKey]watchSpec{teamA: v1, clusterWide: v1}),
			start:   []types.CollectionKey{teamA, clusterWide},
		},
		{
			name:     "an emptied plan stops everything",
			previous: planOf(map[types.CollectionKey]watchSpec{teamA: v1, clusterWide: v1}),
			stop:     []types.CollectionKey{teamA, clusterWide},
		},
		{
			name: "an empty plan on both sides classifies nothing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diff := diffTargetWatchPlans(tc.previous, tc.desired, tc.force)

			assert.Equal(t, tc.keep, diff.Keep, "keep")
			assert.Equal(t, tc.start, diff.Start, "start")
			assert.Equal(t, tc.restart, diff.Restart, "restart")
			assert.Equal(t, tc.stop, diff.Stop, "stop")
		})
	}
}

// A cluster-wide collection is a PEER of a named namespace on the same type, so narrowing one rule
// must not be read as a change to the other.
func TestDiffTargetWatchPlans_ClusterWideAndNamespacedAreDistinctCollections(t *testing.T) {
	clusterWide := planCollection(planV1, "")
	namespaced := planCollection(planV1, "team-a")
	create := watchSpec{Version: "v1"}

	diff := diffTargetWatchPlans(
		planOf(map[types.CollectionKey]watchSpec{clusterWide: create}),
		planOf(map[types.CollectionKey]watchSpec{namespaced: create}),
		false,
	)

	assert.Equal(t, []types.CollectionKey{namespaced}, diff.Start)
	assert.Equal(t, []types.CollectionKey{clusterWide}, diff.Stop)
}

func TestDescribeCollections_NamesEveryCollectionWithItsSpec(t *testing.T) {
	teamA := planCollection(planV1, "team-a")
	clusterWide := planCollection(planApps, "")
	plan := planOf(map[types.CollectionKey]watchSpec{
		teamA:       {Version: "v1"},
		clusterWide: {Version: "v1beta1"},
	})

	got := describeCollections([]types.CollectionKey{clusterWide, teamA}, plan)

	assert.Equal(t, "deployments.apps@v1beta1 | configmaps in team-a@v1", got)
	assert.Empty(t, describeCollections(nil, plan))
}

func TestLogTargetWatchPlanDiff_NamesEveryCategoryItHas(t *testing.T) {
	log, lines := recordingLogger()
	teamA := planCollection(planV1, "team-a")
	teamB := planCollection(planV1, "team-b")
	create := watchSpec{Version: "v1"}
	previous := planOf(map[types.CollectionKey]watchSpec{teamA: create, teamB: create})
	desired := planOf(map[types.CollectionKey]watchSpec{teamA: create})

	logTargetWatchPlanDiff(log, previous, desired, diffTargetWatchPlans(previous, desired, false))

	require.Len(t, *lines, 1)
	line := (*lines)[0]
	assert.Contains(t, line, `"keep"=1`)
	assert.Contains(t, line, `"stop"=1`)
	assert.Contains(t, line, `"keepCollections"="configmaps in team-a@v1"`)
	// A stopped collection has no desired spec left, so it is described from the previous plan.
	assert.Contains(t, line, `"stopCollections"="configmaps in team-b@v1"`)
	assert.NotContains(t, line, "startCollections")
	assert.NotContains(t, line, "restartCollections")
}

// An all-keep reconcile must still be classified and logged: "nothing changed" is the most
// common outcome, and it is the one that proves no unrelated collection was replayed.
func TestReplaceGitTargetWatches_LogsAnAllKeepReconcileAndTouchesNothing(t *testing.T) {
	log, lines := recordingLogger()
	gitDest := types.NewResourceReference("target", "default")
	table := WatchedTypeTable{
		GitDest: gitDest,
		Types: []WatchedType{{
			GVR:             configmapsGVR,
			NamespaceScopes: map[string]struct{}{"apps": {}},
		}},
	}
	manager := &Manager{Log: log}
	cancelled := false
	key := targetWatchKey{GVR: configmapsGVR, Namespace: "apps"}
	manager.targetWatchSet(gitDest).streams[key.Collection()] = &runningTargetWatch{
		key: key, cancel: func() { cancelled = true },
	}

	require.NoError(t, manager.replaceGitTargetWatches(context.Background(), table))

	require.Equal(t, 1, countContaining(*lines, "target watch plan reconciled"))
	planLine := firstContaining(t, *lines, "target watch plan reconciled")
	assert.Contains(t, planLine, `"keep"=1`)
	assert.Contains(t, planLine, `"keepCollections"="configmaps in apps@v1"`)
	assert.False(t, cancelled, "a kept collection's stream must not be cancelled")
}

// Two resources in one group sort by resource, which is the branch a group-only comparison
// would leave arbitrary.
func TestDiffTargetWatchPlans_SortsWithinAGroupByResource(t *testing.T) {
	deployments := planCollection(planApps, "team-a")
	statefulsets := planCollection(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "team-a")
	create := watchSpec{Version: "v1"}

	diff := diffTargetWatchPlans(
		targetWatchPlan{},
		planOf(map[types.CollectionKey]watchSpec{statefulsets: create, deployments: create}),
		false,
	)

	assert.Equal(t, []types.CollectionKey{deployments, statefulsets}, diff.Start)
}

// The set's own side of the diff is what is RUNNING, so a plan change is measured against the
// streams rather than against the declaration that started them.
func TestTargetWatchSet_PlanDescribesTheRunningStreams(t *testing.T) {
	set := &targetWatchSet{streams: map[types.CollectionKey]*runningTargetWatch{}}
	key := targetWatchKey{GVR: planV1beta, Namespace: "team-a"}
	set.streams[key.Collection()] = &runningTargetWatch{key: key, cancel: func() {}}

	plan := set.plan()

	assert.Equal(t,
		map[types.CollectionKey]watchSpec{planCollection(planV1, "team-a"): {Version: "v1beta1"}},
		plan.Collections,
		"the collection is versionless; the served version it opened at is spec data")
}

func TestTargetWatchSet_StopCancelsOneCollectionAndStopAllTheRest(t *testing.T) {
	set := &targetWatchSet{streams: map[types.CollectionKey]*runningTargetWatch{}}
	stopped := map[string]bool{}
	for _, ns := range []string{"team-a", "team-b"} {
		key := targetWatchKey{GVR: planV1, Namespace: ns}
		set.streams[key.Collection()] = &runningTargetWatch{
			key: key, cancel: func() { stopped[ns] = true },
		}
	}

	set.stop(planCollection(planV1, "team-a"))
	assert.True(t, stopped["team-a"])
	assert.False(t, stopped["team-b"])
	assert.Len(t, set.streams, 1)

	set.stop(planCollection(planV1, "team-a"))
	set.stopAll()
	assert.True(t, stopped["team-b"])
	assert.Empty(t, set.streams)
}

// firstContaining returns the first recorded log line containing substr, so an assertion does not
// depend on how many other lines the call emitted.
func firstContaining(t *testing.T, lines []string, substr string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return line
		}
	}
	t.Fatalf("no log line containing %q", substr)
	return ""
}
