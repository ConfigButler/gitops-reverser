// SPDX-License-Identifier: Apache-2.0

package git

// What happens to an apply request after the worker decided it: a refusal owed while the branch
// was busy, and a request the requester could not deliver and handed back. Both wait, and both are
// checked again when they stop waiting. These are the worker's halves of the review findings 3 and
// 4 recorded in docs/design/apply-trigger-follow-ups.md §2.3 and §2.5.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusalLoop is an event loop over refusalTouchWorker's GitTarget team-a/editing, asking for
// Reapply, whose requests are recorded.
func refusalLoop(t *testing.T) (*branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{OnRefusal: configv1alpha3.RefusalActionReapply})
	requester := &recordingRequester{}
	w.applyRequester = requester
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)
	return loop, requester
}

// updateEditing changes team-a/editing in the worker's fake client.
func updateEditing(t *testing.T, w *BranchWorker, mutate func(*configv1alpha3.GitTarget)) {
	t.Helper()
	target := &configv1alpha3.GitTarget{}
	require.NoError(t, w.Client.Get(w.ctx, client.ObjectKey{Name: "editing", Namespace: "team-a"}, target))
	mutate(target)
	require.NoError(t, w.Client.Update(w.ctx, target))
}

// TestServiceRefusals_AnOwedRefusalAsksForConsentAgain: a refusal owed while the branch was busy
// waited, behind an open window or a publication outage, for as long as those lasted, and was
// sent to a target suspended or set back to Ignore in the meantime.
func TestServiceRefusals_AnOwedRefusalAsksForConsentAgain(t *testing.T) {
	for name, withdraw := range map[string]func(*configv1alpha3.GitTarget){
		"suspended":     func(g *configv1alpha3.GitTarget) { g.Spec.Suspend = true },
		"set to Ignore": func(g *configv1alpha3.GitTarget) { g.Spec.OnRefusal = configv1alpha3.RefusalActionIgnore },
	} {
		t.Run(name, func(t *testing.T) {
			loop, requester := refusalLoop(t)
			loop.openWindow = &openWindow{}
			loop.oweReapply(editingKey(), "a base-owned field", "observation-1")
			loop.serviceAppliers()

			updateEditing(t, loop.w, withdraw)
			loop.openWindow = nil
			loop.serviceAppliers()

			assert.Zero(t, requester.refusals())
			assert.Empty(t, loop.reapplyOwed, "the obligation is spent, not kept for later")
		})
	}
}

// TestRefusalRecovered_CancelsWhatIsOwedForThatCollectionOnly: a refusal whose collection recovered
// while its request waited for a quiet branch has nothing left to revert. A sibling collection still
// refused keeps its own; the whole-target evaluation speaks for every collection.
func TestRefusalRecovered_CancelsWhatIsOwedForThatCollectionOnly(t *testing.T) {
	configMaps := refusalKeyFor(editingRef(), "configmaps")

	loop, requester := refusalLoop(t)
	loop.openWindow = &openWindow{}
	loop.oweReapply(editingKey(), "a base-owned field", "observation-1")
	loop.oweReapply(configMaps, "another base-owned field", "observation-2")

	loop.refusalRecovered(editingRef(), editingKey().collection)
	assert.Equal(t, map[refusalKey]string{configMaps: "another base-owned field"}, loop.reapplyOwed)

	loop.openWindow = nil
	loop.serviceAppliers()
	assert.Equal(t, 1, requester.refusals(), "the configmaps refusal still stands")

	loop2, requester2 := refusalLoop(t)
	loop2.openWindow = &openWindow{}
	loop2.oweReapply(editingKey(), "a base-owned field", "observation-1")
	loop2.oweReapply(configMaps, "another base-owned field", "observation-2")
	loop2.refusalRecovered(editingRef(), itypes.CollectionKey{})
	loop2.openWindow = nil
	loop2.serviceAppliers()
	assert.Zero(t, requester2.refusals())
}

// TestHandBack_IsSentAgainOnlyWhenDueAndOnAQuietBranch: a handed-back request waits out its
// backoff, then waits for a quiet branch like any other, and carries its hand-back count so the
// requester does not report the same failure twice.
func TestHandBack_IsSentAgainOnlyWhenDueAndOnAQuietBranch(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	require.Len(t, requester.taken(), 1, "the ForeignCommit, which then failed")

	alpha := itypes.NewResourceReference("alpha", "team-a")
	loop.w.debt.handBack(ApplyRequest{
		Reason: configv1alpha3.ApplyReasonForeignCommit, Commit: "bbb",
		Targets: []itypes.ResourceReference{alpha}, HandBacks: 1,
	}, time.Now())

	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not before its backoff")
	require.NotNil(t, loop.applierTimer, "the backoff is armed")

	loop.w.debt.returnedDue = time.Now().Add(-time.Second)
	loop.openWindow = &openWindow{}
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not while the branch is busy")

	loop.openWindow = nil
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, ApplyRequest{
		Branch: BranchKey{RepoNamespace: "team-a", RepoName: "test-provider", Branch: "main"},
		Reason: configv1alpha3.ApplyReasonForeignCommit, Commit: "bbb",
		Targets: []itypes.ResourceReference{alpha}, HandBacks: 1,
	}, requests[0], "only the target handed back, at the tip, with its count")
	assert.Nil(t, loop.applierTimer)
	assert.Empty(t, loop.handBackCount, "the count went with the request")
}

// TestHandBack_ARefusalIsOwedAgainOnlyWhileItStands: a Refusal handed back is owed again while the
// refusal has not recovered, and only with consent. One that recovered while its request was
// failing, and a handed-back CatchUp, are owed a catch-up, which waits for the quiet period.
func TestHandBack_ARefusalIsOwedAgainOnlyWhileItStands(t *testing.T) {
	handBack := func(loop *branchWorkerEventLoop, reason configv1alpha3.ApplyReason) {
		loop.w.debt.handBack(ApplyRequest{
			Reason: reason, Commit: "aaa", Targets: []itypes.ResourceReference{editingRef()}, HandBacks: 2,
		}, time.Now())
		loop.w.debt.returnedDue = time.Time{}
	}

	t.Run("standing", func(t *testing.T) {
		loop, requester := refusalLoop(t)
		loop.w.recordRefusalObservation(editingKey(), "observation-1")
		handBack(loop, configv1alpha3.ApplyReasonRefusal)

		loop.serviceAppliers()
		requests := requester.taken()
		require.Len(t, requests, 1)
		assert.Equal(t, configv1alpha3.ApplyReasonRefusal, requests[0].Reason)
		assert.Equal(t, 2, requests[0].HandBacks)
	})

	t.Run("standing, but consent withdrawn", func(t *testing.T) {
		loop, requester := refusalLoop(t)
		loop.w.recordRefusalObservation(editingKey(), "observation-1")
		updateEditing(t, loop.w, func(g *configv1alpha3.GitTarget) { g.Spec.Suspend = true })
		handBack(loop, configv1alpha3.ApplyReasonRefusal)

		loop.serviceAppliers()
		assert.Empty(t, requester.taken())
	})

	for _, tc := range []struct {
		name   string
		reason configv1alpha3.ApplyReason
	}{
		{"recovered", configv1alpha3.ApplyReasonRefusal},
		{"a catch-up", configv1alpha3.ApplyReasonCatchUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, requester := refusalLoop(t)
			handBack(loop, tc.reason)

			loop.serviceAppliers()
			assert.Empty(t, requester.taken(), "a catch-up waits for the quiet period")
			assert.Contains(t, loop.catchUpOwed, editingRef())

			loop.quietSince = time.Now().Add(-time.Hour)
			loop.serviceAppliers()
			requests := requester.taken()
			require.Len(t, requests, 1)
			assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, requests[0].Reason)
			assert.Equal(t, 2, requests[0].HandBacks)
		})
	}
}

// TestHandBack_ATargetThatNoLongerNamesAnApplierIsOwedNothing.
func TestHandBack_ATargetThatNoLongerNamesAnApplierIsOwedNothing(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("plain", false))
	loop.w.debt.handBack(ApplyRequest{
		Reason: configv1alpha3.ApplyReasonForeignCommit, Commit: "aaa",
		Targets: []itypes.ResourceReference{itypes.NewResourceReference("plain", "team-a")}, HandBacks: 1,
	}, time.Now())
	loop.w.debt.returnedDue = time.Time{}

	loop.serviceAppliers()

	assert.Empty(t, requester.taken())
	assert.Empty(t, loop.handBackCount)
	assert.Nil(t, loop.applierTimer)
}

// TestHandBackApply_RecordsTheDebtAndWakesTheLoop: the record is the debt; the wake only makes an
// idle loop look at it.
func TestHandBackApply_RecordsTheDebtAndWakesTheLoop(t *testing.T) {
	w := newMetricsTestWorker()
	req := ApplyRequest{
		Reason: configv1alpha3.ApplyReasonCatchUp, Commit: "aaa",
		Targets: []itypes.ResourceReference{editingRef()}, HandBacks: 1,
	}
	w.handBackApply(req)

	require.Len(t, w.eventQueue, 1)
	assert.True(t, (<-w.eventQueue).ApplyWake)
	assert.Equal(t, map[itypes.ResourceReference]configv1alpha3.ApplyReason{
		editingRef(): configv1alpha3.ApplyReasonCatchUp,
	}, w.debt.returned)

	for range cap(w.eventQueue) {
		w.eventQueue <- WorkItem{}
	}
	req.Reason = configv1alpha3.ApplyReasonRefusal
	w.handBackApply(req)
	assert.Equal(t, configv1alpha3.ApplyReasonRefusal, w.debt.returned[editingRef()],
		"a full queue drops the wake, never the request, and the stronger reason wins")
}

func TestHandBackDelay_DoublesToACap(t *testing.T) {
	for handBacks, want := range map[int]time.Duration{
		0: 15 * time.Second, 1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute,
		5: 4 * time.Minute, 6: 5 * time.Minute, 50: 5 * time.Minute,
	} {
		assert.Equal(t, want, handBackDelay(handBacks), "after %d hand-backs", handBacks)
	}
}

func TestApplyDebt_HandBackWaitsForTheLatestBackoff(t *testing.T) {
	var debt applyDebt
	now := time.Now()
	target := itypes.NewResourceReference("alpha", "team-a")
	debt.handBack(ApplyRequest{Reason: configv1alpha3.ApplyReasonCatchUp, Targets: []itypes.ResourceReference{target},
		HandBacks: 3}, now)
	debt.handBack(ApplyRequest{Reason: configv1alpha3.ApplyReasonCatchUp, Targets: []itypes.ResourceReference{target},
		HandBacks: 1}, now)

	returned, count, due := debt.takeReturned(now.Add(30 * time.Second))
	assert.Nil(t, returned)
	assert.Zero(t, count)
	assert.Equal(t, now.Add(time.Minute), due, "the longer backoff stands")

	returned, count, _ = debt.takeReturned(now.Add(time.Minute))
	assert.Len(t, returned, 1)
	assert.Equal(t, 3, count)
	returned, _, due = debt.takeReturned(now.Add(time.Hour))
	assert.Nil(t, returned)
	assert.True(t, due.IsZero(), "taken once")
}
