// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// This file is the worker's recovery from a parent branch that is missing or does not resolve: the
// obligation to write what was decided while it was gone, once it exists.
//
// An observation flip is not an obligation. "The parent is Found again" says nothing about whether
// the writes decided meanwhile have landed: the first attempt after it can fail transiently, the
// refresher skips a branch that holds work, and with --git-refresh-interval=0 there is no refresher
// at all. So the worker latches an obligation the moment an attempt fails on the parent, and clears
// it only when the log is published.
//
// The obligation is the log itself. A write decided while the parent is missing stays in the log
// like any other a remote failure holds back (branch_log.go), and admission backpressure bounds the
// log however long the parent stays away, so nothing is dropped and no snapshot is owed.
//
// The worker probes for the parent itself, on the event loop, with one advertisement per deadline of
// its one retry schedule (retry.go), shared by every target on the worker. Until the parent is
// found, nothing else may spend a connection on it: a refresh tick before the deadline is a no-op,
// and a write or resync is decided without a fetch. Reconciles never probe.

// errAwaitingParentProbe is the answer to a cycle that would fetch for a parent already known to be
// missing, before the next probe is due. It is a missing parent as far as every caller is
// concerned, and costs no connection.
var errAwaitingParentProbe = fmt.Errorf("%w (unchanged since the last look; waiting for the next probe)",
	ErrParentBranchNotFound)

// isParentUnavailable reports an error caused by the parent a new write branch would be created
// from: a configured parent that is absent, or a default branch that does not resolve.
func isParentUnavailable(err error) bool {
	return errors.Is(err, ErrParentBranchNotFound) || errors.Is(err, ErrDefaultBranchUnresolved)
}

// parentRecovery is the loop's latch on the obligation. Loop-goroutine only.
type parentRecovery struct {
	// active is the latch.
	active bool
	// found records that the parent was seen since the latch was set (or since it was last lost).
	found bool
	// gen is the parent generation the latch was set under; a change makes the probe due at once.
	gen uint64
}

// now is the loop's clock, injectable so the retry schedule can be tested without sleeping.
func (w *BranchWorker) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// noteParentUnavailable latches the obligation when err was caused by the parent and the log holds
// work it owes. Any other error is not this obligation's concern, and neither is a failure that left
// nothing owed: an obligation with nothing to publish would never close. It does not schedule the
// next attempt: whoever observed the failure does, once (scheduleRetry), and a new latch starts that
// schedule over so the first probe is one initial backoff away.
func (l *branchWorkerEventLoop) noteParentUnavailable(err error) {
	if !isParentUnavailable(err) {
		return
	}
	r := &l.recovery
	if !r.active {
		if len(l.pendingWrites) == 0 {
			return
		}
		r.active = true
		r.gen = l.w.parentSnapshot().gen
		l.clearRetry()
	}
	r.found = false
	l.publishRecovery()
}

// closeRecoveryIfDone clears the latch once the log is published.
func (l *branchWorkerEventLoop) closeRecoveryIfDone() {
	if !l.recovery.active || len(l.pendingWrites) > 0 {
		return
	}
	l.recovery = parentRecovery{}
	l.publishRecovery()
}

// probeDue reports whether the next background look at the remote may be spent now.
func (l *branchWorkerEventLoop) probeDue() bool {
	r := &l.recovery
	return r.active && (!l.w.now().Before(l.retry.due) || r.gen != l.w.parentSnapshot().gen)
}

// runParentProbe is the attempt while the obligation is open: while the parent is missing it spends
// one advertisement, and once it is found it publishes the log. A publication that fails schedules
// the next attempt itself.
func (l *branchWorkerEventLoop) runParentProbe() {
	r := &l.recovery
	if !r.active {
		return
	}
	if gen := l.w.parentSnapshot().gen; gen != r.gen {
		r.gen, r.found = gen, false
		l.retry.backoff = 0
	}
	if !r.found {
		missing, err := l.probeParent()
		if err != nil || missing {
			if err != nil {
				l.w.Log.V(1).Info("Parent branch probe failed", "branch", l.w.Branch, "error", err.Error())
			}
			l.scheduleRetry()
			return
		}
		r.found = true
		l.retry.backoff = 0
		l.publishRecovery()
		l.w.Log.Info("The parent branch is on the remote again; publishing the writes decided meanwhile",
			"branch", l.w.Branch, "parentBranch", l.w.ParentBranch(), "pendingWrites", len(l.pendingWrites))
	}
	l.pushPending()
}

// probeParent reads one advertisement and records what it says. It reports whether the parent is
// still missing; a write branch that now exists is not.
func (l *branchWorkerEventLoop) probeParent() (bool, error) {
	w := l.w
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		return true, fmt.Errorf("get GitProvider: %w", err)
	}
	auth, err := getAuthFromSecret(w.ctx, w.Client, provider, w.sshHostKeys, w.credentialPolicy)
	if err != nil {
		return true, fmt.Errorf("get auth: %w", err)
	}
	parent := w.parentSnapshot()
	advertisement, err := advertiseRemoteBranchFn(
		w.repo.URL,
		plumbing.NewBranchReferenceName(w.Branch),
		parent.name,
		auth,
	)
	if err != nil {
		return true, fmt.Errorf("read the remote advertisement: %w", err)
	}
	return w.recordAdvertisement(advertisement, parent.name), nil
}

// publishRecovery mirrors the latch for the readers outside the loop: the controller, and the
// fetch gate the write path consults.
func (l *branchWorkerEventLoop) publishRecovery() {
	r := &l.recovery
	l.w.parentRecoveryOpen.Store(r.active)
	l.w.parentRecoveryFound.Store(r.active && r.found)
	if r.active && !r.found {
		l.w.parentProbeHold.Store(&parentProbeHold{until: l.retry.due, gen: r.gen})
	} else {
		l.w.parentProbeHold.Store(nil)
	}
}

// parentProbeHold is the window in which a missing parent is not looked for again.
type parentProbeHold struct {
	until time.Time
	gen   uint64
}

// awaitingParentProbe reports that the parent is known missing and the next probe is not due, so
// a cycle that would fetch for it must not. A parent change lifts it.
func (w *BranchWorker) awaitingParentProbe() bool {
	hold := w.parentProbeHold.Load()
	return hold != nil && hold.gen == w.parentSnapshot().gen && w.now().Before(hold.until)
}

// ParentRecovery reports whether the worker still owes work decided while the parent branch was
// missing (the first result), and whether the parent has been found since (the second).
func (w *BranchWorker) ParentRecovery() (bool, bool) {
	return w.parentRecoveryOpen.Load(), w.parentRecoveryFound.Load()
}
