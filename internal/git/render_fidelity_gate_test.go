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

func TestRenderFidelityGate_RequiresEveryScopeInEpoch(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	status := gate.Begin(target, []types.CellKey{deployment, configMap})
	assert.Equal(t, RenderFidelityUnknown, status.State)
	assert.False(t, gate.AllowsWrites(target))

	status, applied := gate.RecordScopeClean(target, status.Epoch, deployment)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityUnknown, status.State)
	assert.False(t, gate.AllowsWrites(target))

	status, applied = gate.RecordScopeClean(target, status.Epoch, configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State)
	assert.True(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_IgnoresStaleEpochResult(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")

	first := gate.Begin(target, []types.CellKey{scope})
	second := gate.Begin(target, []types.CellKey{scope})
	_, applied := gate.RecordScopeClean(target, first.Epoch, scope)
	assert.False(t, applied)
	assert.Equal(t, RenderFidelityUnknown, gate.Status(target).State)

	status, applied := gate.RecordScopeClean(target, second.Epoch, scope)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, status.State)
}

func TestRenderFidelityGate_PerWriteDivergenceClosesTarget(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")
	status := gate.Begin(target, []types.CellKey{scope})
	_, applied := gate.RecordScopeClean(target, status.Epoch, scope)
	require.True(t, applied)

	status = gate.Fail(target, manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	assert.Equal(t, RenderFidelityFalse, status.State)
	assert.Equal(t, "RenderDoesNotMatchLive", status.Reason)
	assert.False(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_FullFreshEpochReopensAfterGitRepair(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	deployment := fidelityScope("apps", "deployments")
	configMap := fidelityScope("", "configmaps")

	first := gate.Begin(target, []types.CellKey{deployment, configMap})
	_, applied := gate.RecordScopeDivergence(target, first.Epoch, deployment,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.True(t, applied)
	_, applied = gate.RecordScopeClean(target, first.Epoch, configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityFalse, gate.Status(target).State)

	second := gate.Begin(target, []types.CellKey{deployment, configMap})
	assert.Equal(t, RenderFidelityUnknown, second.State)
	_, applied = gate.RecordScopeClean(target, second.Epoch, deployment)
	require.True(t, applied)
	_, applied = gate.RecordScopeClean(target, second.Epoch, configMap)
	require.True(t, applied)
	assert.Equal(t, RenderFidelityTrue, gate.Status(target).State)
	assert.True(t, gate.AllowsWrites(target))
}

func TestRenderFidelityGate_ClosedEpochCannotOpenLiveWindow(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.NewResourceReference("apps", "default")
	scope := fidelityScope("apps", "deployments")
	status := gate.Begin(target, []types.CellKey{scope})
	worker := &BranchWorker{
		contentWriter:        newContentWriter(types.SensitiveResourcePolicy{}),
		renderFidelityGate:   gate,
		branchBufferMaxBytes: DefaultBranchBufferMaxBytes,
	}
	loop := newBranchWorkerEventLoop(worker, DefaultCommitWindow)
	event := Event{GitTargetName: target.Name, GitTargetNamespace: target.Namespace, Operation: "UPDATE"}

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	assert.Nil(t, loop.openWindow, "Unknown must block a new live window")

	_, applied := gate.RecordScopeClean(target, status.Epoch, scope)
	require.True(t, applied)
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	assert.NotNil(t, loop.openWindow, "a complete clean epoch reopens normal writes")
}

func fidelityScope(group, resource string) types.CellKey {
	return types.CellKeyFor(
		schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}, "default")
}

// The scope set is keyed by cell, so a stream that reports its result after a served-version
// bump still lands on the scope its epoch began with. Keying on the full GVR left the result
// matching nothing, and the target stayed Unknown — writes closed — until the next epoch.
func TestRenderFidelityGate_ScopeIdentityIgnoresServedVersion(t *testing.T) {
	gate := NewRenderFidelityGate()
	target := types.ResourceReference{Namespace: "default", Name: "app"}
	deployments := func(version string) types.CellKey {
		return types.CellKeyFor(
			schema.GroupVersionResource{Group: "apps", Version: version, Resource: "deployments"}, "team-a")
	}

	status := gate.Begin(target, []types.CellKey{deployments("v1")})
	require.Equal(t, RenderFidelityUnknown, status.State)

	status, applied := gate.RecordScopeClean(target, status.Epoch, deployments("v1beta1"))
	require.True(t, applied, "the same cell, observed at another served version, is the same scope")
	assert.Equal(t, RenderFidelityTrue, status.State)
}
