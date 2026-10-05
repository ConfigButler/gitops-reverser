// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// These tests pin the clock rules of docs/design/commit-timing-surface.md: a window opened by a
// write takes the GitTarget's timers, and a CommitRequest that attaches replaces them, shorter or
// longer. Each one drives the loop directly, so no test waits on a real timer longer than a few
// milliseconds.

// writeTo delivers one write by alice, the author every request in these tests speaks for unless it
// names another.
func writeTo(loop *branchWorkerEventLoop, name string) {
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent(name, "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
}

func namedAttachReq(name string, d time.Duration) *AttachCommitRequest {
	req := attachReq("alice", d)
	req.Name = name
	req.UID = "uid-" + name
	return req
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestWindowTimers_ContinuousActivityStopsAtTheTargetsMaxDuration(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour) // the idle timer never fires here
	defer loop.stopTimers()
	loop.defaultWindow.maxDuration = time.Hour
	loop.lastPushAt = time.Now() // hold the push, so the local commit stays inspectable

	writeTo(loop, "first")
	require.NotNil(t, loop.openWindow)
	// Rewind the deadline rather than sleeping past a short one: a 20ms maxDuration raced the
	// opening write itself, which under a loaded test run took longer than 20ms and closed the
	// window before the assertion above could see it.
	require.WithinDuration(t, time.Now().Add(time.Hour), loop.openWindow.timers.maxAt, time.Minute,
		"the window takes the target's maxDuration when it opens")
	loop.openWindow.timers.maxAt = time.Now().Add(-time.Millisecond)
	writeTo(loop, "second") // restarts idle, but maxDuration has already passed

	assert.Nil(t, loop.openWindow, "a window closes at maxDuration however much keeps arriving")
	require.Len(t, loop.pendingWrites, 1, "both writes are in the one window it closed")
	assert.Len(t, loop.pendingWrites[0].Events, 2)
}

func TestWindowTimers_ATargetMaxDurationOfZeroCommitsEveryWrite(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()
	loop.defaultWindow.maxDuration = 0
	loop.lastPushAt = time.Now()

	writeTo(loop, "first")
	writeTo(loop, "second")

	assert.Nil(t, loop.openWindow)
	assert.Len(t, loop.pendingWrites, 2, "maxDuration 0s closes the window right after the write that opened it")
}

func TestWindowTimers_ARequestIdleTimeoutLongerThanTheTargetsKeepsTheWindowOpen(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, 5*time.Millisecond) // the target closes after 5ms
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	req.IdleTimeout = durationPtr(time.Hour)
	serviceAttach(loop, req)
	writeTo(loop, "first")
	require.NotNil(t, loop.openWindow.pendingCR, "precondition: attached")

	time.Sleep(20 * time.Millisecond)
	loop.closeOrArmWindow() // what the commit timer does when it fires
	loop.endWake(0)

	assert.NotNil(t, loop.openWindow, "the request's longer idleTimeout replaces the target's")
}

func TestWindowTimers_ARequestIdleTimeoutShorterThanTheTargetsClosesSooner(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	req.IdleTimeout = durationPtr(5 * time.Millisecond)
	serviceAttach(loop, req)
	writeTo(loop, "first")
	require.NotNil(t, loop.openWindow.pendingCR, "precondition: attached")

	time.Sleep(20 * time.Millisecond)
	loop.closeOrArmWindow()
	loop.endWake(0)

	assert.Nil(t, loop.openWindow, "the request's shorter idleTimeout replaces the target's hour")
}

func TestWindowTimers_ARequestWithOnlyMaxDurationIgnoresTheTargetsIdleTimer(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, 5*time.Millisecond)
	defer loop.stopTimers()

	serviceAttach(loop, attachReq("alice", time.Hour)) // no idleTimeout
	writeTo(loop, "first")
	time.Sleep(20 * time.Millisecond)
	loop.closeOrArmWindow()
	loop.endWake(0)

	assert.NotNil(t, loop.openWindow, "no idle close: maxDuration alone ends the collection")
}

func TestWindowTimers_AWaitingRequestAttachesBeforeATargetIdleOfZeroCloses(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, 0) // the target commits every write on its own
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	req.Message = "save"
	serviceAttach(loop, req)
	writeTo(loop, "first")

	require.NotNil(t, loop.openWindow,
		"the request attaches in the step the write opens the window, before the zero timer closes it")
	assert.Equal(t, "save", loop.openWindow.pendingMessage)
}

func TestWindowTimers_AZeroAttachTimeoutStillCollectsForMaxDuration(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "first")
	req := attachReq("alice", time.Hour)
	req.AttachTimeout = 0
	serviceAttach(loop, req)

	require.NotNil(t, loop.openWindow, "attachTimeout 0s never forces a zero collection")
	assert.NotNil(t, loop.openWindow.pendingCR, "it attached to the window open at registration")
	_, resolved := outcome(t, worker)
	assert.False(t, resolved)
}

func TestWindowTimers_ARequestMaxDurationOfZeroFinalizesRightAfterTheAttach(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "first")
	req := attachReq("alice", time.Hour)
	req.MaxDuration = 0
	serviceAttach(loop, req)

	assert.Nil(t, loop.openWindow)
	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
}

func TestAttachSelection_CompetingRequestsAreServedFirstComeFirstServed(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// Registered first, with the LATER deadline: order is registration, not deadline.
	serviceAttach(loop, namedAttachReq("early", time.Hour))
	serviceAttach(loop, namedAttachReq("late", time.Minute))
	writeTo(loop, "first")

	require.NotNil(t, loop.openWindow.pendingCR)
	assert.Equal(t, "early", loop.openWindow.pendingCR.Name)
}

func TestAttachSelection_NextClosesTheOpenWindowUnderItsOwnMessage(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	loop.lastPushAt = time.Now()
	writeTo(loop, "earlier")
	current := namedAttachReq("current", time.Hour)
	current.Message = "the earlier save"
	serviceAttach(loop, current)
	require.NotNil(t, loop.openWindow.pendingCR, "precondition: the earlier save is attached")

	next := namedAttachReq("next", time.Hour)
	next.Attach = configv1alpha3.AttachNext
	next.Message = "the new save"
	serviceAttach(loop, next)

	assert.Nil(t, loop.openWindow, "Next closes the author's open window")
	require.Len(t, loop.pendingWrites, 1)
	assert.Equal(t, "the earlier save", loop.pendingWrites[0].CommitMessage,
		"the closed window keeps the message another request attached to it")

	writeTo(loop, "after")
	require.NotNil(t, loop.openWindow)
	require.NotNil(t, loop.openWindow.pendingCR)
	assert.Equal(t, "next", loop.openWindow.pendingCR.Name, "Next attaches to the window the next write opens")

	// A re-send of the same request closes nothing: the close is keyed to its registration.
	serviceAttach(loop, next)
	assert.NotNil(t, loop.openWindow, "a re-send must not close the window it attached to")
	assert.Len(t, loop.pendingWrites, 1)
}

func TestAttachSelection_TheWorkerReportsEachPhase(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()
	phase := func() CommitRequestPhase { return worker.LookupCommitRequestPhase("default", crName, "uid-"+crName) }

	assert.Empty(t, phase(), "nothing is reported before the worker registers the request")
	loop.lastPushAt = time.Now() // keep the push behind its cooldown, so WaitingForPush is observable

	serviceAttach(loop, attachReq("alice", time.Hour))
	assert.Equal(t, PhaseWaitingForWindow, phase())

	writeTo(loop, "first")
	assert.Equal(t, PhaseCollectingWindow, phase())

	forceDue(loop)
	assert.Equal(t, PhaseWaitingForPush, phase())

	loop.pushPending()
	_, resolved := outcome(t, worker)
	require.True(t, resolved)
	assert.Empty(t, phase(), "a resolved request has an outcome, not a phase")
}

// TestWindowTimers_AWaitingRequestWithMaxDurationZeroCommitsExactlyTheNextWrite pins the "save
// first, then make one change" use: a request with attach: Next, a long attachTimeout and
// maxDuration: 0s waits for the next write, and that write alone becomes the commit with the
// request's message. The write after it is ordinary again: its own window, the generated message.
func TestWindowTimers_AWaitingRequestWithMaxDurationZeroCommitsExactlyTheNextWrite(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now() // hold the pushes, so the retained writes can be inspected
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	req.Attach = configv1alpha3.AttachNext
	req.MaxDuration = 0
	req.Message = "save: exactly the next change"
	serviceAttach(loop, req)
	require.Nil(t, loop.openWindow, "the request waits; it opens nothing")
	assert.Equal(t, PhaseWaitingForWindow, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	writeTo(loop, "the-change")
	loop.endWake(0)
	assert.Nil(t, loop.openWindow, "the write opened a window, the request attached, and maxDuration: 0s closed it")
	require.Len(t, loop.pendingWrites, 1)
	assert.Equal(t, req.Message, loop.pendingWrites[0].CommitMessage)
	require.Len(t, loop.pendingWrites[0].Events, 1, "the commit holds that one write")

	writeTo(loop, "the-next-change")
	require.NotNil(t, loop.openWindow, "a later write opens an ordinary window under the target's timers")
	assert.Nil(t, loop.openWindow.pendingCR)
	assert.Empty(t, loop.openWindow.pendingMessage)
}
