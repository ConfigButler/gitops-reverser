// SPDX-License-Identifier: Apache-2.0

package git

import "time"

// This file is the worker's retry of a publication that failed: retained writes whose push, or
// whose rebuild before the push, did not succeed.
//
// The cooldown in maybeSchedulePush spaces SUCCESSFUL pushes; it says nothing about failed ones,
// because lastPushAt advances only on success. Without a schedule of its own a failure had two
// outcomes, both wrong: a branch that then went quiet kept its work indefinitely (and any
// CommitRequest riding it stayed WaitingForPush, since the controller never fails a request the
// worker holds), and a branch that kept receiving commits spent one failed push on each of them.
//
// So a failure opens a retry: the next attempt is due one backoff later, 10s doubling to 5m, and
// the push timer is armed for it whether or not anything else arrives. Until then new commits
// accumulate locally instead of pushing. A successful push, or nothing left to push, closes it.
//
// A failure caused by a missing parent is not retried here. Parent recovery owns that work, with
// its own probe schedule shared across the worker's targets (parent_recovery.go); retrying it on a
// second schedule would spend connections that schedule exists to save. The same holds for any
// failure while that obligation is open: its deadline retries the retained writes.

//nolint:gochecknoglobals // vars, not consts, so a test can run the real loop on a short schedule.
var (
	publicationRetryInitialBackoff = 10 * time.Second
	publicationRetryMaxBackoff     = 5 * time.Minute
)

const publicationRetryBackoffFactor = 2

// publicationRetry is the loop's schedule for retrying a failed publication. The zero value is
// "no failure outstanding". Loop-goroutine only.
type publicationRetry struct {
	backoff     time.Duration
	nextAttempt time.Time
}

// pending reports whether a failed publication is waiting for its next attempt.
func (r publicationRetry) pending() bool {
	return !r.nextAttempt.IsZero()
}

// notePublicationFailed schedules the next attempt after a failed push or rebuild, unless parent
// recovery owns the retry.
func (l *branchWorkerEventLoop) notePublicationFailed() {
	if l.recovery.active {
		l.publicationRetry = publicationRetry{}
		return
	}
	r := &l.publicationRetry
	if r.backoff == 0 {
		r.backoff = publicationRetryInitialBackoff
	} else {
		r.backoff = min(publicationRetryBackoffFactor*r.backoff, publicationRetryMaxBackoff)
	}
	r.nextAttempt = l.w.now().Add(r.backoff)
	l.armPushTimerAt(r.nextAttempt)
}

// notePublicationSettled closes the retry: the work was published, or nothing is retained.
func (l *branchWorkerEventLoop) notePublicationSettled() {
	l.publicationRetry = publicationRetry{}
}

// awaitingPublicationRetry reports that a failed publication's next attempt is not due yet, so a
// new commit must not push early. It makes sure the attempt is still scheduled.
func (l *branchWorkerEventLoop) awaitingPublicationRetry() bool {
	r := l.publicationRetry
	if !r.pending() || !l.w.now().Before(r.nextAttempt) {
		return false
	}
	if l.pushTimer == nil {
		l.armPushTimerAt(r.nextAttempt)
	}
	return true
}

// armPushTimerAt (re)arms the push timer to fire at the given time on the worker's clock.
func (l *branchWorkerEventLoop) armPushTimerAt(at time.Time) {
	l.stopPushTimer()
	l.pushTimer = time.NewTimer(max(at.Sub(l.w.now()), 0))
}
