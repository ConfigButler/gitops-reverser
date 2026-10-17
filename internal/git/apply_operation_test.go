// SPDX-License-Identifier: Apache-2.0

package git

// An attempt that asked the applier for an apply is an operation, and the worker follows it until
// it completes: Requested, Running, Settling, then Succeeded or Failed. An operation the dispatcher
// stopped expecting to finish soon is Unresolved, never "delivered anyway", because a timer that
// closes the window does not stop the apply. A late apply the window withheld leaves the cluster on
// an older value than Git, and owes a Repair. See docs/design/suspended-appliers-plan.md.

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// TestApplier_AWithheldRollbackIsRepairedWithASecondSync is the third review's second finding. An
// Argo CD sync of replicas 2 runs, the API edits replicas to 3, and the sync's write of 2 arrives
// after it: the window withholds it, so Git keeps 3, and the cluster runs 2. An Application is never
// owed a CatchUp, so nothing used to sync again. The withheld write owes a Repair: a second sync,
// pinned to the tip that holds the edit, sent on the quiet branch with no quiet period.
func TestApplier_AWithheldRollbackIsRepairedWithASecondSync(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyRequested)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "the edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", ArgoCDFieldManager)), "the rollback is not")
	report(first[0], ApplySucceeded)
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush}) // the edit's push
	loop.serviceAppliers()

	repair := requester.taken()
	require.Len(t, repair, 1, "the rollback is repaired")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)
	assert.Equal(t, "ccc", repair[0].Commit, "pinned to the tip that holds the edit")
	assert.Equal(t, []ApplyTarget{{
		Ref: repair[0].Targets[0].Ref, UID: "uid-alpha", Reason: configv1alpha3.ApplyReasonRepair,
	}}, repair[0].Targets)

	report(repair[0], ApplySucceeded)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "a repair is owed once")
}

// TestApplier_ARepairWaitsForTheBranchNotForAQuietPeriod: a Repair is decided like a Refusal. It
// waits for a quiet branch, and for the attempt still out, but not for the quiet period; and a
// failed one comes back.
func TestApplier_ARepairWaitsForTheBranchNotForAQuietPeriod(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit"))
	require.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the attempt is still out")

	report(first[0], ApplySucceeded)
	loop.openWindow = &openWindow{}
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not while the branch is busy")

	loop.openWindow = nil
	loop.quietSince = time.Time{}
	loop.serviceAppliers()
	repair := requester.taken()
	require.Len(t, repair, 1, "the moment the branch is quiet")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)

	report(repair[0], ApplyFailed)
	loop.serviceAppliers()
	dueNow(loop)
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1, "a repair whose apply failed is owed again")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, retry[0].Reason)
}

// TestApplier_AnUnresolvedOperationKeepsItsWindowOpen is the third review's fourth finding: a
// dispatcher that stopped waiting reported the attempt delivered, and the window closed a grace
// later while the apply could still run. An Unresolved operation holds its window open and its
// applier's next attempt back, for as long as it takes, and is counted while it does.
func TestApplier_AnUnresolvedOperationKeepsItsWindowOpen(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	for _, phase := range []ApplyOutcome{ApplyRequested, ApplyRunning, ApplyUnresolved} {
		report(first[0], phase)
	}
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	state := onlyApplier(t, loop)
	require.NotNil(t, state.inflight)
	assert.Equal(t, ApplyUnresolved, state.inflight.phase)
	assert.Empty(t, requester.taken(), "what is owed meanwhile waits for the operation")
	assert.True(t, state.window.open(time.Now().Add(24*time.Hour)), "no timer closes the window")
	assert.Equal(t, int64(1), loop.w.unresolvedOperations.Load())

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", ArgoCDFieldManager)),
		"a late apply long after the budget is still withheld")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))

	report(first[0], ApplySucceeded)
	loop.serviceAppliers()
	assert.Zero(t, loop.w.unresolvedOperations.Load(), "it resolved on its token")
	next := requester.taken()
	require.Len(t, next, 1, "the next attempt goes once it completed")
	assert.Equal(t, "ccc", next[0].Commit)
}

// TestApplier_AnApplierNoLongerNamedAbandonsItsOperation: an Unresolved operation also resolves when
// no GitTarget names its applier any more. The worker stops following it, and tells the requester.
func TestApplier_AnApplierNoLongerNamedAbandonsItsOperation(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyUnresolved)
	loop.serviceAppliers()
	require.Equal(t, int64(1), loop.w.unresolvedOperations.Load())

	updateTarget(t, loop, "alpha", func(g *configv1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = nil })
	owesForeign(loop.w)
	loop.serviceAppliers()

	assert.Empty(t, loop.appliers)
	assert.Zero(t, loop.w.unresolvedOperations.Load())
	select {
	case <-first[0].Abandoned:
	default:
		t.Fatal("the requester must be told to stop following the operation")
	}
}

// TestApplier_AFailedOperationIsRetriedOnItsBackoff: an apply the applier ran and failed gives back
// what it carried, like an attempt that could not ask, and keeps its window for the grace, since it
// may have written part of the commit.
func TestApplier_AFailedOperationIsRetriedOnItsBackoff(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	report(first[0], ApplyRequested)
	report(first[0], ApplyFailed)
	loop.serviceAppliers()
	state := onlyApplier(t, loop)
	assert.Equal(t, 1, state.failures)
	assert.True(t, state.window.open(time.Now()), "within the grace")
	assert.Empty(t, requester.taken(), "not before its backoff")

	dueNow(loop)
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, retry[0].Reason)
	assert.Equal(t, 1, retry[0].PriorFailures)
}

// onlyApplier is the one applier the loop tracks.
func onlyApplier(t *testing.T, loop *branchWorkerEventLoop) *applierState {
	t.Helper()
	require.Len(t, loop.appliers, 1)
	for _, state := range loop.appliers {
		return state
	}
	return nil
}

// recordingStatusWriter records what a worker publishes on its triggers' status, in order.
type recordingStatusWriter struct {
	mu        sync.Mutex
	published map[TriggerRef][]ApplyTriggerStatus
}

func (r *recordingStatusWriter) PublishApplyTriggerStatus(trigger TriggerRef, status ApplyTriggerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.published == nil {
		r.published = map[TriggerRef][]ApplyTriggerStatus{}
	}
	r.published[trigger] = append(r.published[trigger], status)
}

// last is the last publication for trigger, and how many there were.
func (r *recordingStatusWriter) last(trigger TriggerRef) (ApplyTriggerStatus, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.published[trigger]
	if len(all) == 0 {
		return ApplyTriggerStatus{}, 0
	}
	return all[len(all)-1], len(all)
}

// TestApplier_TheTriggerStatusFollowsEveryTransition: the operation is on the trigger's status from
// the moment the applier was asked, and moves with every report, so `kubectl get fluxtriggers`
// shows where an apply stands without anything having to reconcile. A worker that has started
// nothing says nothing, and Unresolved holds until the operation completes.
func TestApplier_TheTriggerStatusFollowsEveryTransition(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	writer := &recordingStatusWriter{}
	loop.w.triggerStatus = writer
	trigger := applierRef("alpha").Trigger

	owesForeign(loop.w)
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	_, n := writer.last(trigger)
	assert.Zero(t, n, "nothing is published before the applier was asked")

	phase := func(outcome ApplyOutcome) *configv1alpha3.ApplyOperation {
		t.Helper()
		sent[0].Report(ApplyResult{ID: sent[0].ID, Outcome: outcome, Token: "T1"})
		loop.serviceAppliers()
		status, _ := writer.last(trigger)
		require.NotNil(t, status.Operation)
		return status.Operation
	}

	requested := phase(ApplyRequested)
	assert.Equal(t, configv1alpha3.ApplyOperationRequested, requested.Phase)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requested.Reason)
	assert.Equal(t, "bbb", requested.Commit)
	assert.Equal(t, "T1", requested.Token)
	assert.False(t, requested.RequestedAt.IsZero())
	assert.Nil(t, requested.FinishedAt)

	assert.Equal(t, configv1alpha3.ApplyOperationRunning, phase(ApplyRunning).Phase)
	assert.Equal(t, configv1alpha3.ApplyOperationUnresolved, phase(ApplyUnresolved).Phase)
	assert.Equal(t, configv1alpha3.ApplyOperationUnresolved, phase(ApplyRunning).Phase,
		"Unresolved holds until the operation completes")

	done := phase(ApplySucceeded)
	assert.Equal(t, configv1alpha3.ApplyOperationSucceeded, done.Phase)
	require.NotNil(t, done.FinishedAt)
	assert.Equal(t, requested.RequestedAt, done.RequestedAt, "one operation, one record")

	_, before := writer.last(trigger)
	loop.serviceAppliers()
	_, after := writer.last(trigger)
	assert.Equal(t, before, after, "an unchanged status is not published again")
}

// TestApplier_TheTriggerStatusSaysARepairIsOwed: repairOwed is published from the withheld write
// until an operation that carried the repair succeeded.
func TestApplier_TheTriggerStatusSaysARepairIsOwed(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	writer := &recordingStatusWriter{}
	loop.w.triggerStatus = writer
	trigger := applierRef("alpha").Trigger

	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyRequested)
	loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit"))
	require.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	loop.serviceAppliers()
	status, _ := writer.last(trigger)
	assert.True(t, status.RepairOwed, "the withheld write owes a repair")

	report(first[0], ApplySucceeded)
	loop.serviceAppliers()
	repair := requester.taken()
	require.Len(t, repair, 1)
	status, _ = writer.last(trigger)
	assert.True(t, status.RepairOwed, "still owed while the repair is out")

	report(repair[0], ApplyRequested)
	report(repair[0], ApplySucceeded)
	loop.serviceAppliers()
	status, _ = writer.last(trigger)
	assert.False(t, status.RepairOwed, "repaired")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, status.Operation.Reason)
}
