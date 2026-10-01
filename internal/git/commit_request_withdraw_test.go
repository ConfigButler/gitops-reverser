// SPDX-License-Identifier: Apache-2.0

package git

// The controller withdraws a CommitRequest before failing it closed. These pin the worker's half of
// that agreement: a request the worker has not acted on is cancelled for good, and a request it
// holds (attached, or committed) is not, so a request reported failed is never committed later.

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithdraw_AWaitingRequestIsCancelledForGood(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	serviceAttach(loop, req)
	require.Equal(t, PhaseWaitingForWindow, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	loop.handleQueueItem(WorkItem{Withdraw: req})

	res, ok := outcome(t, worker)
	require.True(t, ok, "a withdrawn request resolves")
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
	assert.Empty(t, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	// A late re-send of the attach, and a window it would have attached to, change nothing.
	serviceAttach(loop, req)
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("late", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	assert.Nil(t, loop.openWindow.pendingCR, "a withdrawn request never attaches")
	assert.Empty(t, loop.pendingCRs)
	res, _ = outcome(t, worker)
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
}

// A withdraw that arrives before the worker ever saw the attach leaves a tombstone, so the attach
// that was still in flight cannot register afterwards.
func TestWithdraw_AnUnregisteredRequestCannotRegisterLater(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	loop.handleQueueItem(WorkItem{Withdraw: req})
	serviceAttach(loop, req)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
	assert.Empty(t, loop.pendingCRs)
}

// A request attached to a window is held: the withdraw is a no-op, and the request still resolves
// with its commit. This is the case the controller reads as "keep waiting".
func TestWithdraw_AnAttachedRequestIsHeldAndStillCommits(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	req := attachReq("alice", time.Hour)
	serviceAttach(loop, req)
	require.Equal(t, PhaseCollectingWindow, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	loop.handleQueueItem(WorkItem{Withdraw: req})

	_, resolved := outcome(t, worker)
	require.False(t, resolved, "a held request is not withdrawn")
	assert.True(t, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName).Held())

	forceDue(loop)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
}

// A request that already resolved keeps its outcome.
func TestWithdraw_AResolvedRequestKeepsItsOutcome(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", 0)
	serviceAttach(loop, req) // no window: resolves NoOpenWindow at once
	before, ok := outcome(t, worker)
	require.True(t, ok)

	loop.handleQueueItem(WorkItem{Withdraw: req})

	after, _ := outcome(t, worker)
	assert.Equal(t, before, after)
}

func TestEnqueueWithdraw_QueueFullIsDroppedAndNilIsANoOp(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	w.EnqueueWithdraw(nil)
	assert.Empty(t, w.eventQueue)

	w.eventQueue <- WorkItem{} // saturate
	w.EnqueueWithdraw(attachReq("alice", 0))
	assert.Zero(t, w.inflightItems.Load(), "a dropped withdraw must not leak an inflight count")
}

func TestCommitRequestPhase_Held(t *testing.T) {
	assert.True(t, PhaseCollectingWindow.Held())
	assert.True(t, PhaseWaitingForPush.Held())
	assert.False(t, PhaseWaitingForWindow.Held())
	assert.False(t, PhaseWaitingForWorker.Held())
	assert.False(t, CommitRequestPhase("").Held())
}
