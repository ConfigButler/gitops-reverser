// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// These tests pin whenNothingToCommit: CommitEmpty (docs/design/commit-timing-surface.md): a
// request that ends with nothing to commit records its message in an empty commit, the outcome
// keeps the cause, and a window that belonged to someone else never falls back to one.

func commitEmptyReq(author, message string) *AttachCommitRequest {
	req := attachReq(author, time.Hour)
	req.Message = message
	req.CommitEmpty = true
	return req
}

// requirePushedEmptyCommit asserts the reported commit reached the remote, carries the message and
// changed no file.
func requirePushedEmptyCommit(t *testing.T, serverRepo *gogit.Repository, res FinalizeResult, message string) {
	t.Helper()
	require.NotEmpty(t, res.Commit, "a recorded message must come with the commit that holds it")
	head, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, res.Commit, head.Hash().String(), "the empty commit is what the remote now holds")
	commit, err := serverRepo.CommitObject(head.Hash())
	require.NoError(t, err)
	assert.Equal(t, message, commit.Message)
	require.Equal(t, 1, commit.NumParents())
	parent, err := commit.Parent(0)
	require.NoError(t, err)
	assert.Equal(t, parent.TreeHash, commit.TreeHash, "the commit changes no file")
}

func TestCommitEmpty_NoWindowBeforeTheTimeoutRecordsTheMessage(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// A first commit, so the branch exists and the record has a parent.
	writeTo(loop, "existing")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()

	serviceAttach(loop, commitEmptyReq("alice", "save: nothing changed"))
	forceDue(loop)
	loop.endWake(0)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome, "the cause is kept: no window arrived in time")
	requirePushedEmptyCommit(t, serverRepo, res, "save: nothing changed")

	commit, err := serverRepo.CommitObject(plumbing.NewHash(res.Commit))
	require.NoError(t, err)
	assert.Equal(t, "alice", commit.Author.Name, "the record is authored by the request's submitter")
}

func TestCommitEmpty_AWindowThatChangedNothingRecordsTheMessage(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "present")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()

	// The same object again: a window with no diff, which the request attaches to.
	writeTo(loop, "present")
	serviceAttach(loop, commitEmptyReq("alice", "save: already there"))
	forceDue(loop)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeAlreadyPresent, res.Outcome, "the cause is kept: what arrived already matched Git")
	requirePushedEmptyCommit(t, serverRepo, res, "save: already there")
}

func TestCommitEmpty_AWindowThatChangedFilesIsAnOrdinaryCommit(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, commitEmptyReq("alice", "save: real change"))
	writeTo(loop, "new")
	forceDue(loop)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeCommitted, res.Outcome, "CommitEmpty changes nothing when there is something to commit")
}

func TestCommitEmpty_AWindowMismatchNeverFallsBackToAnEmptyCommit(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "existing")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()
	before, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	serviceAttach(loop, commitEmptyReq("bob", "bob's save"))
	writeTo(loop, "alices-edit") // only alice's window is ever open
	forceDue(loop)
	loop.endWake(0)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeWindowMismatch, res.Outcome)
	assert.Empty(t, res.Commit, "recording bob's save while alice's work is in flight would claim more than happened")
	assert.Empty(t, loop.pendingWrites, "no record write was made")
	after, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, before.Hash(), after.Hash())
}

func TestCommitEmpty_ResolveIsTheDefaultAndCommitsNothing(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	req.Message = "save"
	serviceAttach(loop, req)
	forceDue(loop)
	loop.endWake(0)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
	assert.Empty(t, res.Commit)
	assert.Empty(t, loop.pendingWrites)
}

// TestCommitEmpty_AFailedPushTreatsEveryCommitShapeAlike pins that a request's result follows the
// data through a failed push, whichever commit carries it. A window with changes, a window that
// changed nothing, and the record of a request no window reached all become one retained write, so
// a push that keeps failing leaves each of them WaitingForPush, a re-sent attach resolves none of
// them, and the push that finally succeeds settles each with the commit the remote now holds.
func TestCommitEmpty_AFailedPushTreatsEveryCommitShapeAlike(t *testing.T) {
	const message = "save: through a failed push"
	cases := []struct {
		name    string
		commit  func(loop *branchWorkerEventLoop)
		outcome FinalizeOutcome
		empty   bool
	}{
		{
			name: "a window that changed files",
			commit: func(loop *branchWorkerEventLoop) {
				serviceAttach(loop, commitEmptyReq("alice", message))
				writeTo(loop, "new")
				forceDue(loop)
			},
			outcome: FinalizeCommitted,
		},
		{
			name: "a window that changed nothing",
			commit: func(loop *branchWorkerEventLoop) {
				writeTo(loop, "existing")
				serviceAttach(loop, commitEmptyReq("alice", message))
				forceDue(loop)
			},
			outcome: FinalizeAlreadyPresent,
			empty:   true,
		},
		{
			name: "the record of a request no window reached",
			commit: func(loop *branchWorkerEventLoop) {
				serviceAttach(loop, commitEmptyReq("alice", message))
				forceDue(loop)
				loop.endWake(0)
			},
			outcome: FinalizeNoOpenWindow,
			empty:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker, serverRepo, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")
			loop := newBranchWorkerEventLoop(worker, time.Hour)
			defer loop.stopTimers()

			// A first commit, pushed, so the branch exists and the push cooldown is running: from
			// here on only the explicit pushPending calls below publish anything.
			writeTo(loop, "existing")
			require.True(t, loop.finalizeOpenWindow())
			loop.pushPending()
			require.Empty(t, loop.pendingWrites)

			originalPush := pushAtomicFn
			pushAtomicFn = func(
				_ context.Context, _ *gogit.Repository, _ plumbing.Hash,
				_ plumbing.ReferenceName, _ []gitclient.Option,
			) (PushOutcome, error) {
				return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
			}
			originalFetch := fetchRemoteBranchHashFn
			fetchRemoteBranchHashFn = func(
				_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ []gitclient.Option,
			) (plumbing.Hash, error) {
				return worker.pushCycleRootHash, nil // unmoved: not contention, so no replay
			}
			restore := func() {
				pushAtomicFn = originalPush
				fetchRemoteBranchHashFn = originalFetch
			}
			defer restore()

			tc.commit(loop)
			require.Len(t, loop.pendingWrites, 1, "the request rides one retained write")

			for range 3 {
				loop.pushPending()
				require.Len(t, loop.pendingWrites, 1, "a failed push retains the write")
				serviceAttach(loop, commitEmptyReq("alice", message)) // the controller's re-send
				_, resolved := outcome(t, worker)
				require.False(t, resolved, "the remote has neither accepted nor refused it")
				assert.Equal(t, PhaseWaitingForPush,
					worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))
				require.Len(t, loop.pendingWrites, 1, "and the re-send made no second write")
			}

			restore()
			loop.pushPending()

			res, ok := outcome(t, worker)
			require.True(t, ok, "the push that succeeds settles it")
			require.NoError(t, res.Err)
			assert.Equal(t, tc.outcome, res.Outcome)
			if tc.empty {
				requirePushedEmptyCommit(t, serverRepo, res, message)
				return
			}
			head, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
			require.NoError(t, err)
			assert.Equal(t, head.Hash().String(), res.Commit)
		})
	}
}

func TestCommitEmpty_AFailedRecordFailsTheRequest(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// No GitTarget exists for the request, so the record cannot be built: the request asked for its
	// message to be recorded, and reporting a benign NoWindow would hide that it was not.
	serviceAttach(loop, commitEmptyReq("alice", "save"))
	forceDue(loop)
	loop.endWake(0)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.Error(t, res.Err, "a record that could not be made fails the request")
	assert.Empty(t, res.Commit)
}

// TestCommitEmpty_ARecordThatFailedToPushLandsInOrderAfterTheRemoteMoved pins WHERE a retried empty
// commit ends up. While its push keeps failing, the author makes another commit behind it and
// another writer moves the remote. The push that finally succeeds must replay both onto the new tip
// in the order they were made: the record first, still empty and still carrying its message, and
// the later commit on top of it. The request resolves with the replayed record's SHA, not the head's
// and not its stale pre-replay hash.
func TestCommitEmpty_ARecordThatFailedToPushLandsInOrderAfterTheRemoteMoved(t *testing.T) {
	const message = "save: recorded before the outage"
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "existing")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()
	require.Empty(t, loop.pendingWrites)

	originalPush := pushAtomicFn
	pushAtomicFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.Hash,
		_ plumbing.ReferenceName, _ []gitclient.Option,
	) (PushOutcome, error) {
		return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
	}
	originalFetch := fetchRemoteBranchHashFn
	fetchRemoteBranchHashFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ []gitclient.Option,
	) (plumbing.Hash, error) {
		return worker.pushCycleRootHash, nil // unmoved: not contention, so no replay yet
	}
	restore := func() {
		pushAtomicFn = originalPush
		fetchRemoteBranchHashFn = originalFetch
	}
	defer restore()

	// The record, then a failed push.
	serviceAttach(loop, commitEmptyReq("alice", message))
	forceDue(loop)
	loop.endWake(0)
	require.Len(t, loop.pendingWrites, 1)
	staleRecordSHA := loop.pendingWrites[0].CommitSHA
	loop.pushPending()
	require.Len(t, loop.pendingWrites, 1, "a failed push retains the record")

	// A later commit of alice's queues up behind it, and another writer moves the remote.
	writeTo(loop, "after-the-record")
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 2)
	pushCompetingCommit(t, remoteURL)
	competing, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	restore()
	loop.pushPending()
	require.Empty(t, loop.pendingWrites, "the replayed push publishes both")

	head, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	landed := commitsAfterHash(t, serverRepo, head.Hash(), competing.Hash())
	require.Len(t, landed, 2, "both commits sit on top of the other writer's, which is kept")

	record, later := landed[0], landed[1]
	assert.Equal(t, message, record.Message, "the record comes first")
	competingCommit, err := serverRepo.CommitObject(competing.Hash())
	require.NoError(t, err)
	assert.Equal(t, competingCommit.TreeHash, record.TreeHash, "and it is still empty after the replay")
	assert.NotEqual(t, message, later.Message, "the later commit is on top of it")
	assert.NotEqual(t, record.TreeHash, later.TreeHash, "and carries its own change")

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
	assert.Equal(t, record.Hash.String(), res.Commit, "the request reports the replayed record")
	assert.NotEqual(t, staleRecordSHA.String(), res.Commit, "never its pre-replay hash")
}

// TestCommitEmpty_AZeroAttachTimeoutStillSeesAnotherAuthorsWindow pins the refusal for a request
// that never waits. With attachTimeout: 0s its deadline has passed by the time any servicing pass
// runs, so the foreign window has to be noted at registration: otherwise bob's request resolves
// NoWindow, and with CommitEmpty records an empty commit while alice's work is still in flight.
func TestCommitEmpty_AZeroAttachTimeoutStillSeesAnotherAuthorsWindow(t *testing.T) {
	for _, commitEmpty := range []bool{false, true} {
		name := "Resolve"
		if commitEmpty {
			name = "CommitEmpty"
		}
		t.Run(name, func(t *testing.T) {
			worker, _, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")
			loop := newBranchWorkerEventLoop(worker, time.Hour)
			defer loop.stopTimers()

			writeTo(loop, "alices-edit")
			require.NotNil(t, loop.openWindow)

			req := commitEmptyReq("bob", "bob's save")
			req.AttachTimeout = 0
			req.CommitEmpty = commitEmpty
			serviceAttach(loop, req)

			res, ok := outcome(t, worker)
			require.True(t, ok, "a request that never waits resolves on the pass that registered it")
			assert.Equal(t, FinalizeWindowMismatch, res.Outcome)
			assert.Empty(t, res.Commit)
			assert.Empty(t, loop.pendingWrites, "no record was made")
			require.NotNil(t, loop.openWindow, "and alice's window is left alone")
			assert.Nil(t, loop.openWindow.pendingCR)
		})
	}
}

// TestCommitEmpty_ARecordOnARepositoryWithNoHistoryKeepsItsCause pins the root-commit case. On a
// remote with no commits yet, the record is the branch's first commit: it has no parent to compare
// with, and its empty tree is what makes it empty. It must still resolve NoOpenWindow, the cause,
// and not Committed, which would claim the save saw writes.
func TestCommitEmpty_ARecordOnARepositoryWithNoHistoryKeepsItsCause(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorkerOnEmptyRemote(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, commitEmptyReq("alice", "save: the first commit"))
	forceDue(loop)
	loop.endWake(0)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome, "the cause is kept on a root commit too")
	require.NotEmpty(t, res.Commit)

	commit, err := serverRepo.CommitObject(plumbing.NewHash(res.Commit))
	require.NoError(t, err)
	assert.Equal(t, 0, commit.NumParents(), "it is the branch's first commit")
	tree, err := commit.Tree()
	require.NoError(t, err)
	assert.Empty(t, tree.Entries, "and it changes no file")
}

// TestCommitEmpty_ABufferFlushStillServesTheWaitingSaves pins the order in the write path: a write
// that opens a window AND trips the buffer limit is flushed at once, so the waiting saves must be
// served before the flush. Otherwise a waiting save misses the very write it waited for, and a save
// waiting on another author never sees the window that refuses it.
func TestCommitEmpty_ABufferFlushStillServesTheWaitingSaves(t *testing.T) {
	t.Run("a waiting Next save takes the write that trips the limit", func(t *testing.T) {
		worker, serverRepo, _ := setupCommitPushSplitWorker(t)
		createPlainGitTarget(t, worker, "team-a", "team-a")
		worker.branchBufferMaxBytes = 1
		loop := newBranchWorkerEventLoop(worker, time.Hour)
		defer loop.stopTimers()

		req := commitEmptyReq("alice", "save: the flushed write")
		req.Attach = configv1alpha3.AttachNext
		serviceAttach(loop, req)
		writeTo(loop, "oversized")
		assert.Nil(t, loop.openWindow, "the buffer limit flushed the window")
		loop.endWake(0)
		loop.pushPending()

		res, ok := outcome(t, worker)
		require.True(t, ok)
		require.NoError(t, res.Err)
		assert.Equal(t, FinalizeCommitted, res.Outcome, "the save took the write, not NoWindow")
		commit, err := serverRepo.CommitObject(plumbing.NewHash(res.Commit))
		require.NoError(t, err)
		assert.Equal(t, "save: the flushed write", commit.Message)
	})

	t.Run("another author's waiting CommitEmpty save sees the flushed window", func(t *testing.T) {
		worker, _, _ := setupCommitPushSplitWorker(t)
		createPlainGitTarget(t, worker, "team-a", "team-a")
		worker.branchBufferMaxBytes = 1
		loop := newBranchWorkerEventLoop(worker, time.Hour)
		loop.lastPushAt = time.Now() // hold the pushes, so the retained writes can be counted
		defer loop.stopTimers()

		serviceAttach(loop, commitEmptyReq("bob", "bob's save"))
		writeTo(loop, "oversized") // alice's write, flushed in the step that opened it
		require.Len(t, loop.pendingWrites, 1, "only alice's commit")
		forceDue(loop)
		loop.endWake(0)

		res, ok := outcome(t, worker)
		require.True(t, ok)
		assert.Equal(t, FinalizeWindowMismatch, res.Outcome)
		assert.Empty(t, res.Commit)
		assert.Len(t, loop.pendingWrites, 1, "no record was made while alice's work was in flight")
	})
}
