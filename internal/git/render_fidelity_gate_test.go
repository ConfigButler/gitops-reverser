// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestRenderFidelityGate_RequiresEveryScopeToReport(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	status, revisions := restartAll(gate, target, deployment, configMap)
	assert.Equal(t, RenderFidelityUnknown, status.State)
	assert.False(t, gate.AllowsWrites(target))

	status, applied := gate.RecordScopeClean(target, revisions[deployment], deployment)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityUnknown, status.State)
	assert.False(t, gate.AllowsWrites(target))

	status, applied = gate.RecordScopeClean(target, revisions[configMap], configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State)
	assert.True(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_IgnoresStaleRevisionResult(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")

	_, first := restartAll(gate, target, scope)
	_, second := restartAll(gate, target, scope)
	require.NotEqual(t, first[scope], second[scope])
	_, applied := gate.RecordScopeClean(target, first[scope], scope)
	assert.False(t, applied)
	assert.Equal(t, RenderFidelityUnknown, gate.Status(target).State)

	status, applied := gate.RecordScopeClean(target, second[scope], scope)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State)
}

func TestRenderFidelityGate_PerWriteDivergenceClosesTarget(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")
	_, revisions := restartAll(gate, target, scope)
	status, applied := gate.RecordScopeClean(target, revisions[scope], scope)
	require.True(t, applied)
	require.Equal(t, RenderFidelityTrue, status.State)

	status = gate.Fail(target, manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	assert.Equal(t, RenderFidelityFalse, status.State)
	assert.Equal(t, "RenderDoesNotMatchLive", status.Reason)
	assert.False(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_FullRestartReopensAfterGitRepair(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	_, first := restartAll(gate, target, deployment, configMap)
	_, applied := gate.RecordScopeDivergence(target, first[deployment], deployment,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.True(t, applied)
	_, applied = gate.RecordScopeClean(target, first[configMap], configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityFalse, gate.Status(target).State)

	status, second := restartAll(gate, target, deployment, configMap)
	assert.Equal(t, RenderFidelityUnknown, status.State)
	_, applied = gate.RecordScopeClean(target, second[deployment], deployment)
	require.True(t, applied)
	_, applied = gate.RecordScopeClean(target, second[configMap], configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, gate.Status(target).State)
	assert.True(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_PendingScopeCannotOpenLiveWindow(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")
	_, revisions := restartAll(gate, target, scope)
	worker := &BranchWorker{
		contentWriter:        newContentWriter(types.SensitiveResourcePolicy{}),
		renderFidelityGate:   gate,
		branchBufferMaxBytes: DefaultBranchBufferMaxBytes,
	}
	loop := newBranchWorkerEventLoop(worker, DefaultCommitWindow)
	event := Event{GitTargetName: target.Name, GitTargetNamespace: target.Namespace, Operation: "UPDATE"}

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	assert.Nil(t, loop.openWindow, "Unknown must block a new live window")

	_, applied := gate.RecordScopeClean(target, revisions[scope], scope)
	require.True(t, applied)
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	assert.NotNil(t, loop.openWindow, "every scope reporting clean reopens normal writes")
}

// restartAll is the whole-plan reconcile: every scope is restarted, which is what a forced
// recheck does and what every declaration did before the plan was diffed per cell.
func restartAll(
	gate *RenderFidelityGate,
	target types.ResourceReference,
	scopes ...types.CellKey,
) (RenderFidelityStatus, map[types.CellKey]uint64) {
	return gate.Reconcile(target, scopes, scopes)
}

func fidelityScope(group, resource string) types.CellKey {
	return types.CellKeyFor(
		schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}, "default")
}

// The scope set is keyed by cell, so a stream that reports its result after a served-version
// bump still lands on the scope its revision was issued for. Keying on the full GVR left the result
// matching nothing, and the target stayed Unknown — writes closed — until the next restart.
func TestRenderFidelityGate_ScopeIdentityIgnoresServedVersion(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.ResourceReference{Namespace: "default", Name: "app"}
	deployments := func(version string) types.CellKey {
		return types.CellKeyFor(
			schema.GroupVersionResource{Group: "apps", Version: version, Resource: "deployments"}, "team-a")
	}

	status, revisions := restartAll(gate, target, deployments("v1"))
	require.Equal(t, RenderFidelityUnknown, status.State)

	status, applied := gate.RecordScopeClean(target, revisions[deployments("v1")], deployments("v1beta1"))
	require.True(t, applied, "the same cell, observed at another served version, is the same scope")
	assert.Equal(t, RenderFidelityTrue, status.State)
}

// The point of a per-scope revision: a plan change that leaves a cell's stream running must not
// ask that cell to prove itself again. A target-wide epoch closed writes on every plan edit.
func TestRenderFidelityGate_KeptScopeKeepsItsResult(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	_, revisions := restartAll(gate, target, deployment)
	_, applied := gate.RecordScopeClean(target, revisions[deployment], deployment)
	require.True(t, applied)
	require.True(t, gate.AllowsWrites(target))

	// A second cell joins the plan. Only it is restarted.
	status, next := gate.Reconcile(target,
		[]types.CellKey{deployment, configMap}, []types.CellKey{configMap})
	assert.Equal(t, RenderFidelityUnknown, status.State, "the new cell has not replayed yet")
	assert.Equal(t, revisions[deployment], next[deployment], "a kept cell keeps its revision")

	status, applied = gate.RecordScopeClean(target, next[configMap], configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State,
		"the kept cell's earlier result still counts, so one replay is enough")
}

// A target held open by a divergence stays held: adding a WatchRule is no evidence that the
// divergence was resolved.
func TestRenderFidelityGate_UnrelatedPlanChangeCannotClearADivergence(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	_, revisions := restartAll(gate, target, deployment)
	_, applied := gate.RecordScopeDivergence(target, revisions[deployment], deployment,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.True(t, applied)

	status, next := gate.Reconcile(target,
		[]types.CellKey{deployment, configMap}, []types.CellKey{configMap})
	assert.Equal(t, RenderFidelityFalse, status.State)
	_, applied = gate.RecordScopeClean(target, next[configMap], configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityFalse, gate.Status(target).State,
		"only a replay of the DIVERGENT cell may clear it")

	status, reopened := gate.Reconcile(target,
		[]types.CellKey{deployment, configMap}, []types.CellKey{deployment})
	assert.Equal(t, RenderFidelityUnknown, status.State)
	_, applied = gate.RecordScopeClean(target, reopened[deployment], deployment)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, gate.Status(target).State)
}

// A write-path divergence belongs to no scope, so no single scope's replay clears it. Only a
// complete fresh measurement of the target does.
func TestRenderFidelityGate_WriteDivergenceClearsOnlyOnAFullRestart(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")
	scopes := []types.CellKey{deployment, configMap}

	_, revisions := restartAll(gate, target, scopes...)
	for _, scope := range scopes {
		_, applied := gate.RecordScopeClean(target, revisions[scope], scope)
		require.True(t, applied)
	}
	gate.Fail(target, manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.False(t, gate.AllowsWrites(target))

	_, partial := gate.Reconcile(target, scopes, []types.CellKey{deployment})
	_, applied := gate.RecordScopeClean(target, partial[deployment], deployment)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityFalse, gate.Status(target).State,
		"restarting one cell is not a fresh measurement of the target")

	status, full := restartAll(gate, target, scopes...)
	assert.Equal(t, RenderFidelityUnknown, status.State, "the write divergence is cleared, not the pending scopes")
	for _, scope := range scopes {
		_, applied := gate.RecordScopeClean(target, full[scope], scope)
		require.True(t, applied)
	}
	assert.True(t, gate.AllowsWrites(target))
}

// A cell that left the plan is gone: its stream's tail cannot report into it, and its absence
// cannot hold the target Unknown.
func TestRenderFidelityGate_DroppedScopeStopsCounting(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	_, revisions := restartAll(gate, target, deployment, configMap)
	_, applied := gate.RecordScopeClean(target, revisions[deployment], deployment)
	require.True(t, applied)
	require.Equal(t, RenderFidelityUnknown, gate.Status(target).State)

	status, _ := gate.Reconcile(target, []types.CellKey{deployment}, nil)
	assert.Equal(t, RenderFidelityTrue, status.State, "the pending cell left the plan with its stream")

	_, applied = gate.RecordScopeClean(target, revisions[configMap], configMap)
	assert.False(t, applied, "a dropped cell's tail reports into nothing")
}

// A plan that selects nothing restarts every scope only vacuously, which is the absence of a
// measurement rather than a fresh one. Clearing on it would readmit the writes that do not come
// from a watch — an atomic request, a CommitRequest — on the strength of nothing.
func TestRenderFidelityGate_AnEmptyPlanDoesNotClearAWriteDivergence(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")

	_, revisions := restartAll(gate, target, scope)
	_, applied := gate.RecordScopeClean(target, revisions[scope], scope)
	require.True(t, applied)
	gate.Fail(target, manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.False(t, gate.AllowsWrites(target))

	status, _ := gate.Reconcile(target, nil, nil)

	assert.Equal(t, RenderFidelityFalse, status.State)
	assert.False(t, gate.AllowsWrites(target), "a plan that selects nothing measures nothing")
}
