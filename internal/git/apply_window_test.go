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

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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
	report(req, ApplySucceeded)
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
	for _, outcome := range []ApplyOutcome{ApplyStale, ApplyRequestFailed, ApplyRejected, ApplyDropped, ApplyCurrent} {
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

// TestApplyWindow_AFirstTieIsRememberedAsAnEdit: a write by the applier and another manager in the
// same second may be the API edit, so it is mirrored. It must also be remembered as one: when the
// applier then writes the object alone a second later, that is its apply reverting the edit, and is
// withheld. A tie on an object nobody edited before is not a collision, so it is not counted.
func TestApplyWindow_AFirstTieIsRememberedAsAnEdit(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager, "kubectl-edit")),
		"a tie may be the API edit itself")
	assert.Equal(t, int64(0), collisions(t, reader, collisionAmbiguous),
		"nothing was edited before it, so it collides with nothing")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the applier's write a second later is its apply over the tie's edit")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
}

// completedThenNext is a loop whose applier, alpha's, completed attempt A over an API edit made
// while it ran, and was sent attempt B within A's grace: a push somebody else made owes it again.
func completedThenNext(t *testing.T) (*branchWorkerEventLoop, *recordingRequester, ApplyRequest) {
	t.Helper()
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	require.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "the edit is mirrored")

	report(first[0], ApplySucceeded)
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	next := requester.taken()
	require.Len(t, next, 1, "B goes out within A's grace")
	require.NotEqual(t, first[0].ID, next[0].ID)
	return loop, requester, next[0]
}

// TestApplyWindow_ANewAttemptKeepsTheGraceOfTheOneBefore is the fifth review's third finding. A new
// attempt replaced its applier's window, and the edits the one before had seen went with it, while
// that earlier apply's writes could still arrive within its grace. Its late write over the edit was
// then mirrored, and the revert reached Git as the applier's commit.
func TestApplyWindow_ANewAttemptKeepsTheGraceOfTheOneBefore(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _, _ := completedThenNext(t)

	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"A's late write over the edit is withheld while B is out")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
	assert.True(t, onlyApplier(t, loop).repair, "and owes a Repair")
}

// TestApplyWindow_AnAttemptThatAppliedNothingKeepsTheGraceBeforeIt: B ending without an apply has no
// window of its own, but A's grace still runs, so A's late write is withheld until it ends, and
// mirrored after.
func TestApplyWindow_AnAttemptThatAppliedNothingKeepsTheGraceBeforeIt(t *testing.T) {
	for _, outcome := range []ApplyOutcome{ApplyCurrent, ApplyStale} {
		t.Run(string(outcome), func(t *testing.T) {
			loop, _, next := completedThenNext(t)
			report(next, outcome)
			loop.openWindow = &openWindow{} // a busy branch: a stale B is not sent again yet
			loop.serviceAppliers()
			state := onlyApplier(t, loop)
			require.Nil(t, state.inflight)

			assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
				"A's late write is withheld within A's grace")
			assert.True(t, state.repair)

			state.window.closesAt = time.Now().Add(-time.Millisecond)
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "after it")
		})
	}
}

// TestApplyWindow_TheCarriedGraceEndsWhenTheEarlierWindowWouldHave: the edits a new attempt carries
// from the window before it count only until that window would have closed. After that, the
// applier's write is B's apply of a commit that holds the edit, and is mirrored; an edit made since
// B was sent still counts.
func TestApplyWindow_TheCarriedGraceEndsWhenTheEarlierWindowWouldHave(t *testing.T) {
	loop, _, _ := completedThenNext(t)
	window := onlyApplier(t, loop).window
	require.False(t, window.carriedUntil.IsZero())
	window.carriedUntil = time.Now().Add(-time.Millisecond)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager)),
		"A's grace is over: the applier's write is B's apply")
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "4", "kubectl-edit")))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager)),
		"an edit made while B runs is B's own")
}

// TestApplyWindow_ConsecutiveOperationsOnOneApplier composes the steps: A completes, B is sent within
// A's grace, A's late write over the edit is withheld, B completes, and the Repair that write owes
// goes out once, on the quiet branch, at the tip. Each window closes a grace after its own attempt.
func TestApplyWindow_ConsecutiveOperationsOnOneApplier(t *testing.T) {
	loop, requester, second := completedThenNext(t)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, second.Reason)

	require.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the Repair waits for B")

	report(second, ApplySucceeded)
	loop.serviceAppliers()
	state := onlyApplier(t, loop)
	repair := requester.taken()
	require.Len(t, repair, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)
	assert.Equal(t, "ccc", repair[0].Commit)
	assert.WithinDuration(t, time.Now().Add(applyWindowGrace), state.window.carriedUntil, time.Second,
		"the Repair carries B's grace, which ends a grace after B completed")

	report(repair[0], ApplySucceeded)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the Repair is owed once")
	assert.False(t, state.repairOwed())
	assert.WithinDuration(t, time.Now().Add(applyWindowGrace), state.window.closesAt, time.Second)
}
