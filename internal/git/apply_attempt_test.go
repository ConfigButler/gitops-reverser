// SPDX-License-Identifier: Apache-2.0

package git

// What happens to an apply attempt after the worker decided it, and how one worker serves the
// many GitTargets and appliers on its branch. The worker owns every obligation until an attempt
// delivers it; an attempt that does not deliver gives back only what still stands, and its retry is
// decided again like a first attempt. See docs/design/apply-trigger.md.

import (
	"context"
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

// oweAlphaRefusal owes alpha's applier a Refusal request for a standing refusal of its deployments.
func oweAlphaRefusal(loop *branchWorkerEventLoop) refusalKey {
	key := refusalKeyFor(itypes.NewResourceReference("alpha", "team-a"), "deployments")
	loop.oweReapply(key, owedRefusal{detail: "a base-owned field", uid: "uid-alpha"}, "obs-alpha")
	return key
}

// TestApplier_ARetryIsDecidedAgainOnAQuietBranch: an attempt that failed is not replayed blind. It
// waits out its backoff, then waits for a quiet branch like a first attempt, and names the tip as
// it is then.
func TestApplier_ARetryIsDecidedAgainOnAQuietBranch(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)

	report(first[0], ApplyRequestFailed)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not before its backoff")
	assert.NotNil(t, loop.applierTimer, "the backoff is armed")

	dueNow(loop)
	loop.openWindow = &openWindow{}
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not while the branch is busy")

	loop.openWindow = nil
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, retry[0].Reason, "the obligation came back")
	assert.Equal(t, "ccc", retry[0].Commit, "at the tip as it is now")
	assert.Equal(t, 1, retry[0].PriorFailures, "so the requester does not report the failure twice")
}

// TestApplier_WithdrawnConsentNeverBecomesAnImmediateApply is the second review's first finding.
// The requester used to turn a Refusal whose target withdrew consent into a CatchUp and send it at
// once, skipping the quiet period, so a behind artifact was fetched over an unpublished edit. Now
// the requester only reports that the reason no longer holds, and the worker decides: no Refusal,
// and a CatchUp only after the quiet period.
func TestApplier_WithdrawnConsentNeverBecomesAnImmediateApply(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying, func(g *configv1alpha3.GitTarget) {
		g.Spec.Applier.QuietPeriod = &metav1.Duration{Duration: time.Hour}
	}))
	loop.w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch}) // owes a catch-up
	oweAlphaRefusal(loop)
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

	assert.Empty(t, requester.taken(), "the catch-up waits out the hour-long quiet period")
	assert.Empty(t, loop.reapplyOwed, "and the Refusal is gone with the consent")
	assert.True(t, loop.appliers[applierRef("alpha")].catchUp)
}

// TestApplier_ARecoveredRefusalIsNotRetried is the second review's second finding: a refusal that
// recovered while its attempt was out came back with the failed attempt and was requested again.
func TestApplier_ARecoveredRefusalIsNotRetried(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying))
	key := oweAlphaRefusal(loop)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	loop.refusalRecovered(key.target, key.collection)
	report(out[0], ApplyRequestFailed)
	loop.serviceAppliers()
	dueNow(loop)
	loop.serviceAppliers()

	assert.Empty(t, requester.taken(), "a recovered refusal has nothing left to ask for")
	assert.Empty(t, loop.reapplyOwed)
}

// TestApplier_ARecreatedTargetInheritsNothing is the second review's third finding: an obligation
// decided for one incarnation of a GitTarget executed for another created under the same name.
// Requests carry the uid, and the worker drops what was owed to an incarnation that is gone.
func TestApplier_ARecreatedTargetInheritsNothing(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, reapplying))
	loop.openWindow = &openWindow{}
	oweAlphaRefusal(loop)

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
	assert.Empty(t, loop.reapplyOwed)

	owesForeign(loop.w)
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
	owesForeign(loop.w)
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

// TestApplier_OneAttemptForEveryTargetNamingIt: GitTargets that share a Kustomization are asked for
// together, each with its own reason, and the shared applier waits for the longest of their quiet
// periods, because its apply reaches every one of their folders.
func TestApplier_OneAttemptForEveryTargetNamingIt(t *testing.T) {
	shared := func(quiet time.Duration) func(*configv1alpha3.GitTarget) {
		return func(g *configv1alpha3.GitTarget) {
			g.Spec.Applier = testApplier("shared")
			g.Spec.Applier.QuietPeriod = &metav1.Duration{Duration: quiet}
		}
	}
	loop, requester := applyTriggerLoop(t,
		applierTestTarget("alpha", true, shared(time.Minute), reapplying),
		applierTestTarget("bravo", true, shared(time.Hour)),
	)
	loop.w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})
	loop.serviceAppliers()
	assert.Equal(t, time.Hour, loop.appliers[applierRef("shared")].quietPeriod())

	loop.quietSince = time.Now().Add(-2 * time.Minute)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "past alpha's quiet period, not bravo's")

	oweAlphaRefusal(loop)
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
	}, out[0].Targets, "a Refusal applies the tip, so it answers the catch-up owed to bravo as well")
	assert.False(t, loop.appliers[applierRef("shared")].catchUp)
}

// TestApplier_AnAttemptOutHoldsTheNext: one attempt per applier at a time. What is owed while it is
// out waits for its report, and goes in the attempt after.
func TestApplier_AnAttemptOutHoldsTheNext(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the first attempt is still out")

	report(out[0], ApplySucceeded)
	loop.serviceAppliers()
	next := requester.taken()
	require.Len(t, next, 1)
	assert.Equal(t, "ccc", next[0].Commit)
	assert.Zero(t, next[0].PriorFailures)
}

// TestApplier_ATargetThatNoLongerNamesAnApplierIsOwedNothing: a report for an applier nobody names
// any more is dropped with it.
func TestApplier_ATargetThatNoLongerNamesAnApplierIsOwedNothing(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)

	updateTarget(t, loop, "alpha", func(g *configv1alpha3.GitTarget) { g.Spec.Applier = nil })
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	loop.serviceAppliers()
	report(out[0], ApplyRequestFailed)
	loop.serviceAppliers()

	assert.Empty(t, loop.appliers)
	assert.Empty(t, requester.taken())
	assert.Nil(t, loop.applierTimer)
}

// TestSiblings_ABusyWorkerOnTheSameBranchHoldsTheOther: two GitProviders naming one repository give
// one branch two workers, and a Kustomization applies the tip whichever of them wrote it. So one
// worker sends nothing while the other holds an open window, and is woken when it closes.
func TestSiblings_ABusyWorkerOnTheSameBranchHoldsTheOther(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true))
	loop.w.repo = RepoIdentity{URL: "https://github.com/acme/platform"}
	loop.w.applyWake = make(chan struct{}, 1)
	sibling := newMetricsTestWorker()
	sibling.repo = RepoIdentity{URL: "git@github.com:acme/platform.git"}
	sibling.applyWake = make(chan struct{}, 1)
	other := newMetricsTestWorker()
	other.repo = RepoIdentity{URL: "https://github.com/acme/elsewhere"}
	siblings := newBranchSiblings()
	for _, w := range []*BranchWorker{loop.w, sibling, other} {
		siblings.add(w)
	}

	other.publishWriteBusy(true)
	sibling.publishWriteBusy(true)
	owesForeign(loop.w)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the sibling's open window is on this branch too")
	assert.True(t, loop.quietSince.IsZero())

	sibling.publishWriteBusy(false)
	select {
	case <-loop.w.applyWake:
	default:
		t.Fatal("a sibling going quiet must wake the worker it held back")
	}
	loop.serviceAppliers()
	assert.Len(t, requester.taken(), 1, "a worker on another repository never held it")

	siblings.remove(sibling)
	assert.False(t, loop.w.siblingWriteBusy())
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
// at the patch (the branch moved on, or became busy) asked nothing, and that is no failure. What it
// carried comes back, and the next quiet moment decides again at the new tip.
func TestApplier_AStaleAttemptIsDecidedAgainWithoutBackoff(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	owesForeign(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	require.True(t, out[0].Current(), "decided at the tip on a quiet branch")

	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	assert.False(t, out[0].Current(), "the branch moved on")

	report(out[0], ApplyStale)
	loop.serviceAppliers()
	retry := requester.taken()
	require.Len(t, retry, 1, "decided again at once: nothing failed")
	assert.Equal(t, "ccc", retry[0].Commit)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, retry[0].Reason)
	assert.Zero(t, retry[0].PriorFailures)
}

// TestDecisionHolds_OnlyAtTheTipOnAQuietBranch: the check the requester makes before it patches.
func TestDecisionHolds_OnlyAtTheTipOnAQuietBranch(t *testing.T) {
	w := newMetricsTestWorker()
	w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})
	assert.True(t, w.decisionHolds("aaa"))
	assert.False(t, w.decisionHolds("older"), "another commit is not the tip")

	w.publishWriteBusy(true)
	assert.False(t, w.decisionHolds("aaa"), "a window open since")
	w.publishWriteBusy(false)

	w.eventQueue <- WorkItem{}
	assert.False(t, w.decisionHolds("aaa"), "a queued event is a live edit not yet decided")
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
