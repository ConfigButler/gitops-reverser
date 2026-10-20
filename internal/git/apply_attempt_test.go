// SPDX-License-Identifier: Apache-2.0

package git

// What happens to an apply attempt after the worker decided it, and how one worker serves the
// many GitTargets and appliers on its branch. The worker keeps every pending reason until an
// attempt delivers it; an attempt that does not deliver gives back only what still stands, and its retry is
// decided again like a first attempt. See docs/design/apply-trigger.md.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// manualLoop is applyTriggerLoop with a requester that reports nothing until the test does.
func manualLoop(t *testing.T, targets ...client.Object) (*branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	loop, requester := applyTriggerLoop(t, targets...)
	requester.manual = true
	return loop, requester
}

// report delivers the requester's report of an attempt, as the requester's goroutine would.
func report(req ApplyRequest, outcome ApplyOutcome) {
	req.Report(ApplyResult{ID: req.ID, Outcome: outcome})
}

// dueNow makes every applier's backoff run out.
func dueNow(loop *branchWorkerEventLoop) {
	for _, state := range loop.appliers {
		state.retryAt = time.Time{}
	}
}

// updateTarget changes a GitTarget in the loop's fake client.
func updateTarget(t *testing.T, loop *branchWorkerEventLoop, name string, mutate func(*configv1alpha3.GitTarget)) {
	t.Helper()
	target := &configv1alpha3.GitTarget{}
	require.NoError(
		t,
		loop.w.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "team-a"}, target),
	)
	mutate(target)
	require.NoError(t, loop.w.Client.Update(context.Background(), target))
}

// reapplying makes a GitTarget consent to a Refusal request.
func reapplying(g *configv1alpha3.GitTarget) { g.Spec.OnRefusal = configv1alpha3.RefusalActionReapply }

// addAlphaUnsentRefusal decides a Refusal request for alpha's applier, for a standing refusal of its
// deployments.
func addAlphaUnsentRefusal(loop *branchWorkerEventLoop) refusalKey {
	key := refusalKeyFor(itypes.NewResourceReference("alpha", "team-a"), "deployments")
	loop.addUnsentRefusal(key, unsentRefusal{detail: "a base-owned field", uid: "uid-alpha"}, "obs-alpha")
	return key
}

// TestApplier_ARetryIsDecidedAgain: an attempt that failed is not replayed blind. It waits out its
// backoff, then is decided like a first attempt, after a flush, and names the tip as it is then.
func TestApplier_ARetryIsDecidedAgain(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	report(first[0], ApplyRequestFailed)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not before its backoff")
	assert.NotNil(t, loop.applierTimer, "the backoff is armed")

	dueNow(loop)
	holdApplies(loop)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not while a save holds the flush")

	releaseApplies(loop)
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, retry[0].Reason, "the pending ForeignCommit came back")
	assert.Equal(t, "ccc", retry[0].Commit, "at the tip as it is now")
	assert.Equal(t, 1, retry[0].PriorFailures, "so the requester does not report the failure twice")
}

// TestApplier_WithdrawnConsentNeverBecomesAnImmediateApply is the second review's first finding.
// The requester used to turn a Refusal whose target withdrew consent into a CatchUp and send it at
// once, skipping the quiet period, so a behind artifact was fetched over an unpublished edit. Now
// the requester only reports that the reason no longer holds, and the worker decides: no Refusal,
// only the converge the first naming left pending, as a CatchUp.
func TestApplier_WithdrawnConsentNeverBecomesAnImmediateApply(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying))
	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch}) // leaves a converge pending
	addAlphaUnsentRefusal(loop)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	require.Equal(t, configv1alpha3.ApplyReasonRefusal, out[0].Reason)

	updateTarget(
		t,
		loop,
		"alpha",
		func(g *configv1alpha3.GitTarget) { g.Spec.OnRefusal = configv1alpha3.RefusalActionIgnore },
	)
	report(out[0], ApplyDropped)
	loop.serviceAppliers()
	dueNow(loop)
	loop.serviceAppliers()

	next := requester.taken()
	require.Len(t, next, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, next[0].Reason, "the converge, never the Refusal")
	assert.Empty(t, loop.unsentRefusals, "the Refusal is gone with the consent")
}

// TestApplier_ARecoveredRefusalIsNotRetried is the second review's second finding: a refusal that
// recovered while its attempt was out came back with the failed attempt and was requested again.
func TestApplier_ARecoveredRefusalIsNotRetried(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying),
		fluxTrigger("alpha", recording(recordedOperation(configv1alpha3.ApplyOperationSucceeded), false)))
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch}) // the tip it applied
	key := addAlphaUnsentRefusal(loop)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	loop.refusalRecovered(key.target, key.collection)
	report(out[0], ApplyRequestFailed)
	loop.serviceAppliers()
	dueNow(loop)
	loop.serviceAppliers()

	assert.Empty(t, requester.taken(), "a recovered refusal has nothing left to ask for")
	assert.Empty(t, loop.unsentRefusals)
}

// TestApplier_ARecreatedTargetInheritsNothing is the second review's third finding: a request
// decided for one incarnation of a GitTarget executed for another created under the same name.
// Requests carry the uid, and the worker drops what was pending for an incarnation that is gone.
func TestApplier_ARecreatedTargetInheritsNothing(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying))
	loop.openWindow = &openWindow{}
	addAlphaUnsentRefusal(loop)

	old := &configv1alpha3.GitTarget{}
	require.NoError(
		t,
		loop.w.Client.Get(context.Background(), client.ObjectKey{Name: "alpha", Namespace: "team-a"}, old),
	)
	require.NoError(t, loop.w.Client.Delete(context.Background(), old))
	recreated := applierTestTarget("alpha", true, reapplying)
	recreated.UID = "uid-alpha-recreated"
	require.NoError(t, loop.w.Client.Create(context.Background(), recreated))

	loop.openWindow = nil
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the refusal was the old incarnation's")
	assert.Empty(t, loop.unsentRefusals)

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	assert.Equal(t, k8stypes.UID("uid-alpha-recreated"), out[0].Targets[0].UID,
		"an attempt names the incarnation it was decided for")
}

// TestApplier_EachApplierRetriesOnItsOwnClock is the second review's fourth finding: hand-backs
// shared one branch-wide deadline and count, so an applier that kept failing held back one that had
// recovered.
func TestApplier_EachApplierRetriesOnItsOwnClock(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", true))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 2)

	loop.appliers[applierRef("bravo")].failures = 6 // a long run of failures behind it
	for _, req := range out {
		report(req, ApplyRequestFailed)
	}
	loop.serviceAppliers()
	assert.Equal(t, 1, loop.appliers[applierRef("alpha")].failures)
	assert.Equal(t, 7, loop.appliers[applierRef("bravo")].failures)

	loop.appliers[applierRef("alpha")].retryAt = time.Now().Add(-time.Second)
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1, "alpha is due; bravo's longer backoff is bravo's alone")
	assert.Equal(t, applierRef("alpha"), retry[0].Applier)
}

// TestApplier_OneAttemptForEveryTargetNamingIt: GitTargets that share a trigger are asked for
// together, each with its own reason, because its apply reaches every one of their folders.
func TestApplier_OneAttemptForEveryTargetNamingIt(t *testing.T) {
	shared := func(g *configv1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = testApplier("shared") }
	loop, requester := applyTriggerLoop(t,
		applierTestTarget("alpha", true, shared, reapplying),
		applierTestTarget("bravo", true, shared),
		fluxTrigger("shared"),
	)
	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush}) // the startup converge
	addAlphaUnsentRefusal(loop)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1, "one attempt for the shared applier")
	assert.Equal(t, configv1alpha3.ApplyReasonRefusal, out[0].Reason)
	assert.Equal(t, []ApplyTarget{
		{
			Ref:    itypes.NewResourceReference("alpha", "team-a"),
			UID:    "uid-alpha",
			Reason: configv1alpha3.ApplyReasonRefusal,
		},
		{
			Ref:    itypes.NewResourceReference("bravo", "team-a"),
			UID:    "uid-bravo",
			Reason: configv1alpha3.ApplyReasonCatchUp,
		},
	}, out[0].Targets, "a Refusal applies the tip, so it answers the pending converge as well")
	assert.False(t, loop.appliers[applierRef("shared")].converge)
}

// TestApplier_AnAttemptOutHoldsTheNext: one attempt per applier at a time. A reason that becomes pending
// while it is out waits for its report, and goes in the attempt after.
func TestApplier_AnAttemptOutHoldsTheNext(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the first attempt is still out")

	report(out[0], ApplySucceeded)
	loop.serviceAppliers()
	next := requester.taken()
	require.Len(t, next, 1)
	assert.Equal(t, "ccc", next[0].Commit)
	assert.Zero(t, next[0].PriorFailures)
}

// TestApplier_ATargetThatNoLongerNamesAnApplierHasNothingPending: a report for an applier nobody names
// any more is dropped with it.
func TestApplier_ATargetThatNoLongerNamesAnApplierHasNothingPending(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	updateTarget(t, loop, "alpha", func(g *configv1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = nil })
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	loop.serviceAppliers()
	report(out[0], ApplyRequestFailed)
	loop.serviceAppliers()

	assert.Empty(t, loop.appliers)
	assert.Empty(t, requester.taken())
	assert.Nil(t, loop.applierTimer)
}

// answerFlush is a sibling's wake answering the flush another worker asked of it, with no writes of
// its own to publish beyond what the test put in its pending writes.
func answerFlush(loop *branchWorkerEventLoop) {
	loop.takeSiblingFlush()
	loop.reportSiblingFlush()
}

// TestSiblings_AnApplyWaitsForItsSiblingsFlush: two GitProviders naming one repository give one
// branch two workers, whose GitTargets can name one trigger. An apply sent while a sibling holds an
// edit not yet published would revert it on the cluster, and the sibling, with no apply window, would
// mirror the revert. So the flush covers the siblings: the worker waits until each has published the
// writes it decided before it was asked, and a write decided after does not hold the send.
func TestSiblings_AnApplyWaitsForItsSiblingsFlush(t *testing.T) {
	first, firstRequests, second, _ := siblingLoops(t)
	second.decisions = 1
	second.pendingWrites = []PendingWrite{{decision: 1}}

	first.serviceAppliers()
	assert.Empty(t, firstRequests.taken(), "the sibling has not flushed yet")
	require.Positive(t, second.w.flushAsked.Load(), "it was asked to")

	answerFlush(second)
	assert.Equal(t, uint64(1), second.siblingFlushMark, "the mark is the last write it decided")
	assert.True(t, second.publishesAtOnce(), "what the mark covers is pushed at once, cooldown or not")
	first.serviceAppliers()
	assert.Empty(t, firstRequests.taken(), "its write up to the mark is not published yet")

	second.decisions = 2
	second.pendingWrites = []PendingWrite{{decision: 2}} // published the first, decided another
	second.reportSiblingFlush()
	select {
	case <-first.w.applyWake:
	default:
		t.Fatal("a sibling done flushing must wake the worker waiting for it")
	}
	first.serviceAppliers()
	assert.Len(t, firstRequests.taken(), 1, "a write decided after the mark does not hold the send")
}

// TestSiblings_ShareWhatTheyObserve: a sibling's push moves the branch with content already live,
// as a worker's own push does, so it moves the tip every worker on the branch sends for, and asks
// nothing; a commit a sibling fetched nobody on the branch wrote, and is foreign to all of them.
func TestSiblings_ShareWhatTheyObserve(t *testing.T) {
	first, _, second, _ := siblingLoops(t)
	first.w.inbox.take()

	second.w.recordRemoteObservation("ccc", ObservedByPush)
	assert.Equal(t, "ccc", first.w.inbox.lastTip())
	assert.False(t, first.w.inbox.foreignPending(), "a sibling's push asks nothing")

	second.w.recordRemoteObservation("ddd", ObservedByFetch)
	assert.Equal(t, "ddd", first.w.inbox.lastTip())
	assert.True(t, first.w.inbox.foreignPending(), "what a sibling fetched is foreign here too")
}

func TestApplierRetryDelay_DoublesToACap(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		1: 2 * time.Second, 2: 4 * time.Second, 4: 16 * time.Second, 8: 256 * time.Second, 9: 5 * time.Minute,
		50: 5 * time.Minute,
	} {
		assert.Equal(t, want, applierRetryDelay(failures, false), "after %d failures", failures)
	}
	assert.Equal(t, 15*time.Second, applierRetryDelay(1, true), "a rejected attempt waits for a configuration change")
	assert.Equal(t, 5*time.Minute, applierRetryDelay(50, true))
}

// TestApplier_AStaleAttemptIsDecidedAgainWithoutBackoff: an attempt whose decision no longer held
// at the patch (the branch moved on) asked nothing, and that is no failure. What it carried comes
// back, and the next wake decides again at the new tip.
func TestApplier_AStaleAttemptIsDecidedAgainWithoutBackoff(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	require.True(t, out[0].Current(), "decided at the tip")

	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	assert.False(t, out[0].Current(), "the branch moved on")

	report(out[0], ApplyStale)
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1, "decided again at once: nothing failed")
	assert.Equal(t, "ccc", retry[0].Commit)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, retry[0].Reason)
	assert.Zero(t, retry[0].PriorFailures)
}

// TestDecisionHolds_OnlyAtTheTip: the check the requester makes before it patches. Only a moved tip
// makes a decision stale: an edit is published before the send, or recorded in the apply window.
func TestDecisionHolds_OnlyAtTheTip(t *testing.T) {
	w := newMetricsTestWorker()
	w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})
	assert.True(t, w.decisionHolds("aaa"))
	assert.False(t, w.decisionHolds("older"), "another commit is not the tip")

	require.True(t, w.Enqueue(windowEvent("alpha", "3", "kubectl-edit")))
	assert.True(t, w.decisionHolds("aaa"), "a queued edit holds nothing")
}

// TestPushNotice_TheTipAlreadySeenDoesNotCutAWindow is the customer review's third blocker: the
// late webhook for the worker's own push closed the next save's window, splitting that save across
// two commits. A notice naming the tip already observed leaves the window alone; one naming
// anything else, or nothing, still closes it.
func TestPushNotice_TheTipAlreadySeenDoesNotCutAWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		after     string
		wantClose bool
	}{
		{name: "our own push, reported late", after: "ours"},
		{name: "somebody else's push", after: "theirs", wantClose: true},
		{name: "a notice that names no commit", after: "", wantClose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop, _ := applyTriggerLoop(t, applierTestTarget("alpha", true), &configv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "test-provider", Namespace: "team-a"},
				Spec:       configv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
			})
			loop.w.recordRemoteObservation("ours", ObservedByPush)
			loop.openWindow = &openWindow{}

			loop.handleRefreshRequest(&RefreshRequest{PushNotice: true, After: tc.after})

			assert.Equal(t, tc.wantClose, loop.pushHinted)
		})
	}
}

// TestWriteBusy_ASaveAnnouncedBeforeItsWrites: a registered CommitRequest makes the branch busy, so
// a catch-up cannot land between a save's announcement and its first writes.
func TestWriteBusy_ASaveAnnouncedBeforeItsWrites(t *testing.T) {
	loop, _ := applyTriggerLoop(t, applierTestTarget("alpha", true))
	assert.False(t, loop.writeBusy())
	loop.pendingCRs = map[commitRequestID]*pendingCommitRequest{{}: {}}
	assert.True(t, loop.writeBusy())
}

// queuedRefreshes takes the refreshes the worker queued for its loop.
func queuedRefreshes(w *BranchWorker) []*RefreshRequest {
	var refreshes []*RefreshRequest
	for {
		select {
		case item := <-w.eventQueue:
			if item.Refresh != nil {
				refreshes = append(refreshes, item.Refresh)
			}
		default:
			return refreshes
		}
	}
}

// TestApplyAttempt_ASourceElsewhereIsAHintToLook: an applier's source that fetched another commit
// than the one asked for says the branch may have moved, past a decision made before the worker heard
// of it. With no receiver, the worker would otherwise ask again for a commit the source no longer
// serves, until its own push or the refresher found the move; it looks, as at a push notice. A source
// at the tip known here says nothing new.
func TestApplyAttempt_ASourceElsewhereIsAHintToLook(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	observeForeignCommit(loop.w)
	failWith := func(revision string) {
		t.Helper()
		loop.serviceAppliers()
		sent := requester.taken()
		require.Len(t, sent, 1)
		sent[0].Report(ApplyResult{ID: sent[0].ID, Outcome: ApplyRequestFailed,
			Err: errors.New("the source fetched another commit"), SourceRevision: revision})
		loop.serviceAppliers()
		dueNow(loop)
	}

	failWith("main@sha1:bbb")
	assert.Empty(t, queuedRefreshes(loop.w), "the source holds the tip known here")

	failWith("main@sha1:ccc")
	refreshes := queuedRefreshes(loop.w)
	require.Len(t, refreshes, 1, "the worker looks at the remote")
	assert.True(t, refreshes[0].PushNotice)
	assert.Equal(t, "ccc", refreshes[0].After)
}

// TestApplyAttempt_ANewFlushMarkStartsTheEditsOver: an applier can stay due without being sent (a
// sibling holds its group), and the next flush takes a newer mark, which publishes the edits recorded
// before it. Carried into the window, those would withhold the apply's own write of a commit that
// holds them, with a false collision and an extra Repair. A flush in progress keeps its record.
func TestApplyAttempt_ANewFlushMarkStartsTheEditsOver(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	loop.followRecordedOperations(time.Now())
	state := loop.appliers[applierRef("alpha")]
	state.flushEdits = map[string]time.Time{"x": time.Now()}

	loop.flushing = true
	require.True(t, loop.flushed([]*applierState{state}))
	assert.Len(t, state.flushEdits, 1, "the flush in progress keeps what it recorded")

	loop.flushing = false
	require.True(t, loop.flushed([]*applierState{state}))
	assert.Empty(t, state.flushEdits, "a new mark published them")
}
