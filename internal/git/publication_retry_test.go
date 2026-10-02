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

// firePushTimer does what the event loop does when the push timer fires.
func firePushTimer(l *branchWorkerEventLoop) {
	l.pushTimer = nil
	l.pushPending()
}

func seedMain(t *testing.T) func(string) {
	return func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
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
	require.NotNil(t, f.loop.pushTimer, "the failure armed the push timer")
	assert.Equal(t, clock.Now().Add(publicationRetryInitialBackoff), f.loop.publicationRetry.nextAttempt)

	clock.advance(publicationRetryInitialBackoff)
	firePushTimer(f.loop)

	assert.Equal(t, int32(2), attempts.Load())
	assert.False(t, f.ref("feature").IsZero(), "published with no further write")
	assert.Empty(t, f.loop.pendingWrites)
	assert.False(t, f.loop.publicationRetry.pending(), "a success closes the retry")
	assert.Nil(t, f.loop.pushTimer)
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
	require.NotNil(t, f.loop.pushTimer, "and the retry stays scheduled")

	clock.advance(publicationRetryInitialBackoff)
	firePushTimer(f.loop)
	require.Equal(t, int32(2), attempts.Load(), "the retry fails again")
	assert.Equal(t, clock.Now().Add(2*publicationRetryInitialBackoff), f.loop.publicationRetry.nextAttempt,
		"and the next wait is twice as long")

	clock.advance(publicationRetryInitialBackoff)
	liveWrite(f.loop, "cm4")
	assert.Equal(t, int32(2), attempts.Load(), "still not due")

	clock.advance(publicationRetryInitialBackoff)
	firePushTimer(f.loop)
	assert.Equal(t, int32(3), attempts.Load())
	assert.Empty(t, f.loop.pendingWrites, "all four writes published in one push")
	assert.False(t, f.ref("feature").IsZero())
	assert.False(t, f.loop.publicationRetry.pending())
}

// The schedule doubles up to its cap, and a settled publication starts it over.
func TestPublicationRetry_BackoffDoublesToTheCapAndResets(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard()}
	clock := useTestClock(w)
	loop := newBranchWorkerEventLoop(w, time.Second)
	t.Cleanup(loop.stopTimers)

	var waits []time.Duration
	for range 8 {
		loop.notePublicationFailed()
		waits = append(waits, loop.publicationRetry.nextAttempt.Sub(clock.Now()))
	}
	assert.Equal(t, []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}, waits)

	loop.notePublicationSettled()
	assert.False(t, loop.publicationRetry.pending())
	loop.notePublicationFailed()
	assert.Equal(t, clock.Now().Add(publicationRetryInitialBackoff), loop.publicationRetry.nextAttempt)
}

// A push that fails because the parent branch is missing belongs to parent recovery, which retries
// it on the worker's probe schedule. A second schedule here would spend the connections that one
// exists to save.
func TestPublicationRetry_ParentRecoveryOwnsAMissingParent(t *testing.T) {
	f := newRealServerNewBranch(t, "retry-parent", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "branch", "release", "main")
	})
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
	assert.False(t, f.loop.publicationRetry.pending(), "and the publication retry stays out of it")
	assert.Nil(t, f.loop.pushTimer)
}

// The user-facing half, on the real event loop: a CommitRequest riding a write whose push failed is
// held (the controller never fails a held request), so before this it stayed WaitingForPush until
// some unrelated write came along. It now resolves Committed on its own.
func TestPublicationRetry_AHeldCommitRequestIsCommittedWithoutAnotherWrite(t *testing.T) {
	original := publicationRetryInitialBackoff
	publicationRetryInitialBackoff = 50 * time.Millisecond
	t.Cleanup(func() { publicationRetryInitialBackoff = original })

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
