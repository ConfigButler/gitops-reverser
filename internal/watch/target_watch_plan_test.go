// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
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

func planCell(gvr schema.GroupVersionResource, namespace string) types.CellKey {
	return types.CellKeyFor(gvr, namespace)
}

func planOf(cells map[types.CellKey]cellSpec) targetWatchPlan {
	return targetWatchPlan{Cells: cells}
}

func TestTargetWatchPlanFor_ReKeysByCellAndCarriesTheVersion(t *testing.T) {
	plan, err := targetWatchPlanFor(map[targetWatchKey]string{
		{GVR: planV1, Namespace: "team-a"}: "[CREATE]",
		{GVR: planApps}:                    "[*]",
	})

	require.NoError(t, err)
	require.Len(t, plan.Cells, 2)
	assert.Equal(t, cellSpec{Operations: "[CREATE]", Version: "v1"}, plan.Cells[planCell(planV1, "team-a")])
	assert.Equal(t, cellSpec{Operations: "[*]", Version: "v1"}, plan.Cells[planCell(planApps, "")])
}

// The whole point of re-keying: one storage-version bump is ONE cell, so it is a restart of that
// cell rather than a stop of one key plus a start of another.
func TestTargetWatchPlanFor_AServedVersionBumpKeepsTheSameCell(t *testing.T) {
	before, err := targetWatchPlanFor(map[targetWatchKey]string{{GVR: planV1, Namespace: "team-a"}: "[CREATE]"})
	require.NoError(t, err)
	after, err := targetWatchPlanFor(map[targetWatchKey]string{{GVR: planV1beta, Namespace: "team-a"}: "[CREATE]"})
	require.NoError(t, err)

	diff := diffTargetWatchPlans(before, after, false)

	assert.Equal(t, []types.CellKey{planCell(planV1, "team-a")}, diff.Restart)
	assert.Empty(t, diff.Start)
	assert.Empty(t, diff.Stop)
	assert.Empty(t, diff.Keep)
}

// targetWatchStreams guarantees one stream per cell, so this is an assertion on an invariant
// rather than a case the renderer can produce.
func TestTargetWatchPlanFor_RejectsTwoStreamsOnOneCell(t *testing.T) {
	_, err := targetWatchPlanFor(map[targetWatchKey]string{
		{GVR: planV1, Namespace: "team-a"}:     "[CREATE]",
		{GVR: planV1beta, Namespace: "team-a"}: "[CREATE]",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "configmaps in team-a")
	assert.Contains(t, err.Error(), "v1beta1")
}

func TestTargetWatchPlanFor_EmptySpecsIsAnEmptyPlan(t *testing.T) {
	plan, err := targetWatchPlanFor(nil)

	require.NoError(t, err)
	assert.Empty(t, plan.Cells)
}

func TestDiffTargetWatchPlans(t *testing.T) {
	teamA := planCell(planV1, "team-a")
	teamB := planCell(planV1, "team-b")
	clusterWide := planCell(planApps, "")
	create := cellSpec{Operations: "[CREATE]", Version: "v1"}
	update := cellSpec{Operations: "[UPDATE]", Version: "v1"}
	createV1beta := cellSpec{Operations: "[CREATE]", Version: "v1beta1"}

	tests := []struct {
		name     string
		previous targetWatchPlan
		desired  targetWatchPlan
		force    bool
		keep     []types.CellKey
		start    []types.CellKey
		restart  []types.CellKey
		stop     []types.CellKey
	}{
		{
			name:     "an unchanged plan keeps every cell",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, teamB: update}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: create, teamB: update}),
			keep:     []types.CellKey{teamA, teamB},
		},
		{
			name:     "adding a rule starts one cell and keeps the rest",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			keep:     []types.CellKey{teamA},
			start:    []types.CellKey{teamB},
		},
		{
			name:     "removing one of several rules stops one cell",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: create}),
			keep:     []types.CellKey{teamA},
			stop:     []types.CellKey{teamB},
		},
		{
			name:     "an operation-filter edit restarts that cell alone",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: update, teamB: create}),
			keep:     []types.CellKey{teamB},
			restart:  []types.CellKey{teamA},
		},
		{
			name:     "a served-version change restarts that cell alone",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: createV1beta, teamB: create}),
			keep:     []types.CellKey{teamB},
			restart:  []types.CellKey{teamA},
		},
		{
			name:     "a forced recheck restarts every desired cell, new ones included",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			force:    true,
			restart:  []types.CellKey{teamA, teamB},
		},
		{
			name:     "a forced recheck still stops a dropped cell",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create}),
			desired:  planOf(map[types.CellKey]cellSpec{teamA: create}),
			force:    true,
			restart:  []types.CellKey{teamA},
			stop:     []types.CellKey{teamB},
		},
		{
			name:    "a cold start starts everything",
			desired: planOf(map[types.CellKey]cellSpec{teamA: create, clusterWide: create}),
			start:   []types.CellKey{teamA, clusterWide},
		},
		{
			name:     "an emptied plan stops everything",
			previous: planOf(map[types.CellKey]cellSpec{teamA: create, clusterWide: create}),
			stop:     []types.CellKey{teamA, clusterWide},
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

// A cluster-wide cell is a PEER of a named namespace on the same type, so narrowing one rule
// must not be read as a change to the other.
func TestDiffTargetWatchPlans_ClusterWideAndNamespacedAreDistinctCells(t *testing.T) {
	clusterWide := planCell(planV1, "")
	namespaced := planCell(planV1, "team-a")
	create := cellSpec{Operations: "[CREATE]", Version: "v1"}

	diff := diffTargetWatchPlans(
		planOf(map[types.CellKey]cellSpec{clusterWide: create}),
		planOf(map[types.CellKey]cellSpec{namespaced: create}),
		false,
	)

	assert.Equal(t, []types.CellKey{namespaced}, diff.Start)
	assert.Equal(t, []types.CellKey{clusterWide}, diff.Stop)
}

func TestDescribeCells_NamesEveryCellWithItsSpec(t *testing.T) {
	teamA := planCell(planV1, "team-a")
	clusterWide := planCell(planApps, "")
	plan := planOf(map[types.CellKey]cellSpec{
		teamA:       {Operations: "[CREATE]", Version: "v1"},
		clusterWide: {Operations: "[*]", Version: "v1beta1"},
	})

	got := describeCells([]types.CellKey{clusterWide, teamA}, plan)

	assert.Equal(t, "deployments.apps=[*]@v1beta1 | configmaps in team-a=[CREATE]@v1", got)
	assert.Empty(t, describeCells(nil, plan))
}

func TestLogTargetWatchPlanDiff_NamesEveryCategoryItHas(t *testing.T) {
	log, lines := recordingLogger()
	teamA := planCell(planV1, "team-a")
	teamB := planCell(planV1, "team-b")
	create := cellSpec{Operations: "[CREATE]", Version: "v1"}
	previous := planOf(map[types.CellKey]cellSpec{teamA: create, teamB: create})
	desired := planOf(map[types.CellKey]cellSpec{teamA: create})

	logTargetWatchPlanDiff(log, previous, desired, diffTargetWatchPlans(previous, desired, false))

	require.Len(t, *lines, 1)
	line := (*lines)[0]
	assert.Contains(t, line, `"keep"=1`)
	assert.Contains(t, line, `"stop"=1`)
	assert.Contains(t, line, `"keepCells"="configmaps in team-a=[CREATE]@v1"`)
	// A stopped cell has no desired spec left, so it is described from the previous plan.
	assert.Contains(t, line, `"stopCells"="configmaps in team-b=[CREATE]@v1"`)
	assert.NotContains(t, line, "startCells")
	assert.NotContains(t, line, "restartCells")
}

func TestReportTargetWatchPlanDiff_ReportsAPlanItCouldNotBuild(t *testing.T) {
	log, lines := recordingLogger()

	reportTargetWatchPlanDiff(log, targetWatchPlan{}, targetWatchPlan{}, assert.AnError, false)

	require.Len(t, *lines, 1)
	assert.Contains(t, (*lines)[0], assert.AnError.Error())
}

// The no-op path returns before touching anything, so the classification has to be logged
// before that early return or the all-keep case would never appear.
func TestReplaceGitTargetWatches_LogsTheDiffOnTheNoOpPath(t *testing.T) {
	log, lines := recordingLogger()
	gitDest := types.NewResourceReference("target", "default")
	table := WatchedTypeTable{
		GitDest: gitDest,
		Types: []WatchedType{{
			GVR:          configmapsGVR,
			NamespaceOps: map[string]OperationSet{"apps": {"CREATE": struct{}{}}},
		}},
	}
	manager := &Manager{
		Log: log,
		targetWatches: map[string]*targetWatchSet{
			gitDest.Key(): {cancel: func() {}, specs: targetWatchSpecs(table)},
		},
	}

	require.NoError(t, manager.replaceGitTargetWatches(context.Background(), table))

	require.Equal(t, 1, countContaining(*lines, "target watch plan diff"),
		"the all-keep case must still be classified and logged")
	assert.Contains(t, (*lines)[0], `"keep"=1`)
	assert.Contains(t, (*lines)[0], `"keepCells"="configmaps in apps=[CREATE]@v1"`)
	assert.Zero(t, countContaining(*lines, "watch set reconciled"),
		"the no-op path must stay a no-op apart from the diff log")
}

// Two resources in one group sort by resource, which is the branch a group-only comparison
// would leave arbitrary.
func TestDiffTargetWatchPlans_SortsWithinAGroupByResource(t *testing.T) {
	deployments := planCell(planApps, "team-a")
	statefulsets := planCell(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "team-a")
	create := cellSpec{Operations: "[CREATE]", Version: "v1"}

	diff := diffTargetWatchPlans(
		targetWatchPlan{},
		planOf(map[types.CellKey]cellSpec{statefulsets: create, deployments: create}),
		false,
	)

	assert.Equal(t, []types.CellKey{deployments, statefulsets}, diff.Start)
}

func TestTargetWatchPlansLocked_ReportsAPriorSetThatBreaksTheInvariant(t *testing.T) {
	gitDest := types.NewResourceReference("target", "default")
	manager := &Manager{
		targetWatches: map[string]*targetWatchSet{
			gitDest.Key(): {cancel: func() {}, specs: map[targetWatchKey]string{
				{GVR: planV1, Namespace: "team-a"}:     "[CREATE]",
				{GVR: planV1beta, Namespace: "team-a"}: "[CREATE]",
			}},
		},
	}

	_, _, err := manager.targetWatchPlansLocked(gitDest.Key(), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "configmaps in team-a")
}
