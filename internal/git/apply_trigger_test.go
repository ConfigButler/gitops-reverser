// SPDX-License-Identifier: Apache-2.0

package git

// The branch worker's half of the apply trigger, from
// docs/design/push-notification-and-reconcile-trigger.md part 3: what the appliers are owed, and the
// rule that nothing is asked while anything live is unpublished.

import (
	"context"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestApplyDebt_FirstObservationIsABaseline(t *testing.T) {
	var debt applyDebt
	debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})

	tip, foreign, catchUp := debt.take()
	assert.Equal(t, "aaa", tip)
	assert.False(t, foreign, "nothing says the applier has not applied what was there at startup")
	assert.True(t, catchUp, "but nothing says it has, so it is owed a catch-up")
}

func TestApplyDebt_ClassifiesEachMove(t *testing.T) {
	cases := []struct {
		name        string
		next        RemoteObservation
		wantForeign bool
		wantCatchUp bool
	}{
		{name: "an unchanged tip owes nothing", next: RemoteObservation{Commit: "aaa", By: ObservedByFetch}},
		{name: "our own push owes a catch-up", next: RemoteObservation{Commit: "bbb", By: ObservedByPush},
			wantCatchUp: true},
		{name: "a fetch that found the branch moved owes a ForeignCommit",
			next: RemoteObservation{Commit: "bbb", By: ObservedByFetch}, wantForeign: true},
		{name: "a deleted branch is a move like any other",
			next: RemoteObservation{By: ObservedByFetch}, wantForeign: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var debt applyDebt
			debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
			debt.take()

			debt.observe(tc.next)
			tip, foreign, catchUp := debt.take()
			assert.Equal(t, tc.next.Commit, tip)
			assert.Equal(t, tc.wantForeign, foreign)
			assert.Equal(t, tc.wantCatchUp, catchUp)
		})
	}
}

func TestApplyDebt_RestorePutsBackWhatWasTaken(t *testing.T) {
	var debt applyDebt
	debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	debt.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch})
	_, foreign, catchUp := debt.take()
	require.True(t, foreign)

	debt.restore(foreign, catchUp)
	assert.True(t, debt.foreignOwed())
}

// applierTestTarget is a GitTarget on the trigger worker's provider and branch unless overridden.
func applierTestTarget(name string, applier bool, mutate ...func(*configv1alpha3.GitTarget)) *configv1alpha3.GitTarget {
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec: configv1alpha3.GitTargetSpec{
			GitProviderRef: meta.LocalObjectReference{Name: "test-provider"},
			Branch:         "main",
			Path:           "apps/" + name,
		},
	}
	if applier {
		target.Spec.Applier = testApplier(name)
	}
	for _, m := range mutate {
		m(target)
	}
	return target
}

// applyTriggerLoop is an event loop on provider team-a/test-provider, branch main, whose fake client
// holds the given GitTargets and whose requests are recorded.
func applyTriggerLoop(t *testing.T, targets ...client.Object) (*branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	w := newMetricsTestWorker()
	w.GitProviderNamespace = "team-a"
	w.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(targets...).Build()
	w.Log = logr.Discard()
	w.ctx = context.Background()
	requester := &recordingRequester{}
	w.applyRequester = requester
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)
	return loop, requester
}

// owesForeign makes the worker's debt hold a ForeignCommit at commit "bbb".
func owesForeign(w *BranchWorker) {
	w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	w.debt.take()
	w.debt.observe(RemoteObservation{Commit: "bbb", By: ObservedByFetch})
}

func TestServiceAppliers_ForeignCommitAsksEveryApplierOnTheBranch(t *testing.T) {
	loop, requester := applyTriggerLoop(t,
		applierTestTarget("alpha", true),
		applierTestTarget("bravo", true, func(g *configv1alpha3.GitTarget) { g.Spec.Suspend = true }),
		applierTestTarget("no-applier", false),
		applierTestTarget("other-branch", true, func(g *configv1alpha3.GitTarget) { g.Spec.Branch = "release" }),
		applierTestTarget("other-provider", true, func(g *configv1alpha3.GitTarget) {
			g.Spec.GitProviderRef.Name = "elsewhere"
		}),
	)
	owesForeign(loop.w)

	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[0].Reason)
	assert.Equal(t, "bbb", requests[0].Commit)
	assert.Equal(t, BranchKey{RepoNamespace: "team-a", RepoName: "test-provider", Branch: "main"}, requests[0].Branch)
	assert.Equal(t, []itypes.ResourceReference{
		itypes.NewResourceReference("alpha", "team-a"),
		itypes.NewResourceReference("bravo", "team-a"),
	}, requests[0].Targets, "a suspended target stops writing, not deployments")
}

func TestServiceAppliers_NothingIsSentWhileAnythingLiveIsUnpublished(t *testing.T) {
	cases := []struct {
		name string
		busy func(l *branchWorkerEventLoop)
	}{
		{name: "an open commit window", busy: func(l *branchWorkerEventLoop) { l.openWindow = &openWindow{} }},
		{name: "a pending write", busy: func(l *branchWorkerEventLoop) { l.pendingWrites = []PendingWrite{{}} }},
		{name: "a parked heal", busy: func(l *branchWorkerEventLoop) { l.deferredHeals = []*ResyncRequest{{}} }},
		{name: "a worktree a failed write left dirty", busy: func(l *branchWorkerEventLoop) {
			l.w.markWorktreeDirty("test")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true))
			owesForeign(loop.w)
			tc.busy(loop)

			loop.serviceAppliers()

			assert.Empty(t, requester.taken())
			assert.True(t, loop.quietSince.IsZero(), "a busy branch is not quiet")
			assert.True(t, loop.w.debt.foreignOwed(), "and the debt waits for it")
		})
	}
}

func TestServiceAppliers_CatchUpWaitsForTheQuietPeriod(t *testing.T) {
	loop, requester := applyTriggerLoop(t,
		applierTestTarget("soon", true, func(g *configv1alpha3.GitTarget) {
			g.Spec.Applier.QuietPeriod = &metav1.Duration{Duration: time.Minute}
		}),
		applierTestTarget("late", true, func(g *configv1alpha3.GitTarget) {
			g.Spec.Applier.QuietPeriod = &metav1.Duration{Duration: time.Hour}
		}),
	)
	loop.w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})

	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the branch has only just become quiet")
	require.NotNil(t, loop.applierTimer, "the earliest catch-up is armed")
	require.Len(t, loop.catchUpOwed, 2)

	// Two minutes of quiet: the one-minute applier is due, the one-hour one is not.
	loop.quietSince = time.Now().Add(-2 * time.Minute)
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, requests[0].Reason)
	assert.Equal(t, "aaa", requests[0].Commit)
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference("soon", "team-a")}, requests[0].Targets)
	assert.Contains(t, loop.catchUpOwed, itypes.NewResourceReference("late", "team-a"))
	assert.NotNil(t, loop.applierTimer)
}

func TestServiceAppliers_AWriteRestartsTheQuietPeriod(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true))
	loop.w.debt.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})
	loop.serviceAppliers()
	loop.quietSince = time.Now().Add(-time.Hour)

	loop.openWindow = &openWindow{}
	loop.serviceAppliers()
	assert.True(t, loop.quietSince.IsZero())
	assert.Nil(t, loop.applierTimer)

	loop.openWindow = nil
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the quiet period restarted when the write arrived")
	assert.WithinDuration(t, time.Now(), loop.quietSince, time.Second)
}

// TestServiceAppliers_AWriteDecidedAndPushedInOneWakeRestartsTheQuietPeriod is the regression the
// bi-directional e2e found. A window with a zero idle timeout opens, closes, commits and pushes in a
// single wake that ends quiet, so a quiet period read only from how wakes END was inherited from
// the quiet wakes before it: the catch-up went out at once, Flux applied that commit over the next
// API edit, and the revert was mirrored as Flux's commit.
func TestServiceAppliers_AWriteDecidedAndPushedInOneWakeRestartsTheQuietPeriod(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true))
	loop.serviceAppliers()
	loop.quietSince = time.Now().Add(-time.Hour)

	loop.decide(PendingWrite{})
	loop.pendingWrites, loop.pendingWritesBytes = nil, 0 // the same wake pushed it
	loop.w.debt.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	loop.serviceAppliers()

	assert.Empty(t, requester.taken(), "the catch-up waits a quiet period from the push, not from before it")
	assert.WithinDuration(t, time.Now(), loop.quietSince, time.Second)
	assert.NotNil(t, loop.applierTimer)
}

func TestServiceAppliers_AForeignCommitAnswersEverythingElseOwed(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("alpha", true))
	loop.oweReapply(refusalKeyFor(itypes.NewResourceReference("alpha", "team-a"), "deployments"), "refused", "obs")
	owesForeign(loop.w)

	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 1, "one apply of the tip covers the refusal and the catch-up")
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[0].Reason)
	assert.Empty(t, loop.reapplyOwed)
	assert.Empty(t, loop.catchUpOwed)
}

func TestServiceAppliers_NoApplierOnTheBranchClearsTheDebt(t *testing.T) {
	loop, requester := applyTriggerLoop(t, applierTestTarget("plain", false))
	owesForeign(loop.w)
	loop.w.debt.observe(RemoteObservation{Commit: "ccc", By: ObservedByPush})

	loop.serviceAppliers()

	assert.Empty(t, requester.taken())
	assert.False(t, loop.w.debt.foreignOwed())
	assert.Empty(t, loop.catchUpOwed)
	assert.Nil(t, loop.applierTimer)
}

func TestServiceAppliers_NoRequesterSwitchesTheTriggerOff(t *testing.T) {
	loop, _ := applyTriggerLoop(t, applierTestTarget("alpha", true))
	loop.w.applyRequester = nil
	owesForeign(loop.w)

	loop.serviceAppliers()

	assert.True(t, loop.w.debt.foreignOwed(), "nothing took the debt")
}

// TestCloseWindowForForeignCommit_PublishesTheOpenWindowNow: a window closes on silence, so a user
// who keeps editing would hold a foreign commit back for as long as they type. It is closed instead,
// and its write published and replayed onto the move.
func TestCloseWindowForForeignCommit_PublishesTheOpenWindowNow(t *testing.T) {
	cases := []struct {
		name  string
		cause func(l *branchWorkerEventLoop)
	}{
		{name: "a foreign commit observed", cause: func(l *branchWorkerEventLoop) { owesForeign(l.w) }},
		{name: "a Git host's push notice on a busy branch", cause: func(l *branchWorkerEventLoop) {
			l.handleRefreshRequest(&RefreshRequest{PushNotice: true})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker, _, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")
			worker.applyRequester = &recordingRequester{}
			loop := newBranchWorkerEventLoop(worker, time.Hour)
			t.Cleanup(loop.stopTimers)

			loop.handleQueueItem(WorkItem{Request: &WriteRequest{
				Events:     []Event{configMapTargetEvent("edited", "alice", "team-a")},
				CommitMode: CommitModePerEvent,
			}})
			require.NotNil(t, loop.openWindow, "an hour-long window is still open")

			tc.cause(loop)
			loop.closeWindowForForeignCommit()

			assert.Nil(t, loop.openWindow)
			assert.Len(t, loop.pendingWrites, 1, "the window's write is decided, to be published")
		})
	}
}

// applierLedgerFixture is a real Git server, a GitTarget naming an applier, and an event loop whose
// requests are recorded.
func applierLedgerFixture(t *testing.T, slug string) (*ledgerFixture, *branchWorkerEventLoop, *recordingRequester) {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.createLedgerTarget("team-a", nil)
	target := &configv1alpha3.GitTarget{}
	require.NoError(t, f.worker.Client.Get(f.worker.ctx,
		client.ObjectKey{Name: ledgerTargetName, Namespace: "default"}, target))
	target.Spec.Applier = testApplier("apps")
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, target))

	requester := &recordingRequester{}
	f.worker.applyRequester = requester
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(loop.stopTimers)
	return f, loop, requester
}

func foreignRequests(requests []ApplyRequest) []ApplyRequest {
	var foreign []ApplyRequest
	for _, req := range requests {
		if req.Reason == configv1alpha3.ApplyReasonForeignCommit {
			foreign = append(foreign, req)
		}
	}
	return foreign
}

// TestApplyTrigger_APushNoticeFindsAForeignCommit is §3.3 end to end over a real Git server: the
// notice names nothing the worker trusts, the advertisement shows a tip the worker did not push,
// and the request names that tip.
func TestApplyTrigger_APushNoticeFindsAForeignCommit(t *testing.T) {
	f, loop, requester := applierLedgerFixture(t, "apply-trigger-notice")
	f.publish("ours")
	loop.endWake(0)
	assert.Empty(t, foreignRequests(requester.taken()), "the worker's own push is not foreign")

	f.contend("OUTSIDE.md", "from-another-writer\n")
	loop.handleRefreshRequest(&RefreshRequest{PushNotice: true})
	loop.endWake(0)

	requests := foreignRequests(requester.taken())
	require.Len(t, requests, 1)
	assert.Equal(t, gitOut(t, f.repoDir, "rev-parse", "main"), requests[0].Commit)
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(ledgerTargetName, "default")},
		requests[0].Targets)

	// A duplicate delivery of the same push is one more advertisement and no second request.
	loop.handleRefreshRequest(&RefreshRequest{PushNotice: true})
	loop.endWake(0)
	assert.Empty(t, foreignRequests(requester.taken()))
}

// TestApplyTrigger_ARejectedPushOwesTheReplayedTip is §3.4: the foreign commit is found by the
// compare-and-swap, the pending write is replayed onto it, and the request names the replayed tip,
// which carries the API side's answer.
func TestApplyTrigger_ARejectedPushOwesTheReplayedTip(t *testing.T) {
	f, loop, requester := applierLedgerFixture(t, "apply-trigger-rejected")
	f.publish("prime")
	loop.endWake(0)
	requester.taken()

	f.commit("edited")
	f.contend("OUTSIDE.md", "from-another-writer\n")
	f.push()
	loop.endWake(0)

	requests := foreignRequests(requester.taken())
	require.Len(t, requests, 1)
	head := gitOut(t, f.repoDir, "rev-parse", "main")
	assert.Equal(t, head, requests[0].Commit, "the replayed tip, not the foreign commit under it")
	assert.Equal(t, "from-another-writer", gitOut(t, f.repoDir, "show", head+":OUTSIDE.md"))
}
