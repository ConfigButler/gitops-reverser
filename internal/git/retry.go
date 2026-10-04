// SPDX-License-Identifier: Apache-2.0

package git

import "time"

// This file is the worker's one retry schedule: the deadline at which the work it still owes is
// attempted again, whatever made the last attempt fail.
//
// The cooldown in maybeSchedulePush spaces SUCCESSFUL pushes; it says nothing about failed ones,
// because lastPushAt advances only on success. Without a schedule of its own a failure had two
// outcomes, both wrong: a branch that then went quiet kept its work indefinitely (and any
// CommitRequest riding it stayed WaitingForPush, since the controller never fails a request the
// worker holds), and a branch that kept receiving commits spent one failed attempt on each of them.
//
// So a failed attempt schedules the next one: due one backoff later, 10s doubling to 5m, on a timer
// of its own, whether or not anything else arrives. Until then new commits accumulate instead of
// pushing. A successful publication, or nothing left owed, clears it.
//
// It used to be two schedules, one for a failed push and one for parent recovery's probe, and the
// two had to hand the work back and forth: a failure while recovery was open deferred to recovery's
// deadline, or every new commit retried at once. There is one deadline now, and what is due when it
// fires decides what the attempt is: while the parent is missing, one advertisement looking for it
// (parent_recovery.go); otherwise, materializing the log and pushing it.

//nolint:gochecknoglobals // vars, not consts, so a test can run the real loop on a short schedule.
var (
	retryInitialBackoff = 10 * time.Second
	retryMaxBackoff     = 5 * time.Minute
)

const retryBackoffFactor = 2

// retrySchedule is the loop's one retry deadline. The zero value is "nothing owed a retry".
// Loop-goroutine only.
type retrySchedule struct {
	backoff time.Duration
	due     time.Time
	timer   *time.Timer
	// cause is the failure the schedule is waiting out: what the last attempt met. A caller that
	// asks for the remote before the attempt is due hears it instead of spending a connection to
	// learn it again.
	cause error
	// since is when the first of these failed attempts was made. It stays put across retries, so
	// the report of the outage does not change with every attempt.
	since time.Time
}

// pending reports whether a failed attempt is waiting for its next one.
func (r *retrySchedule) pending() bool { return !r.due.IsZero() }

// scheduleRetry makes the next attempt due one backoff from now, doubling the backoff since the last
// failure up to its cap. Whoever observes a failed attempt calls it, once for that attempt, with the
// failure it met.
func (l *branchWorkerEventLoop) scheduleRetry(cause error) {
	r := &l.retry
	r.cause = cause
	if r.due.IsZero() {
		r.since = l.w.now()
	}
	if r.backoff == 0 {
		r.backoff = retryInitialBackoff
	} else {
		r.backoff = min(retryBackoffFactor*r.backoff, retryMaxBackoff)
	}
	r.due = l.w.now().Add(r.backoff)
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.NewTimer(max(r.due.Sub(l.w.now()), 0))
	l.publishRecovery()
}

// endOutage closes the retry and parent recovery together: a publication landed, or nothing is owed
// any more. Either ends whatever the last failed attempt was waiting out, and with it the pause on
// intake (intake.go).
func (l *branchWorkerEventLoop) endOutage() {
	l.recovery = parentRecovery{}
	l.clearRetry()
}

// clearRetry closes the schedule. It also publishes the recovery latch, which reads the schedule's
// deadline.
func (l *branchWorkerEventLoop) clearRetry() {
	if l.retry.timer != nil {
		l.retry.timer.Stop()
	}
	l.retry = retrySchedule{}
	l.publishRecovery()
}

// awaitingRetry reports that a failed attempt's next one is not due yet, so a new commit must not
// push early.
func (l *branchWorkerEventLoop) awaitingRetry() bool {
	return l.retry.pending() && l.w.now().Before(l.retry.due)
}

// runRetry is the deadline. While parent recovery is open, the attempt is its probe; otherwise it
// is the publication, which settles the schedule or moves it on.
func (l *branchWorkerEventLoop) runRetry() {
	if l.recovery.active {
		l.runParentProbe()
		return
	}
	l.pushPending()
}

func (l *branchWorkerEventLoop) retryTimerC() <-chan time.Time {
	if l.retry.timer == nil {
		return nil
	}
	return l.retry.timer.C
}
