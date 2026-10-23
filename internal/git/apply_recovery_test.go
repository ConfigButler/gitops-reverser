// SPDX-License-Identifier: Apache-2.0

package git

// Restart recovery (docs/design/apply-trigger.md, "Restart recovery"): the applier claim's
// status.operation is the durable record of an apply in flight, written before the apply could
// start, and a worker that starts follows what it finds there, with the apply window open, before
// it mirrors a live event.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// recording gives an applier claim a recorded status.
func recording(operation *configv1alpha3.ApplyOperation, repairPending bool) func(*configv1alpha3.FluxClaim) {
	return func(applierClaim *configv1alpha3.FluxClaim) {
		applierClaim.Status.Operation, applierClaim.Status.RepairPending = operation, repairPending
	}
}

// recordedOperation is a worker's Refusal operation at commit "bbb", with token T1.
func recordedOperation(phase configv1alpha3.ApplyOperationPhase) *configv1alpha3.ApplyOperation {
	return &configv1alpha3.ApplyOperation{
		Phase: phase, Reason: configv1alpha3.ApplyReasonRefusal, Commit: "bbb", Token: "T1",
		RequestedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
	}
}

// TestRecovery_AnOperationLeftInFlightIsFollowedWithItsWindowOpen: the process stopped while an apply
// ran. The new one follows that operation by its recorded token, asks the applier for nothing, and
// holds the apply window open from the start, so the apply's late write over an edit made after the
// restart is withheld.
func TestRecovery_AnOperationLeftInFlightIsFollowedWithItsWindowOpen(t *testing.T) {
	recorded := recordedOperation(configv1alpha3.ApplyOperationRunning)
	loop, requester := manualLoop(t, applierTestTarget("alpha", true),
		fluxClaim("alpha", recording(recorded, false)))
	writer := &recordingStatusWriter{}
	loop.w.claimStatus = writer
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch}) // the tip it applies

	loop.followRecordedOperations(time.Now())

	followed := requester.taken()
	require.Len(t, followed, 1, "the recorded operation is followed")
	require.NotNil(t, followed[0].Recovered)
	assert.Equal(t, "T1", followed[0].Recovered.Token)
	assert.Equal(t, "T1", followed[0].Token)
	assert.Equal(t, configv1alpha3.ApplyReasonRefusal, followed[0].Reason)
	assert.Equal(t, "bbb", followed[0].Commit)
	assert.Nil(t, followed[0].Record, "nothing is asked, so nothing is recorded first")
	require.True(t, onlyApplier(t, loop).window.open(time.Now()))

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "the edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the recovered apply's late write is not")

	followed[0].Report(ApplyResult{ID: followed[0].ID, Outcome: ApplySettling, SuspendToken: "T2"})
	followed[0].Report(ApplyResult{ID: followed[0].ID, Outcome: ApplySucceeded, SuspendToken: "T2",
		AppliedRevision: "main@sha1:bbb"})
	holdApplies(loop) // the repair the withheld write left pending waits
	loop.serviceAppliers()
	status, _ := writer.last(applierRef("alpha").ApplierClaim)
	completed := status.Operation
	require.NotNil(t, completed)
	assert.Equal(t, configv1alpha3.ApplyOperationSucceeded, completed.Phase, "the record is completed")
	assert.Equal(t, "T1", completed.Token, "one operation, one record")
	assert.Equal(t, recorded.RequestedAt.Unix(), completed.RequestedAt.Unix())
	assert.Equal(t, "T2", completed.SuspendToken)
	assert.Empty(t, requester.taken())

	releaseApplies(loop)
	loop.serviceAppliers()
	repair := requester.taken()
	require.Len(t, repair, 1, "the withheld write leaves a repair pending, as in a process that never stopped")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)
}

// TestRecovery_APendingRepairIsRestored: repairPending was recorded because a late apply was withheld;
// the cluster still runs the older value, so the new process sends the repair.
func TestRecovery_APendingRepairIsRestored(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true),
		fluxClaim("alpha", recording(recordedOperation(configv1alpha3.ApplyOperationSucceeded), true)))
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch}) // the tip it applied
	loop.followRecordedOperations(time.Now())
	assert.True(t, onlyApplier(t, loop).repair)
	loop.serviceAppliers()
	repair := requester.taken()
	require.Len(t, repair, 1, "the repair is sent after a flush")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)
	assert.Nil(t, repair[0].Recovered)
}

// TestRecovery_OnlyAnOperationInFlightIsFollowed: a completed record is history, and asks for
// nothing; a worker with no applier claim record says nothing.
func TestRecovery_OnlyAnOperationInFlightIsFollowed(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true),
		fluxClaim("alpha", recording(recordedOperation(configv1alpha3.ApplyOperationSucceeded), false)))
	loop.followRecordedOperations(time.Now())
	assert.Empty(t, requester.taken())
	assert.Nil(t, onlyApplier(t, loop).inflight)
}

// TestRecovery_ARecordIsReadOnceButAnApplyStartedElsewhereWhenever: a worker record is read back
// once, when this process first lists the applier, because afterwards the cache can lag the worker's
// own writes. The applier claim's reconciler records an apply somebody else started (no request token)
// at any time, and the worker follows each such record once, so its window opens for it.
func TestRecovery_ARecordIsReadOnceButAnApplyStartedElsewhereWhenever(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	loop.followRecordedOperations(time.Now())
	require.Empty(t, requester.taken())

	setRecorded := func(operation *configv1alpha3.ApplyOperation) {
		t.Helper()
		applierClaim := &configv1alpha3.FluxClaim{}
		require.NoError(
			t,
			loop.w.Client.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "alpha"}, applierClaim),
		)
		applierClaim.Status.Operation = operation
		require.NoError(t, loop.w.Client.Status().Update(t.Context(), applierClaim))
	}
	setRecorded(recordedOperation(configv1alpha3.ApplyOperationRunning))
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "a worker's own record is not read back while it runs")

	external := &configv1alpha3.ApplyOperation{
		Phase: configv1alpha3.ApplyOperationUnresolved, SuspendToken: "F1", RequestedAt: metav1.Now(),
	}
	setRecorded(external)
	loop.serviceAppliers()
	followed := requester.taken()
	require.Len(t, followed, 1, "an apply started elsewhere is followed")
	assert.Equal(t, "F1", followed[0].Recovered.SuspendToken)
	assert.Empty(t, followed[0].Token)

	followed[0].Report(ApplyResult{ID: followed[0].ID, Outcome: ApplySucceeded, SuspendToken: "F1"})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "once")
}
