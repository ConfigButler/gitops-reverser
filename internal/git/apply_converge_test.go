// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// spec.catchUpAfter: a push of the worker's own asks nothing at once, but an applier left behind
// the tip for the interval is asked a CatchUp of it, so its own state (applied revision, source
// artifact) catches up with the branch.

// convergedAt starts loop's applier, and has it apply the first tip, "aaa", at startup.
func convergedAt(t *testing.T, loop *branchWorkerEventLoop, requester *recordingRequester) {
	t.Helper()
	const commit = "aaa"
	loop.w.inbox.observe(RemoteObservation{Commit: commit, By: ObservedByFetch})
	loop.serviceAppliers()
	startup := requester.taken()
	require.Len(t, startup, 1, "the startup converge")
	startup[0].Report(ApplyResult{ID: startup[0].ID, Outcome: ApplySucceeded, AppliedRevision: commit})
	loop.serviceAppliers()
	require.Empty(t, requester.taken())
}

// intervalPassed moves the clock of every applier's converge back by more than its interval.
func intervalPassed(t *testing.T, loop *branchWorkerEventLoop) {
	t.Helper()
	for _, state := range loop.appliers {
		require.False(t, state.behindSince.IsZero(), "the clock runs only for an applier behind the tip")
		state.behindSince = state.behindSince.Add(-state.catchUpAfter - time.Second)
	}
}

func catchUpEvery(interval time.Duration) func(*configv1alpha3.FluxClaim) {
	return func(c *configv1alpha3.FluxClaim) { c.Spec.CatchUpAfter = &metav1.Duration{Duration: interval} }
}

// TestCatchUpAfter_ABurstOfOwnPushesConvergesOnceAfterTheInterval: each push asks nothing at once and the
// timer is armed for the interval; once it has passed, one CatchUp of the newest tip goes, and an
// applier at the tip is not clocked any more.
func TestCatchUpAfter_ABurstOfOwnPushesConvergesOnceAfterTheInterval(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	state := loop.appliers[applierRef("alpha")]

	for _, commit := range []string{"bbb", "ccc", "ddd"} {
		loop.w.inbox.observe(RemoteObservation{Commit: commit, By: ObservedByPush})
		loop.serviceAppliers()
		require.Empty(t, requester.taken(), "the worker's own push asks nothing at once")
	}
	require.NotNil(t, loop.applierTimer, "the timer wakes the loop when the interval has passed")
	assert.Equal(t, configv1alpha3.DefaultFluxCatchUpAfter, state.catchUpAfter, "a FluxClaim's default")

	intervalPassed(t, loop)
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1, "one apply for the whole burst")
	assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, requests[0].Reason)
	assert.Equal(t, "ddd", requests[0].Commit)
	assert.True(t, state.behindSince.IsZero(), "a send starts the clock again")

	requests[0].Report(ApplyResult{ID: requests[0].ID, Outcome: ApplySucceeded, AppliedRevision: "main@sha1:ddd"})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken())
	assert.True(t, state.behindSince.IsZero(), "an applier at the tip is not clocked")
}

// TestCatchUpAfter_APushDuringAnAttemptWaitsAFreshInterval: the attempt applied the tip it was sent for,
// and the push that landed meanwhile waits an interval from the send, not from before it.
func TestCatchUpAfter_APushDuringAnAttemptWaitsAFreshInterval(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	loop.serviceAppliers()
	intervalPassed(t, loop)
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)

	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	loop.serviceAppliers()
	sent[0].Report(ApplyResult{ID: sent[0].ID, Outcome: ApplySucceeded, AppliedRevision: "bbb"})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "behind again, but the interval starts at the send")

	intervalPassed(t, loop)
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, "ccc", requests[0].Commit)
}

// TestCatchUpAfter_ZeroNeverConverges: "0s" keeps a push of the worker's own asking nothing, ever; an
// ArgoCDClaim defaults to it, because a sync runs the Application's hooks.
func TestCatchUpAfter_ZeroNeverConverges(t *testing.T) {
	for name, objects := range map[string][]client.Object{
		"flux 0s": {applierTestTarget("alpha", true), fluxClaim("alpha", catchUpEvery(0))},
		"argo cd": {applierTestTarget("alpha", true, argoApplier)},
	} {
		t.Run(name, func(t *testing.T) {
			loop, requester := manualLoop(t, objects...)
			convergedAt(t, loop, requester)
			loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
			loop.serviceAppliers()

			for _, state := range loop.appliers {
				assert.Zero(t, state.catchUpAfter)
				assert.True(t, state.behindSince.IsZero(), "no clock runs")
			}
			assert.Empty(t, requester.taken())
		})
	}
}

// TestCatchUpAfter_YieldsToAnEditBeforeItsResume: an interval converge's decision stops holding once a
// live edit is mirrored after its send, since the tip it fetched lacks that edit and applying it would
// flip the object on the cluster. Stale, it waits a fresh interval, then goes for the newer tip.
func TestCatchUpAfter_YieldsToAnEditBeforeItsResume(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	loop.serviceAppliers()
	intervalPassed(t, loop)
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	require.True(t, sent[0].Current(), "nothing happened since the send")

	loop.handleLiveEvents(&WriteRequest{Events: []Event{windowEvent("alpha", "2", "kubectl-edit")}})
	assert.False(t, sent[0].Current(), "an edit arrived during the fetch")

	report(sent[0], ApplyStale)
	loop.openWindow = nil
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "not again at once: a fresh interval")
	state := loop.appliers[applierRef("alpha")]
	assert.False(t, state.converge)

	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})
	intervalPassed(t, loop)
	loop.serviceAppliers()
	again := requester.taken()
	require.Len(t, again, 1)
	assert.Equal(t, "ccc", again[0].Commit)
}

// TestCatchUpAfter_AnyOtherReasonDoesNotYield: a ForeignCommit is not a catch-up, so an edit does not make
// it stale; the apply window repairs what it overwrites (S1).
func TestCatchUpAfter_AnyOtherReasonDoesNotYield(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch})
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	require.Equal(t, configv1alpha3.ApplyReasonForeignCommit, sent[0].Reason)

	loop.handleLiveEvents(&WriteRequest{Events: []Event{windowEvent("alpha", "2", "kubectl-edit")}})
	assert.True(t, sent[0].Current())
}

// TestCatchUpAfter_AChangedIntervalAppliesFromTheNextWake: the interval is read from the applier claim on
// every pass that finds the applier behind, so a change needs no listing, which a push of the
// worker's own does not make.
func TestCatchUpAfter_AChangedIntervalAppliesFromTheNextWake(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), fluxClaim("alpha", catchUpEvery(0)))
	convergedAt(t, loop, requester)
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	loop.serviceAppliers()
	state := loop.appliers[applierRef("alpha")]
	require.True(t, state.behindSince.IsZero(), "off")

	claim := &configv1alpha3.FluxClaim{}
	require.NoError(t, loop.w.Client.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "alpha"}, claim))
	catchUpEvery(30 * time.Second)(claim)
	require.NoError(t, loop.w.Client.Update(t.Context(), claim))

	loop.serviceAppliers()
	assert.Equal(t, 30*time.Second, state.catchUpAfter)
	intervalPassed(t, loop)
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, "bbb", requests[0].Commit)
}

// TestCatchUpAfter_ASaveThatAsksCatchesUpAtOnce: a CommitRequest with catchUp: ImmediatelyAfterPush
// leaves a converge pending for its GitTarget's applier once its commit is pushed, without waiting
// for catchUpAfter; one that pushed no commit asks nothing. It does not yield to an edit: it was
// asked for.
func TestCatchUpAfter_ASaveThatAsksCatchesUpAtOnce(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	loop.pendingCRs = map[commitRequestID]*pendingCommitRequest{}
	save := func(name string, commit string) {
		id := commitRequestID{Namespace: "team-a", Name: name, UID: "uid-" + name}
		loop.pendingCRs[id] = &pendingCommitRequest{
			id: id, gitTargetName: "alpha", gitTargetNamespace: "team-a", catchUpNow: true,
		}
		loop.resolveCommitRequest(id, FinalizeResult{Outcome: FinalizeCommitted, Commit: commit})
	}

	save("nothing-pushed", "")
	loop.serviceAppliers()
	require.Empty(t, requester.taken(), "no commit, nothing to catch up with")

	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	save("pushed", "bbb")
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1, "at once, not after catchUpAfter")
	assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, requests[0].Reason)
	assert.Equal(t, "bbb", requests[0].Commit)

	loop.handleLiveEvents(&WriteRequest{Events: []Event{windowEvent("alpha", "2", "kubectl-edit")}})
	assert.True(t, requests[0].Current(), "asked for, so it does not yield to an edit")
}

// TestSaveApply_FollowsTheAttemptThatCarriesIt: a save that asked to catch up waits on its applier,
// is carried by the attempt sent next, is retried when that attempt does not succeed, and is
// settled applied by the one that does. One that pushed nothing, or whose GitTarget names no
// applier, is settled at once as not applied.
func TestSaveApply_FollowsTheAttemptThatCarriesIt(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("loner", false))
	convergedAt(t, loop, requester)
	loop.pendingCRs = map[commitRequestID]*pendingCommitRequest{}
	save := func(name, target, commit string) commitRequestID {
		id := commitRequestID{Namespace: "team-a", Name: name, UID: "uid-" + name}
		loop.pendingCRs[id] = &pendingCommitRequest{
			id: id, gitTargetName: target, gitTargetNamespace: "team-a", catchUpNow: true,
		}
		loop.resolveCommitRequest(id, FinalizeResult{Outcome: FinalizeCommitted, Commit: commit})
		return id
	}
	lookup := func(id commitRequestID) SaveApply {
		apply, known := loop.w.LookupSaveApply(id.Namespace, id.Name, id.UID)
		require.True(t, known, "the worker knows %s", id.Name)
		return apply
	}

	nothing := save("nothing", "alpha", "")
	assert.Equal(t, SaveApply{Done: true, Reason: SaveApplyNothingPushed, Message: lookup(nothing).Message},
		lookup(nothing))

	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	pushed := save("pushed", "alpha", "bbb")
	alone := save("alone", "loner", "bbb")
	assert.Equal(t, SaveApplyApplying, lookup(pushed).Reason)
	loop.serviceAppliers()
	assert.True(t, lookup(alone).Done)
	assert.Equal(t, SaveApplyNoApplier, lookup(alone).Reason)

	first := requester.taken()
	require.Len(t, first, 1)
	report(first[0], ApplyFailed)
	loop.serviceAppliers()
	assert.Equal(t, SaveApplyRetrying, lookup(pushed).Reason)
	assert.False(t, lookup(pushed).Done)

	dueNow(loop)
	loop.serviceAppliers()
	again := requester.taken()
	require.Len(t, again, 1, "the retry carries the save")
	again[0].Report(ApplyResult{ID: again[0].ID, Outcome: ApplySucceeded, AppliedRevision: "main@sha1:bbb"})
	loop.serviceAppliers()
	settled := lookup(pushed)
	assert.True(t, settled.Done)
	assert.True(t, settled.Applied)
	assert.Equal(t, "main@sha1:bbb", settled.AppliedRevision)
}

// TestSaveApply_AnApplierAlreadyAtTheTipSettlesIt: when the applier applied the tip before the save's
// converge went, nothing is asked, and the save is applied all the same.
func TestSaveApply_AnApplierAlreadyAtTheTipSettlesIt(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	convergedAt(t, loop, requester)
	loop.pendingCRs = map[commitRequestID]*pendingCommitRequest{}
	id := commitRequestID{Namespace: "team-a", Name: "late", UID: "uid-late"}
	loop.pendingCRs[id] = &pendingCommitRequest{
		id:                 id,
		gitTargetName:      "alpha",
		gitTargetNamespace: "team-a",
		catchUpNow:         true,
	}
	loop.resolveCommitRequest(id, FinalizeResult{Outcome: FinalizeCommitted, Commit: "aaa"})

	loop.serviceAppliers()
	assert.Empty(t, requester.taken())
	apply, known := loop.w.LookupSaveApply(id.Namespace, id.Name, id.UID)
	require.True(t, known)
	assert.True(t, apply.Applied)
}
