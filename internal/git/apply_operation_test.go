// SPDX-License-Identifier: Apache-2.0

package git

// An attempt that asked the applier for an apply is an operation, and the worker follows it until
// it completes: Requested, Running, Settling, then Succeeded or Failed. An operation the dispatcher
// stopped expecting to finish soon is Unresolved, never "delivered anyway", because a timer that
// closes the window does not stop the apply. A late apply the window withheld leaves the cluster on
// an older value than Git, and leaves a Repair pending. See docs/design/apply-trigger.md.

import (
	"context"
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
// sent a CatchUp, so nothing used to sync again. The withheld write leaves a Repair pending: a
// second sync, pinned to the tip that holds the edit, sent on the quiet branch with no quiet
// period.
func TestApplier_AWithheldRollbackIsRepairedWithASecondSync(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyRequested)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "the edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", ArgoCDFieldManager)), "the rollback is not")
	report(first[0], ApplySucceeded)
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush}) // the edit's push
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
	assert.Empty(t, requester.taken(), "a repair is sent once")
}

// TestApplier_ARepairWaitsForTheBranchNotForAQuietPeriod: a Repair is decided like a Refusal. It
// waits for a quiet branch, and for the attempt still out, but not for the quiet period; and a
// failed one comes back.
func TestApplier_ARepairWaitsForTheBranchNotForAQuietPeriod(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
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
	require.Len(t, retry, 1, "a repair whose apply failed is pending again")
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
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	for _, phase := range []ApplyOutcome{ApplyRequested, ApplyRunning, ApplyUnresolved} {
		report(first[0], phase)
	}
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	state := onlyApplier(t, loop)
	require.NotNil(t, state.inflight)
	assert.Equal(t, ApplyUnresolved, state.inflight.phase)
	assert.Empty(t, requester.taken(), "a reason that becomes pending meanwhile waits for the operation")
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
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyUnresolved)
	loop.serviceAppliers()
	require.Equal(t, int64(1), loop.w.unresolvedOperations.Load())

	updateTarget(t, loop, "alpha", func(g *configv1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = nil })
	observeForeignCommit(loop.w)
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
	observeForeignCommit(loop.w)
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

// recordingStatusWriter records what a worker publishes on its triggers' status, in order, and the
// operations the requester recorded.
type recordingStatusWriter struct {
	mu        sync.Mutex
	published map[TriggerRef][]ApplyTriggerStatus
	recorded  map[TriggerRef][]*configv1alpha3.ApplyOperation
}

func (r *recordingStatusWriter) RecordApplyOperation(
	_ context.Context, trigger TriggerRef, operation *configv1alpha3.ApplyOperation,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == nil {
		r.recorded = map[TriggerRef][]*configv1alpha3.ApplyOperation{}
	}
	r.recorded[trigger] = append(r.recorded[trigger], operation)
	return nil
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

// records are the operations recorded on trigger, in order.
func (r *recordingStatusWriter) records(trigger TriggerRef) []*configv1alpha3.ApplyOperation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorded[trigger]
}

// TestApplier_TheTriggerStatusFollowsEveryTransition: the operation reaches the trigger's status
// first through the request's Record, which the requester writes through before it applies (so a
// restart finds every apply that may have started), and moves with every report, so `kubectl get
// fluxtriggers` shows where an apply stands without anything having to reconcile. Unresolved holds
// until the operation completes.
func TestApplier_TheTriggerStatusFollowsEveryTransition(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	writer := &recordingStatusWriter{}
	loop.w.triggerStatus = writer
	trigger := applierRef("alpha").Trigger

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	_, n := writer.last(trigger)
	require.Zero(t, n, "nothing is published as the attempt is sent")
	require.NotNil(t, sent[0].Record)
	require.NoError(t, sent[0].Record(context.Background()))
	records := writer.records(trigger)
	require.Len(t, records, 1, "the request writes it through before it applies")
	requested := records[0]
	assert.Equal(t, configv1alpha3.ApplyOperationRequested, requested.Phase)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requested.Reason)
	assert.Equal(t, "bbb", requested.Commit)
	assert.NotEmpty(t, requested.Token)
	assert.Equal(t, requested.Token, sent[0].Token, "the request carries the token it is recorded with")
	assert.False(t, requested.RequestedAt.IsZero())
	assert.Nil(t, requested.FinishedAt)

	phase := func(res ApplyResult) *configv1alpha3.ApplyOperation {
		t.Helper()
		res.ID = sent[0].ID
		sent[0].Report(res)
		loop.serviceAppliers()
		status, _ := writer.last(trigger)
		require.NotNil(t, status.Operation)
		assert.Nil(t, status.Withdrawn)
		return status.Operation
	}

	assert.Equal(t, requested, phase(ApplyResult{Outcome: ApplyRequested}), "published once reported")
	assert.Equal(t, configv1alpha3.ApplyOperationRunning, phase(ApplyResult{Outcome: ApplyRunning}).Phase)
	assert.Equal(t, configv1alpha3.ApplyOperationUnresolved, phase(ApplyResult{Outcome: ApplyUnresolved}).Phase)
	settling := phase(ApplyResult{Outcome: ApplySettling, SuspendToken: "T2"})
	assert.Equal(t, configv1alpha3.ApplyOperationUnresolved, settling.Phase,
		"Unresolved holds until the operation completes")
	assert.Equal(t, "T2", settling.SuspendToken)

	done := phase(ApplyResult{Outcome: ApplySucceeded, SuspendToken: "T2", AppliedRevision: "main@sha1:bbb"})
	assert.Equal(t, configv1alpha3.ApplyOperationSucceeded, done.Phase)
	require.NotNil(t, done.FinishedAt)
	assert.Equal(t, "main@sha1:bbb", done.AppliedRevision)
	assert.Equal(t, requested.RequestedAt, done.RequestedAt, "one operation, one record")
	assert.Equal(t, requested.Token, done.Token)

	_, before := writer.last(trigger)
	loop.serviceAppliers()
	_, after := writer.last(trigger)
	assert.Equal(t, before, after, "an unchanged status is not published again")
}

// TestApplier_AnAttemptEndedBeforeItsRecordLeavesTheTriggerAlone is the fourth review's first
// finding: the Requested operation was published as the attempt was sent, so an attempt the
// requester refused (an Argo CD Application with automated sync on: Rejected) left a Requested
// operation on the trigger that nothing would ever complete. Whatever ends an attempt before its
// Record, the trigger is told nothing about it.
func TestApplier_AnAttemptEndedBeforeItsRecordLeavesTheTriggerAlone(t *testing.T) {
	for _, outcome := range []ApplyOutcome{
		ApplyRejected, ApplyDropped, ApplyStale, ApplyRequestFailed, ApplyCurrent,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
			writer := &recordingStatusWriter{}
			loop.w.triggerStatus = writer
			trigger := argoApplierRef("alpha").Trigger

			observeForeignCommit(loop.w)
			loop.serviceAppliers()
			first := requester.taken()
			require.Len(t, first, 1)
			report(first[0], ApplyRequested)
			report(first[0], ApplySucceeded)
			loop.serviceAppliers()
			completed, published := writer.last(trigger)
			require.Equal(t, configv1alpha3.ApplyOperationSucceeded, completed.Operation.Phase)

			observeForeignCommit(loop.w)
			loop.serviceAppliers()
			second := requester.taken()
			require.Len(t, second, 1)
			report(second[0], outcome)
			loop.serviceAppliers()

			_, after := writer.last(trigger)
			assert.Equal(t, published, after, "nothing is published for an attempt that was never recorded")
			assert.Empty(t, writer.records(trigger))
			assert.Equal(t, completed.Operation, onlyApplier(t, loop).shownOperation(),
				"the last operation stands")
		})
	}
}

// TestApplier_AnAttemptEndedAfterItsRecordWithdrawsIt: the requester recorded the operation, and
// then could not start the apply (the resume failed). The operation is withdrawn, so the trigger
// shows the one before it again, and a restart does not follow an apply that never started.
func TestApplier_AnAttemptEndedAfterItsRecordWithdrawsIt(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	writer := &recordingStatusWriter{}
	loop.w.triggerStatus = writer
	trigger := applierRef("alpha").Trigger

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyRequested)
	report(first[0], ApplySucceeded)
	loop.serviceAppliers()
	completed, _ := writer.last(trigger)

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	second := requester.taken()
	require.Len(t, second, 1)
	require.NoError(t, second[0].Record(context.Background()))
	recorded := writer.records(trigger)
	require.Len(t, recorded, 1)
	report(second[0], ApplyRequestFailed)
	loop.serviceAppliers()

	withdrawal, n := writer.last(trigger)
	assert.Equal(t, recorded[0], withdrawal.Withdrawn, "the recorded operation is withdrawn")
	assert.Equal(t, completed.Operation, withdrawal.Operation, "and the one before it shown again")

	loop.serviceAppliers()
	_, again := writer.last(trigger)
	assert.Equal(t, n, again, "withdrawn once")
}

// TestApplier_TheTriggerStatusSaysARepairIsPending: repairPending is published from the withheld write
// until an operation that carried the repair succeeded.
func TestApplier_TheTriggerStatusSaysARepairIsPending(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	writer := &recordingStatusWriter{}
	loop.w.triggerStatus = writer
	trigger := applierRef("alpha").Trigger

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyRequested)
	loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit"))
	require.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	loop.serviceAppliers()
	status, _ := writer.last(trigger)
	assert.True(t, status.RepairPending, "the withheld write leaves a repair pending")

	report(first[0], ApplySucceeded)
	loop.serviceAppliers()
	repair := requester.taken()
	require.Len(t, repair, 1)
	status, _ = writer.last(trigger)
	assert.True(t, status.RepairPending, "still pending while the repair is out")

	report(repair[0], ApplyRequested)
	report(repair[0], ApplySucceeded)
	loop.serviceAppliers()
	status, _ = writer.last(trigger)
	assert.False(t, status.RepairPending, "repaired")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, status.Operation.Reason)
}
