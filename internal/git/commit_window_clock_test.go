// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin that the commit windows' timers read the worker's clock: a test steps it past a
// deadline, delivers the wake of each timer the worker armed for it, and the window or the waiting
// request is due, with no sleep and no rewound deadline.

// timerWake delivers the wakes the commit and attach timers would, as run does: each timer the worker
// has armed whose time on the worker's clock has come. A deadline no timer was armed for passes.
func timerWake(loop *branchWorkerEventLoop) {
	now := loop.w.now()
	if loop.commitTimer != nil && !loop.commitTimerAt.After(now) {
		loop.commitTimer = nil
		loop.closeOrArmWindow()
		loop.endWake(0)
	}
	if loop.attachTimer != nil && !loop.attachTimerAt.After(now) {
		loop.attachTimer = nil
		loop.endWake(0)
	}
}

func TestWindowClock_TheIdleTimeoutIsMeasuredOnTheWorkersClock(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, 10*time.Second)
	defer loop.stopTimers()
	holdPushes(loop) // hold the push, so the local commit stays inspectable

	writeTo(loop, "first")
	require.NotNil(t, loop.openWindow)
	assert.Equal(t, clock.Now(), loop.openWindow.openedAt, "the window opens at the clock's time")

	clock.advance(9 * time.Second)
	timerWake(loop)
	require.NotNil(t, loop.openWindow, "a second short of the idle timeout")

	writeTo(loop, "second") // restarts the idle interval
	clock.advance(9 * time.Second)
	timerWake(loop)
	require.NotNil(t, loop.openWindow, "the second write restarted it")

	clock.advance(time.Second)
	timerWake(loop)
	assert.Nil(t, loop.openWindow, "idle for the whole timeout")
	require.Len(t, loop.pendingWrites, 1)
	assert.Len(t, loop.pendingWrites[0].Events, 2)
}

func TestWindowClock_MaxDurationIsMeasuredOnTheWorkersClock(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, 10*time.Second)
	defer loop.stopTimers()
	loop.defaultWindow.maxDuration = 25 * time.Second
	holdPushes(loop)

	for i, name := range []string{"a", "b", "c"} {
		writeTo(loop, name)
		require.NotNil(t, loop.openWindow, "write %d keeps the window busy", i)
		clock.advance(9 * time.Second)
		timerWake(loop)
	}
	assert.Nil(t, loop.openWindow, "27s after it opened, past its 25s maxDuration")
	require.Len(t, loop.pendingWrites, 1)
	assert.Len(t, loop.pendingWrites[0].Events, 3)
}

func TestWindowClock_AnAttachedRequestsTimersRunFromTheAttachOnTheWorkersClock(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "first")
	clock.advance(time.Minute)
	req := attachReq("alice", time.Hour)
	req.MaxDuration = 30 * time.Second
	serviceAttach(loop, req)
	require.NotNil(t, loop.openWindow.pendingCR, "precondition: attached")
	assert.Equal(t, clock.Now().Add(30*time.Second), loop.openWindow.timers.maxAt)

	clock.advance(29 * time.Second)
	timerWake(loop)
	require.NotNil(t, loop.openWindow)

	clock.advance(time.Second)
	timerWake(loop)
	assert.Nil(t, loop.openWindow, "the request's maxDuration ran out")
	res, ok := outcome(t, worker)
	require.True(t, ok)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
}

func TestWindowClock_TheAttachDeadlineIsMeasuredOnTheWorkersClock(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, attachReq("alice", 5*time.Second))
	clock.advance(4 * time.Second)
	timerWake(loop)
	_, resolved := outcome(t, worker)
	require.False(t, resolved, "still waiting a second before its deadline")

	clock.advance(time.Second)
	timerWake(loop)
	res, resolved := outcome(t, worker)
	require.True(t, resolved)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
}
