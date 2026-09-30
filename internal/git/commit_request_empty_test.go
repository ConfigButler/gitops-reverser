// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	loop.serviceCommitRequests()
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
	loop.serviceCommitRequests()

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
	loop.serviceCommitRequests()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
	assert.Empty(t, res.Commit)
	assert.Empty(t, loop.pendingWrites)
}
