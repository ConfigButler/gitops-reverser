// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the worker's recovery from a parent branch that is missing or does not resolve:
// work it could not write then, and the obligation to write it once the parent exists.
//
// An observation flip is not an obligation. "The parent is Found again" says nothing about whether
// the writes held back meanwhile have landed: the first attempt after it can fail transiently, the
// push timer is stopped by a failed push, the refresher skips a branch that holds work, and with
// --git-refresh-interval=0 there is no refresher at all. So the worker latches an obligation the
// moment a cycle fails on the parent, and clears it only when the work is published.
//
// The obligation has two halves:
//
//   - Retained writes: the loop's pendingWrites. Nothing extra is needed to remember them; the
//     obligation is open while any are held, and pushPending publishing them is what closes it.
//   - Dropped writes, per (GitTarget, collection) scope: a live window or a resync the worker had
//     to drop because the parent was missing. The worker cannot re-derive what the cluster holds,
//     so it asks the controller for a snapshot (SnapshotRequestSeq), and a scope clears only when
//     a resync of it is published: pushed, or settled as a no-op. A successful resync reply alone
//     is not enough, because its write may still be waiting for the push. Scopes are per
//     collection because a ConfigMap snapshot of target A proves nothing about a Deployment of A,
//     or about target B on the same worker.
//
// The worker probes for the parent itself, on the event loop, with one advertisement per deadline:
// 10s, doubling, capped at 5m, shared by every target on the worker. Until the parent is found,
// nothing else may spend a connection on it: a refresh tick before the deadline is a no-op, and a
// live write or resync fails at once without a fetch. Reconciles never probe.

const (
	parentProbeInitialBackoff = 10 * time.Second
	parentProbeMaxBackoff     = 5 * time.Minute
	parentProbeBackoffFactor  = 2
)

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

// recoveryScope is one slice of a GitTarget whose writes were dropped. A zero collection is a write
// whose collection is unknown, which any resync of the target covers.
type recoveryScope struct {
	target     itypes.ResourceReference
	collection itypes.CollectionKey
}

// parentRecovery is the loop's half of the obligation. Loop-goroutine only.
type parentRecovery struct {
	// active is the latch.
	active bool
	// found records that the parent was seen since the latch was set (or since it was last lost).
	found bool
	// gen is the parent generation the latch was set under; a change makes the probe due at once.
	gen uint64

	// scopes are the dropped writes still owed a published snapshot.
	scopes map[recoveryScope]struct{}
	// awaitingPush are scopes a resync has re-derived into a commit that is not pushed yet.
	awaitingPush map[recoveryScope]struct{}

	backoff     time.Duration
	nextProbeAt time.Time
	timer       *time.Timer
}

// now is the loop's clock, injectable so the probe schedule can be tested without sleeping.
func (w *BranchWorker) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// noteParentUnavailable latches the obligation when err was caused by the parent, and remembers
// the scopes whose writes were dropped for it. Any other error is not this obligation's concern.
func (l *branchWorkerEventLoop) noteParentUnavailable(err error, scopes ...recoveryScope) {
	if !isParentUnavailable(err) {
		return
	}
	r := &l.recovery
	switch {
	case !r.active:
		r.active = true
		r.backoff = parentProbeInitialBackoff
		r.nextProbeAt = l.w.now().Add(r.backoff)
		r.gen = l.w.parentSnapshot().gen
	case r.found:
		// Found, then lost again on the attempt that followed: wait a whole backoff before asking.
		r.nextProbeAt = l.w.now().Add(r.backoff)
	}
	r.found = false
	for _, scope := range scopes {
		if r.scopes == nil {
			r.scopes = map[recoveryScope]struct{}{}
		}
		r.scopes[scope] = struct{}{}
		// A snapshot of this scope that is still waiting for its push was taken before this write
		// was dropped, so publishing it proves nothing about this write. The scope needs a new one.
		delete(r.awaitingPush, scope)
	}
	l.armRecoveryTimer()
	l.publishRecovery()
}

// windowScopes are the scopes of a dropped live window: one per collection its events came from.
func windowScopes(namespace, name string, events []Event) []recoveryScope {
	target := itypes.NewResourceReference(name, namespace)
	seen := map[itypes.CollectionKey]bool{}
	var scopes []recoveryScope
	for i := range events {
		collection := events[i].SourceCollection
		if !seen[collection] {
			seen[collection] = true
			scopes = append(scopes, recoveryScope{target: target, collection: collection})
		}
	}
	return scopes
}

// atomicScope is the scope of a dropped atomic write.
func atomicScope(request *WriteRequest) recoveryScope {
	name, namespace := atomicRefusalTarget(request)
	return recoveryScope{target: itypes.NewResourceReference(name, namespace), collection: request.sourceCollection()}
}

// resyncScope is the scope of a resync.
func resyncScope(req *ResyncRequest) recoveryScope {
	return recoveryScope{
		target:     itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace),
		collection: req.refusalCollection(),
	}
}

// noteResyncApplied records that a resync of one scope succeeded. A resync succeeds only after
// fetching the parent, so it is also proof the parent is back. A no-op settles the scope now; one
// that committed settles it when its commit is pushed.
func (l *branchWorkerEventLoop) noteResyncApplied(namespace, name string, collection itypes.CollectionKey,
	committed bool) {
	r := &l.recovery
	if !r.active {
		return
	}
	target := itypes.NewResourceReference(name, namespace)
	for scope := range r.scopes {
		if scope.target != target || !resyncCovers(collection, scope.collection) {
			continue
		}
		if committed {
			if r.awaitingPush == nil {
				r.awaitingPush = map[recoveryScope]struct{}{}
			}
			r.awaitingPush[scope] = struct{}{}
		} else {
			delete(r.scopes, scope)
		}
	}
	l.closeRecoveryIfDone()
}

// resyncCovers reports whether a resync of one collection re-derives a dropped scope: the same
// collection, or either side unknown (a resync of the whole target, or a write whose collection is
// not known).
func resyncCovers(resynced, dropped itypes.CollectionKey) bool {
	var unknown itypes.CollectionKey
	return resynced == dropped || resynced == unknown || dropped == unknown
}

// noteRecoveryPublished runs after every successful push: the scopes whose snapshot it carried are
// settled, and the retained writes it published were the other half.
func (l *branchWorkerEventLoop) noteRecoveryPublished() {
	r := &l.recovery
	if !r.active {
		return
	}
	for scope := range r.awaitingPush {
		delete(r.scopes, scope)
	}
	r.awaitingPush = nil
	l.closeRecoveryIfDone()
}

// closeRecoveryIfDone clears the latch once nothing is owed.
func (l *branchWorkerEventLoop) closeRecoveryIfDone() {
	r := &l.recovery
	if !r.active || len(l.pendingWrites) > 0 || len(r.scopes) > 0 {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	*r = parentRecovery{}
	l.publishRecovery()
}

// probeDue reports whether the next background look at the remote may be spent now.
func (l *branchWorkerEventLoop) probeDue() bool {
	r := &l.recovery
	return r.active && (!l.w.now().Before(r.nextProbeAt) || r.gen != l.w.parentSnapshot().gen)
}

// runParentProbe is the deadline: while the parent is missing it spends one advertisement; once it
// is found it retries the retained writes and asks again for the snapshots still owed. Whatever
// remains open is scheduled for the next deadline.
func (l *branchWorkerEventLoop) runParentProbe() {
	r := &l.recovery
	if !r.active {
		return
	}
	if gen := l.w.parentSnapshot().gen; gen != r.gen {
		r.gen, r.found, r.backoff = gen, false, parentProbeInitialBackoff
	}
	if !r.found {
		missing, err := l.probeParent()
		if err != nil || missing {
			if err != nil {
				l.w.Log.V(1).Info("Parent branch probe failed", "branch", l.w.Branch, "error", err.Error())
			}
			l.scheduleNextProbe()
			return
		}
		r.found = true
		r.backoff = parentProbeInitialBackoff
		l.publishRecovery()
		l.w.Log.Info("The parent branch is on the remote again; recovering held-back work",
			"branch", l.w.Branch, "parentBranch", l.w.ParentBranch(),
			"pendingWrites", len(l.pendingWrites), "scopes", len(r.scopes))
	}
	if len(l.pendingWrites) > 0 {
		l.pushPending()
	}
	if r.active && r.found {
		for target := range l.recoveryTargets() {
			l.w.bumpSnapshotRequest(target)
		}
	}
	if r.active {
		l.scheduleNextProbe()
	}
}

// recoveryTargets are the GitTargets owed a snapshot.
func (l *branchWorkerEventLoop) recoveryTargets() map[itypes.ResourceReference]struct{} {
	targets := map[itypes.ResourceReference]struct{}{}
	for scope := range l.recovery.scopes {
		if _, waiting := l.recovery.awaitingPush[scope]; !waiting {
			targets[scope.target] = struct{}{}
		}
	}
	return targets
}

// scheduleNextProbe doubles the backoff and moves the deadline that far on: the first deadline is
// parentProbeInitialBackoff after the latch, the next twice that after it, up to the cap.
func (l *branchWorkerEventLoop) scheduleNextProbe() {
	r := &l.recovery
	r.backoff = min(parentProbeBackoffFactor*r.backoff, parentProbeMaxBackoff)
	r.nextProbeAt = l.w.now().Add(r.backoff)
	l.armRecoveryTimer()
	l.publishRecovery()
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

func (l *branchWorkerEventLoop) armRecoveryTimer() {
	r := &l.recovery
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if !r.active {
		return
	}
	r.timer = time.NewTimer(max(r.nextProbeAt.Sub(l.w.now()), 0))
}

func (l *branchWorkerEventLoop) recoveryTimerC() <-chan time.Time {
	if l.recovery.timer == nil {
		return nil
	}
	return l.recovery.timer.C
}

// publishRecovery mirrors the latch for the readers outside the loop: the controller, and the
// fetch gate the write path consults.
func (l *branchWorkerEventLoop) publishRecovery() {
	r := &l.recovery
	l.w.parentRecoveryOpen.Store(r.active)
	l.w.parentRecoveryFound.Store(r.active && r.found)
	if r.active && !r.found {
		l.w.parentProbeHold.Store(&parentProbeHold{until: r.nextProbeAt, gen: r.gen})
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

// bumpSnapshotRequest asks the controller for a fresh snapshot of one GitTarget.
func (w *BranchWorker) bumpSnapshotRequest(target itypes.ResourceReference) {
	w.snapshotRequestsMu.Lock()
	defer w.snapshotRequestsMu.Unlock()
	if w.snapshotRequests == nil {
		w.snapshotRequests = map[itypes.ResourceReference]uint64{}
	}
	w.snapshotRequests[target]++
}

// BumpSnapshotRequestForTest asks for a snapshot of the GitTarget, as the recovery probe does, for
// tests in another package.
func (w *BranchWorker) BumpSnapshotRequestForTest(target itypes.ResourceReference) {
	w.bumpSnapshotRequest(target)
}

// SnapshotRequestSeq is how many times the worker has asked for a fresh snapshot of the GitTarget,
// to re-derive writes it dropped while the parent branch was missing. The controller forces one
// recheck each time it rises, and remembers the value it acted on.
func (w *BranchWorker) SnapshotRequestSeq(target itypes.ResourceReference) uint64 {
	w.snapshotRequestsMu.Lock()
	defer w.snapshotRequestsMu.Unlock()
	return w.snapshotRequests[target]
}

// ParentRecovery reports whether the worker still owes work held back by a missing parent (the
// first result), and whether the parent has been found since (the second).
func (w *BranchWorker) ParentRecovery() (bool, bool) {
	return w.parentRecoveryOpen.Load(), w.parentRecoveryFound.Load()
}
