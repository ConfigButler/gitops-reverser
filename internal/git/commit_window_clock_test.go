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
	loop.lastPushAt = time.Now() // hold the push, so the local commit stays inspectable

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
	loop.lastPushAt = time.Now()

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

func TestWindowClock_AnAttachedRequestsTimersRunFromItsOwnWriteOnTheWorkersClock(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()
	loop.defaultWindow.maxDuration = time.Hour

	writeTo(loop, "first")
	clock.advance(10 * time.Second)
	req := attachReq("alice", time.Minute)
	req.MaxDuration = 30 * time.Second
	serviceAttach(loop, req)
	require.NotNil(t, loop.openWindow.pendingCR, "precondition: attached")
	assert.Equal(t, clock.Now().Add(time.Minute+30*time.Second), loop.openWindow.timers.maxAt,
		"until its own write, the window closes no later than the attach deadline plus maxDuration")

	clock.advance(20 * time.Second)
	writeTo(loop, "second")
	assert.Equal(t, clock.Now().Add(30*time.Second), loop.openWindow.timers.maxAt,
		"the request's own write starts its maxDuration")

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

// TestWindowClock_ARequestAfterItsWritesClosesOnTheTargetsTimers pins the objects-first folder: a
// request attached to a window already open, with no write after it, leaves the window on the
// GitTarget's idle interval, restarted at the attach, and the window still carries its message. On a
// GitTarget whose idle interval is longer, the attach deadline plus maxDuration closes it.
func TestWindowClock_ARequestAfterItsWritesClosesOnTheTargetsTimers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		idle   time.Duration
		closes time.Duration
	}{
		{name: "the target's idle interval", idle: 5 * time.Second, closes: 5 * time.Second},
		{name: "the attach deadline plus maxDuration", idle: 5 * time.Minute, closes: 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, _, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")
			clock := useTestClock(worker)
			loop := newBranchWorkerEventLoop(worker, tc.idle)
			defer loop.stopTimers()
			loop.defaultWindow.maxDuration = time.Hour

			writeTo(loop, "first")
			clock.advance(3 * time.Second)
			req := attachReq("alice", 5*time.Second)
			req.MaxDuration = 2 * time.Second
			req.Message = "save after the writes"
			serviceAttach(loop, req)

			clock.advance(tc.closes - time.Millisecond)
			timerWake(loop)
			require.NotNil(t, loop.openWindow, "open until %s after the attach", tc.closes)
			clock.advance(time.Millisecond)
			timerWake(loop)
			assert.Nil(t, loop.openWindow)
			res, ok := outcome(t, worker)
			require.True(t, ok)
			assert.Equal(t, FinalizeCommitted, res.Outcome)
		})
	}
}

// TestWindowClock_AZeroAttachTimeoutStartsTheTimersAtTheAttach pins that a request asking for no
// write after it keeps the rule of a window it opens: its timers start at the attach.
func TestWindowClock_AZeroAttachTimeoutStartsTheTimersAtTheAttach(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	clock := useTestClock(worker)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	writeTo(loop, "first")
	clock.advance(time.Second)
	req := attachReq("alice", 0)
	req.MaxDuration = 2 * time.Second
	serviceAttach(loop, req)
	require.NotNil(t, loop.openWindow.pendingCR)
	assert.Nil(t, loop.openWindow.awaitingWrite)
	assert.Equal(t, clock.Now().Add(2*time.Second), loop.openWindow.timers.maxAt)
	assert.True(t, loop.openWindow.timers.noIdle)
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
