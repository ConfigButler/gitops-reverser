// SPDX-License-Identifier: Apache-2.0

package git

// A failed publication schedules its own retry: a branch that goes quiet after the failure still
// publishes, a branch that keeps receiving commits does not spend a push on each of them, and a
// failure caused by a missing parent stays with parent recovery's schedule. These pin all three,
// and the CommitRequest that rides the retained write.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failNextPushes makes the next n push attempts fail as an unreachable remote would, and counts every
// attempt, failed or not.
func failNextPushes(t *testing.T, n int32) *atomic.Int32 {
	t.Helper()
	original := pushAtomicFn
	var attempts atomic.Int32
	pushAtomicFn = func(ctx context.Context, repo *git.Repository, rootHash plumbing.Hash,
		rootBranch plumbing.ReferenceName, auth []gitclient.Option) (PushOutcome, error) {
		if attempts.Add(1) <= n {
			return PushOutcome{}, errors.New("dial tcp: connection refused")
		}
		return original(ctx, repo, rootHash, rootBranch, auth)
	}
	t.Cleanup(func() { pushAtomicFn = original })
	return &attempts
}

// fireRetry does what the event loop does when the retry deadline fires.
func fireRetry(l *branchWorkerEventLoop) {
	l.retry.timer = nil
	l.runRetry()
}

func seedMain(t *testing.T) func(string) {
	return func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
	}
}

func seedMainAndRelease(t *testing.T) func(string) {
	return func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "branch", "release", "main")
	}
}

// The gap this closes: after one failed push, a branch that receives nothing else used to keep its
// work indefinitely. The retry is armed by the failure itself.
func TestPublicationRetry_AQuietBranchPublishesAfterAFailedPush(t *testing.T) {
	f := newRealServerNewBranch(t, "retry-quiet", seedMain(t))
	clock := useTestClock(f.worker)
	attempts := failNextPushes(t, 1)

	liveWrite(f.loop, "cm1")
	require.Equal(t, int32(1), attempts.Load(), "the first push was attempted, and failed")
	require.Len(t, f.loop.pendingWrites, 1, "the work is retained")
	assert.True(t, f.ref("feature").IsZero())
	require.NotNil(t, f.loop.retry.timer, "the failure armed the retry")
	assert.Equal(t, clock.Now().Add(retryInitialBackoff), f.loop.retry.due)

	clock.advance(retryInitialBackoff)
	fireRetry(f.loop)

	assert.Equal(t, int32(2), attempts.Load())
	assert.False(t, f.ref("feature").IsZero(), "published with no further write")
	assert.Empty(t, f.loop.pendingWrites)
	assert.False(t, f.loop.retry.pending(), "a success closes the retry")
	assert.Nil(t, f.loop.retry.timer)
}

// The mirror image: lastPushAt advances only on success, so before this an expired cooldown let
// every new commit try the remote that had just refused. Commits now accumulate until the retry is
// due, and a second failure doubles the wait.
func TestPublicationRetry_NewCommitsDoNotBypassTheBackoff(t *testing.T) {
	f := newRealServerNewBranch(t, "retry-arrivals", seedMain(t))
	clock := useTestClock(f.worker)
	attempts := failNextPushes(t, 2)

	liveWrite(f.loop, "cm1")
	require.Equal(t, int32(1), attempts.Load())

	liveWrite(f.loop, "cm2")
	liveWrite(f.loop, "cm3")
	assert.Equal(t, int32(1), attempts.Load(), "commits before the deadline do not push")
	require.Len(t, f.loop.pendingWrites, 3)
	require.NotNil(t, f.loop.retry.timer, "and the retry stays scheduled")

	clock.advance(retryInitialBackoff)
	fireRetry(f.loop)
	require.Equal(t, int32(2), attempts.Load(), "the retry fails again")
	assert.Equal(t, clock.Now().Add(2*retryInitialBackoff), f.loop.retry.due,
		"and the next wait is twice as long")

	clock.advance(retryInitialBackoff)
	liveWrite(f.loop, "cm4")
	assert.Equal(t, int32(2), attempts.Load(), "still not due")

	clock.advance(retryInitialBackoff)
	fireRetry(f.loop)
	assert.Equal(t, int32(3), attempts.Load())
	assert.Empty(t, f.loop.pendingWrites, "all four writes published in one push")
	assert.False(t, f.ref("feature").IsZero())
	assert.False(t, f.loop.retry.pending())
}

// The schedule doubles up to its cap, and a settled publication starts it over.
func TestPublicationRetry_BackoffDoublesToTheCapAndResets(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	clock := useTestClock(w)
	loop := newBranchWorkerEventLoop(w, time.Second)
	t.Cleanup(loop.stopTimers)

	var waits []time.Duration
	for range 8 {
		loop.scheduleRetry()
		waits = append(waits, loop.retry.due.Sub(clock.Now()))
	}
	assert.Equal(t, []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}, waits)

	loop.clearRetry()
	assert.False(t, loop.retry.pending())
	loop.scheduleRetry()
	assert.Equal(t, clock.Now().Add(retryInitialBackoff), loop.retry.due)
}

// A push that fails because the parent branch is missing opens parent recovery, and its next attempt
// is the probe, on the worker's one retry schedule: there is no second schedule to spend the
// connections the probe exists to save.
func TestPublicationRetry_AMissingParentIsProbedOnTheOneSchedule(t *testing.T) {
	f := newRealServerNewBranch(t, "retry-parent", seedMainAndRelease(t))
	useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))

	f.loop.lastPushAt = time.Now()
	liveWrite(f.loop, "cm1")
	gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release")
	f.loop.pushPending()

	require.Len(t, f.loop.pendingWrites, 1)
	open, _ := f.worker.ParentRecovery()
	require.True(t, open, "parent recovery holds the work")
	require.True(t, f.loop.retry.pending(), "the next attempt is scheduled")
	assert.Equal(t, f.worker.now().Add(retryInitialBackoff), f.loop.retry.due,
		"a new latch starts the schedule over: the probe is one initial backoff away")
	assert.Nil(t, f.loop.pushTimer, "the push timer is only the success cooldown")
}

// Once the parent is back, recovery's missing-parent hold is gone but its obligation stays open
// until the work publishes. A publication that fails then must not be retried by every new commit:
// the commits wait for recovery's next deadline. Found in review of #412; it failed on main too.
func TestPublicationRetry_AFailureAfterTheParentReturnsWaitsForRecovery(t *testing.T) {
	f := newRealServerNewBranch(t, "retry-parent-found", seedMainAndRelease(t))
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	f.loop.lastPushAt = time.Now()
	liveWrite(f.loop, "cm1")
	gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release")
	f.loop.pushPending()
	require.True(t, f.loop.recovery.active)
	require.False(t, f.loop.recovery.found)

	gitIn(t, f.repoDir, "branch", "release", "main") // the parent returns
	attempts := failNextPushes(t, 100)
	clock.advance(retryInitialBackoff)
	f.loop.lastPushAt = time.Time{}
	f.loop.runParentProbe()
	require.True(t, f.loop.recovery.found)
	require.Equal(t, int32(1), attempts.Load(), "recovery's attempt failed")
	require.True(t, clock.Now().Before(f.loop.retry.due))

	liveWrite(f.loop, "cm2")
	liveWrite(f.loop, "cm3")
	assert.Equal(t, int32(1), attempts.Load(), "commits before recovery's deadline do not retry")

	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe()
	assert.Equal(t, int32(2), attempts.Load(), "recovery's next deadline does")
}

// The user-facing half, on the real event loop: a CommitRequest riding a write whose push failed is
// held (the controller never fails a held request), so before this it stayed WaitingForPush until
// some unrelated write came along. It now resolves Committed on its own.
func TestPublicationRetry_AHeldCommitRequestIsCommittedWithoutAnotherWrite(t *testing.T) {
	original := retryInitialBackoff
	retryInitialBackoff = 50 * time.Millisecond
	t.Cleanup(func() { retryInitialBackoff = original })

	f := newRealServerNewBranch(t, "retry-held-commit-request", seedMain(t))
	attempts := failNextPushes(t, 1)
	require.NoError(t, f.worker.Start(context.Background()))
	t.Cleanup(f.worker.Stop)

	require.True(t, f.worker.Enqueue(configMapTargetEvent("cm1", "alice", newBranchTarget)))
	f.worker.EnqueueAttach(standbyRequest())

	require.Eventually(t, func() bool {
		_, resolved := outcome(t, f.worker)
		return resolved
	}, 10*time.Second, 20*time.Millisecond, "the request resolves with no further write")
	res, _ := outcome(t, f.worker)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Equal(t, f.ref("feature").String(), res.Commit)
	assert.Equal(t, int32(2), attempts.Load(), "one failed push, then the retry")
}
