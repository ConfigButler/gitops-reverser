// SPDX-License-Identifier: Apache-2.0

package git

// The apply window: while an apply GitOps Reverser asked for runs, the applier's own write over an
// object the API side edited since the request was sent is withheld, so the API value wins. Only a
// proven applier write is withheld; one that could be the edit itself is mirrored and counted. See
// apply_window.go.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// windowEvent is a live UPDATE to the Deployment "api" in target's folder, written last by managers.
func windowEvent(target, replicas string, managers ...string) Event {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "shop"},
		"spec":     map[string]any{"replicas": replicas},
	}}
	return Event{
		Object:             obj,
		Identifier:         itypes.NewResourceIdentifier("apps", "v1", "deployments", "shop", "api"),
		Operation:          string(itypes.OperationUpdate),
		LastFieldManagers:  managers,
		GitTargetName:      target,
		GitTargetNamespace: "team-a",
		Path:               "apps/" + target,
	}
}

// sentAttempt is a loop with alpha's applier sent an attempt that is still out.
func sentAttempt(t *testing.T) (*branchWorkerEventLoop, ApplyRequest) {
	t.Helper()
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	owesForeign(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	return loop, out[0]
}

func collisions(t *testing.T, reader *sdkmetric.ManualReader, outcome string) int64 {
	t.Helper()
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_apply_window_collisions_total",
		map[string]string{"outcome": outcome})
	return got
}

// TestApplyWindow_TheAppliersLateWriteOverANewerEditIsWithheld is the race the window exists for:
// Flux applies the commit it was asked for after an API edit landed, and that write must not reach
// Git as Flux's revert of the edit.
func TestApplyWindow_TheAppliersLateWriteOverANewerEditIsWithheld(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "an API edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the applier's write over it is withheld")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
}

// TestApplyWindow_OnlyAWithheldWriteIsKeptFromTheCommitWindow follows both events through the live
// path: the commit window ends up holding the API edit, not Flux's revert of it.
func TestApplyWindow_OnlyAWithheldWriteIsKeptFromTheCommitWindow(t *testing.T) {
	loop, _ := sentAttempt(t)

	loop.handleLiveEvents(&WriteRequest{Events: []Event{
		windowEvent("alpha", "3", "kubectl-edit"),
		windowEvent("alpha", "2", FluxFieldManager),
	}, CommitMode: CommitModePerEvent})

	require.NotNil(t, loop.openWindow)
	require.Len(t, loop.openWindow.pathToEvent, 1)
	for _, event := range loop.openWindow.pathToEvent {
		replicas, _, _ := unstructured.NestedString(event.Object.Object, "spec", "replicas")
		assert.Equal(t, "3", replicas, "Git keeps the API's value")
	}
}

// TestApplyWindow_WhatIsNeverWithheld: the applier's write is mirrored as ever when it could be
// the edit itself, when nobody edited the object since the attempt, when it is a delete, and for a
// folder another applier (or none) applies.
func TestApplyWindow_WhatIsNeverWithheld(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"an object nobody edited since the attempt: the apply writes what Git holds")

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager, "kubectl-edit")),
		"a tie may be the API edit itself, and withholding that would lose it")
	assert.Equal(t, int64(1), collisions(t, reader, collisionAmbiguous))

	deleted := windowEvent("alpha", "", FluxFieldManager)
	deleted.Operation = string(itypes.OperationDelete)
	assert.False(t, loop.withholdLateApply(deleted), "a delete does not say who deleted")

	assert.False(t, loop.withholdLateApply(windowEvent("bravo", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("bravo", "2", FluxFieldManager)),
		"bravo's folder is not the applier's")
	assert.Equal(t, int64(0), collisions(t, reader, collisionWithheld))
}

// TestApplyWindow_NoWindowBeforeAnAttempt: without an attempt out, the applier applies on its own
// schedule and its writes are mirrored as before.
func TestApplyWindow_NoWindowBeforeAnAttempt(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
}

// TestApplyWindow_ClosesAGraceAfterFluxFinished: a delivered attempt keeps its window open a short
// grace, because Flux's writes reach the worker through the watch after Flux reported done; then it
// closes.
func TestApplyWindow_ClosesAGraceAfterFluxFinished(t *testing.T) {
	loop, req := sentAttempt(t)
	report(req, ApplyDelivered)
	loop.serviceAppliers()

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "within the grace")

	for _, state := range loop.appliers {
		state.window.closesAt = time.Now().Add(-time.Millisecond)
	}
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "after it")
}

// TestApplyWindow_AnAttemptThatAppliedNothingHasNoWindow: a stale, failed or dropped attempt
// stamped nothing, so nothing the applier writes afterwards is the apply it asked for.
func TestApplyWindow_AnAttemptThatAppliedNothingHasNoWindow(t *testing.T) {
	for _, outcome := range []ApplyOutcome{ApplyStale, ApplyFailed, ApplyRejected, ApplyDropped, ApplyCurrent} {
		t.Run(string(outcome), func(t *testing.T) {
			loop, req := sentAttempt(t)
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
			report(req, outcome)
			loop.serviceAppliers()
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
		})
	}
}

// TestApplyWindow_ArgoCDsLateWriteIsWithheldByItsOwnManagers: an Argo CD Application's window
// knows Argo CD's managers, its apply (argocd-controller) and its Replace=true create and replace
// (argocd-application-controller), and not Flux's.
func TestApplyWindow_ArgoCDsLateWriteIsWithheldByItsOwnManagers(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
	owesForeign(loop.w)
	loop.serviceAppliers()
	require.Len(t, requester.taken(), 1)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "an API edit is mirrored")
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"Flux is not this applier: its write is an edit like any other")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager)),
		"Argo CD's apply over the newer edit is withheld")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDDefaultFieldManager)),
		"so is its replace")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager, ArgoCDDefaultFieldManager)),
		"both of its own managers in one second are still only Argo CD")
	assert.Equal(t, int64(3), collisions(t, reader, collisionWithheld))

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager, "kubectl-edit")),
		"a tie with another manager may be the edit itself")
	assert.Equal(t, int64(1), collisions(t, reader, collisionAmbiguous))
}
