// SPDX-License-Identifier: Apache-2.0

package git

// What happens when a replay resets the worktree and then fails.
//
// Resetting and replaying is one operation with two halves, and the first half is destructive: the
// hard reset discards the local commits the retained writes produced. Until the second half
// finishes, those writes describe work that exists NOWHERE — not on the remote, and no longer in
// the worktree — while still carrying the commit hashes they had before the reset.
//
// A push in that state is silently wrong rather than noisily wrong. The local branch is exactly
// the remote tip, so validatePushState reports "already up to date" and PushAtomic returns nil
// before it ever compares the cycle's root hash. The worker then counts the writes as published,
// clears them, and resolves any CommitRequest riding one as Committed, naming a SHA that is not on
// the remote and never will be.
//
// worktreeDirty is false throughout: the reset cleared it, and the worktree really is clean. It is
// the retained writes that are stale, which is why this needs an invariant of its own.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.5.

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// failGitTargetReads makes every GitTarget read fail, which is what aborts a replay between the
// reset and the rebuild: rebuildPendingWrites re-reads each target's prune policy before it
// replays, so a transient API error there leaves the reset done and the rebuild not done.
//
// GitProvider reads are left working, because refreshRemoteAndRebuildPendingWrites resolves the
// provider BEFORE the reset. Failing that too would abort the whole thing harmlessly, which is the
// case that is already safe.
func failGitTargetReads(t *testing.T, worker *BranchWorker, cause error) func() {
	t.Helper()
	seen := 0
	inner, ok := worker.Client.(client.WithWatch)
	require.True(t, ok, "the fixture's fake client supports Watch")
	restore := func() { worker.Client = inner }
	worker.Client = interceptor.NewClient(inner, interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, isTarget := obj.(*configv1alpha3.GitTarget); isTarget {
				seen++
				return cause
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	return restore
}

// TestReplayFailure_DoesNotLetThePushSettleWorkThatWasReset is the P1 this file exists for.
func TestReplayFailure_DoesNotLetThePushSettleWorkThatWasReset(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-push", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	// One retained write, committed locally and waiting out the push cooldown.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("will-be-lost", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1)
	committedSHA := loop.pendingWrites[0].CommitSHA
	require.False(t, committedSHA.IsZero(), "the write has a local commit")

	// Something asks for a refresh — a dirty worktree, a resync snapshot, a forced recheck — and
	// the replay dies after the reset has already discarded that commit.
	apiDown := errors.New("etcdserver: request timed out")
	restoreAPI := failGitTargetReads(t, f.worker, apiDown)
	err := f.worker.refreshRemoteAndRebuildPendingWrites(
		f.worker.ctx, loop.pendingWrites, fetchReasonForcedRecheck)
	require.Error(t, err, "the replay must report that it did not finish")

	require.False(t, f.worker.worktreeDirty(),
		"the reset left a CLEAN worktree, which is why a dirty check cannot catch this")

	// The push timer fires while the API is still down. Local HEAD is the remote tip, so a push
	// would succeed against a branch that has nothing of ours on it.
	loop.pushPending()

	require.Len(t, loop.pendingWrites, 1,
		"the write was reset away and never replayed, so the push must not count it as published")
	assert.NotContains(t, remoteFileNames(t, f.repoDir), "will-be-lost",
		"and nothing reached the remote, which is exactly why it must stay retained")

	// The API error was transient. The next push rebuilds the write and publishes it for real.
	restoreAPI()
	loop.pushPending()

	assert.Empty(t, loop.pendingWrites, "the rebuilt write is published, so nothing stays retained")
	assert.Contains(t, remoteFileNames(t, f.repoDir), "will-be-lost",
		"the write survived the failed replay and reached the remote on the retry")
}

// TestReplayFailure_DoesNotResolveACommitRequestThatNeverLanded is the same hazard seen from the
// caller's side, which is where it does real damage: a user is told their save landed, with a SHA.
func TestReplayFailure_DoesNotResolveACommitRequestThatNeverLanded(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-cr", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("saved-by-hand", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	req := attachReq("alice", 0)
	req.GitTargetName = ledgerTargetName
	req.Message = "save: this must not be reported as committed"
	serviceAttach(loop, req)
	require.Len(t, loop.pendingWrites, 1)
	require.NotNil(t, loop.pendingWrites[0].CommitRequest)

	restoreAPI := failGitTargetReads(t, f.worker, errors.New("etcdserver: request timed out"))
	require.Error(t, f.worker.refreshRemoteAndRebuildPendingWrites(
		f.worker.ctx, loop.pendingWrites, fetchReasonForcedRecheck))

	loop.pushPending()

	_, resolved := f.worker.LookupCommitRequestOutcome("default", crName, "uid-"+crName)
	require.False(t, resolved,
		"the request must stay in flight: nothing of its commit is on the remote")

	// Once the API recovers, the rebuilt commit lands and the request resolves against a SHA that
	// really is on the remote.
	restoreAPI()
	loop.pushPending()

	res, resolved := f.worker.LookupCommitRequestOutcome("default", crName, "uid-"+crName)
	require.True(t, resolved)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Contains(t, remoteFileNames(t, f.repoDir), "saved-by-hand")
}

// remoteFileNames lists every path on the remote's main branch.
func remoteFileNames(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "ls-tree", "-r", "--name-only", "refs/heads/main").
		CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

// TestReplayFailure_HoldsAWholeBatchNotJustItsFirstWrite checks that a failed replay holds every
// retained write, not only the one that happened to be first.
//
// An earlier version of this test was named for a mid-batch failure and did not produce one. The
// injection point is a GitTarget read, and `rebuildPendingWrites` runs `tightenPendingPruneModes`
// to completion BEFORE `executePendingWrites` starts, caching one read per target — so failing
// those reads always aborts before the first write, whatever the batch size. There is no seam that
// fails between two writes, and adding one to production code to reach a state the flag already
// covers by construction is not worth it: `replayRequired` is per-worker, not per-write, so a
// partly-rebuilt batch cannot be settled either. This test pins the batch behaviour that IS
// reachable, and the comment records why the other shape is not.
func TestReplayFailure_HoldsAWholeBatchNotJustItsFirstWrite(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-midbatch", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	// Two retained writes, each its own local commit.
	for _, name := range []string{"first-of-two", "second-of-two"} {
		loop.handleQueueItem(WorkItem{Request: &WriteRequest{
			Events:     []Event{configMapTargetEvent(name, "alice", ledgerTargetName)},
			CommitMode: CommitModePerEvent,
		}})
		require.True(t, loop.finalizeOpenWindow())
	}
	require.Len(t, loop.pendingWrites, 2)

	// Fail the replay after the reset. The batch is torn down and only partly rebuilt.
	restoreAPI := failGitTargetReads(t, f.worker, errors.New("etcdserver: request timed out"))
	require.Error(t, f.worker.refreshRemoteAndRebuildPendingWrites(
		f.worker.ctx, loop.pendingWrites, fetchReasonForcedRecheck))

	loop.pushPending()
	require.Len(t, loop.pendingWrites, 2, "neither write may be settled by a push that sent nothing")

	restoreAPI()
	loop.pushPending()

	names := remoteFileNames(t, f.repoDir)
	assert.Contains(t, names, "first-of-two")
	assert.Contains(t, names, "second-of-two", "the whole batch survives, not just its first half")
	assert.Empty(t, loop.pendingWrites)
}

// TestReplayFailure_RecoveryRunsBeforeEveryCommitPathToo pins that the push is not the only gate.
// A commit arriving before the push timer must rebuild first as well, or it plans on a tree that no
// longer holds the retained work.
func TestReplayFailure_RecoveryRunsBeforeEveryCommitPathToo(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-commit-path", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("retained-across-replay", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())

	restoreAPI := failGitTargetReads(t, f.worker, errors.New("etcdserver: request timed out"))
	require.Error(t, f.worker.refreshRemoteAndRebuildPendingWrites(
		f.worker.ctx, loop.pendingWrites, fetchReasonForcedRecheck))
	require.True(t, f.worker.replayRequired())
	restoreAPI()

	// A second write arrives. Finalizing it recovers first, so both writes are real commits again.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("arrived-after-replay", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	assert.False(t, f.worker.replayRequired(), "the commit path rebuilt the stranded write")

	loop.pushPending()
	names := remoteFileNames(t, f.repoDir)
	assert.Contains(t, names, "retained-across-replay")
	assert.Contains(t, names, "arrived-after-replay")
}

// TestReplayFailure_OnTheContentionPathIsAlsoHeld covers the second place a reset is followed by a
// rebuild, which is easy to miss because it does not go through
// refreshRemoteAndRebuildPendingWrites at all.
//
// runPushCycle resets and replays inline when a push is rejected. The two halves are the same two
// halves, so the same window exists: if the rebuild fails there, the retained writes are stranded
// with their pre-reset hashes and the NEXT push finds the branch already at the tip.
func TestReplayFailure_OnTheContentionPathIsAlsoHeld(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-contention", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("lost-to-contention", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1)

	// Somebody else moves the branch, so our push is rejected and the replay runs inline.
	f.contend("from-another-writer.txt", "hello\n")

	// The replay's rebuild fails on the prune-policy re-read, after its reset has landed.
	restoreAPI := failGitTargetReads(t, f.worker, errors.New("etcdserver: request timed out"))
	loop.pushPending()
	require.Len(t, loop.pendingWrites, 1, "the rejected push retains its writes")

	// The reset inside the replay put us at the contending writer's tip, so a push now has
	// nothing to send and would report success.
	loop.pushPending()
	require.Len(t, loop.pendingWrites, 1,
		"a push that sent nothing must not settle writes the replay's reset discarded")

	restoreAPI()
	loop.pushPending()

	names := remoteFileNames(t, f.repoDir)
	assert.Contains(t, names, "lost-to-contention", "the write survives the failed replay")
	assert.Contains(t, names, "from-another-writer.txt", "and the other writer's commit is kept")
	assert.Empty(t, loop.pendingWrites)
}

// TestReplayFailure_AResetThatDiesHalfwayIsAlsoHeld is the window one step earlier than the rest of
// this file: not a rebuild that failed after a good reset, but a RESET that failed having already
// moved the branch ref.
//
// checkoutAndReset moves HEAD and then rewrites the worktree, so those two can come apart: the
// commits behind the retained writes become unreachable while the call still returns an error.
// None of the three flags described that state until the mark moved ahead of the reset —
// baseTrusted is cleared, but worktreeDirty is false (no write failed) and the reset never
// reported success, so nothing downstream knew the commits were gone.
//
// The failure is injected through the syncToRemoteFn seam rather than by making a directory
// read-only. The point is not which errno gets there; it is that a reset can move the ref and then
// fail, and disk exhaustion or a volume remounting read-only after an I/O error reach that state
// without anyone touching permissions.
func TestReplayFailure_AResetThatDiesHalfwayIsAlsoHeld(t *testing.T) {
	f := newLedgerFixture(t, "replay-failure-halfway-reset", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("must-survive", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1)

	// The reset moves the branch ref onto the remote tip and THEN fails, exactly as a hard reset
	// does when it runs out of disk part-way through rewriting the worktree.
	original := syncToRemoteFn
	syncToRemoteFn = func(
		ctx context.Context, repo *gogit.Repository,
		branch plumbing.ReferenceName, auth []gitclient.Option,
	) (*PullReport, error) {
		if _, err := SmartFetch(ctx, repo, branch, auth); err != nil {
			return nil, err
		}
		remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch.Short()), true)
		require.NoError(t, err)
		// Move the local branch onto the remote tip, discarding our commit, then report failure.
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(branch, remoteRef.Hash())))
		return nil, errors.New("write /repo/team-a/cm.yaml: no space left on device")
	}
	defer func() { syncToRemoteFn = original }()

	require.Error(t, f.worker.refreshRemoteAndRebuildPendingWrites(
		f.worker.ctx, loop.pendingWrites, fetchReasonForcedRecheck))

	require.False(t, f.worker.worktreeDirty(),
		"no write failed, so the dirty flag is false and cannot describe this")
	require.True(t, f.worker.replayRequired(),
		"but the commits behind the retained writes are gone, and that must be recorded")

	// Without the flag, this push finds the branch already at the remote tip, sends nothing,
	// reports success and clears the write.
	syncToRemoteFn = original
	loop.pushPending()

	assert.Empty(t, loop.pendingWrites, "recovery rebuilt the write and published it")
	assert.Contains(t, remoteFileNames(t, f.repoDir), "must-survive",
		"the write must reach the remote rather than vanish with the failed reset")
}
