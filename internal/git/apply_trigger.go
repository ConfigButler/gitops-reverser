// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the branch worker's half of the apply trigger: deciding WHEN the appliers of the
// GitTargets on this branch may apply. See docs/design/push-notification-and-reconcile-trigger.md
// part 3.
//
// An apply is safe only when nothing live is missing from the commit being applied, and only this
// worker can see the edits that are live and not yet pushed: they are its open commit window and its
// pending writes. So a request goes out only on a QUIET branch, with neither, and the applier's own
// triggers are switched off by the installer. Three things owe a request:
//
//   - ForeignCommit: an observation of the remote found the branch somewhere this worker did not
//     push it. Every applier on the branch is asked as soon as the branch is quiet, and an open
//     window is closed rather than waited out, because a user who keeps editing would otherwise
//     hold a Git-side change back for as long as they type.
//   - Refusal: a refused edit on a target with spec.onRefusal: Reapply (refusal_reapply.go).
//   - CatchUp: this worker's own pushes. Flux applies on its own timers and on a restart from
//     whatever artifact it holds, and an artifact behind these pushes would revert edits already in
//     Git. Asked only once the branch has stayed quiet for the target's spec.applier.quietPeriod, so
//     the apply does not land in the middle of an editing session.

// applyDebt is what the branch's appliers are owed because of what was OBSERVED about the remote. It
// is fed from publishObservation, the one point every observation passes through, which can run
// before the loop starts (the first clone), hence the lock.
type applyDebt struct {
	mu sync.Mutex
	// known is whether anything has been observed yet, and tip is the last commit observed. Empty
	// means the remote does not carry the branch.
	known bool
	tip   string
	// foreign and catchUp are owed until the loop takes them on a quiet branch.
	foreign bool
	catchUp bool
	// returned holds the requests the requester handed back undelivered, per GitTarget with the
	// reason it was owed, until returnedDue. returnedCount is the most hand-backs any of them has
	// been through. They live here rather than on the loop because the requester hands back from
	// its own goroutine.
	returned      map[itypes.ResourceReference]v1alpha3.ApplyReason
	returnedCount int
	returnedDue   time.Time
}

// observe classifies one observation. The first one is a baseline, not a move: nothing says what
// the appliers last applied, so they are owed a catch-up, never a ForeignCommit. A request on a
// restart would otherwise apply the tip before the watches have captured the edits made while the
// operator was down.
//
// After that, a commit that differs from the last one observed is either this worker's push, proved
// by the push itself, or somebody else's, because every other observation is a fetch. When in doubt
// the classification errs towards foreign, which costs one apply of the tip on a quiet branch.
func (d *applyDebt) observe(observed RemoteObservation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.known {
		d.known, d.tip, d.catchUp = true, observed.Commit, true
		return
	}
	if observed.Commit == d.tip {
		return
	}
	d.tip = observed.Commit
	if observed.By == ObservedByPush {
		d.catchUp = true
		return
	}
	d.foreign = true
}

// take returns what is owed and clears it. The caller restores it if it cannot act on it.
func (d *applyDebt) take() (string, bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tip, foreign, catchUp := d.tip, d.foreign, d.catchUp
	d.foreign, d.catchUp = false, false
	return tip, foreign, catchUp
}

// restore puts back what take returned, for a caller that could not act on it.
func (d *applyDebt) restore(foreign, catchUp bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.foreign = d.foreign || foreign
	d.catchUp = d.catchUp || catchUp
}

// handBack records a request the requester could not deliver, due after its backoff. A target
// handed back twice keeps the stronger reason (StrongerApplyReason).
func (d *applyDebt) handBack(req ApplyRequest, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mergeReturnedLocked(req.Reason, req.Targets, req.HandBacks)
	if due := now.Add(handBackDelay(req.HandBacks)); due.After(d.returnedDue) {
		d.returnedDue = due
	}
}

func (d *applyDebt) mergeReturnedLocked(
	reason v1alpha3.ApplyReason, targets []itypes.ResourceReference, handBacks int,
) {
	if d.returned == nil {
		d.returned = map[itypes.ResourceReference]v1alpha3.ApplyReason{}
	}
	for _, target := range targets {
		d.returned[target] = StrongerApplyReason(d.returned[target], reason)
	}
	d.returnedCount = max(d.returnedCount, handBacks)
}

// takeReturned returns the handed-back requests and clears them, once they are due. Before then it
// returns none, and when they come due.
func (d *applyDebt) takeReturned(now time.Time) (map[itypes.ResourceReference]v1alpha3.ApplyReason, int, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.returned) == 0 {
		return nil, 0, time.Time{}
	}
	if now.Before(d.returnedDue) {
		return nil, 0, d.returnedDue
	}
	returned, count := d.returned, d.returnedCount
	d.returned, d.returnedCount, d.returnedDue = nil, 0, time.Time{}
	return returned, count, time.Time{}
}

// restoreReturned puts back what takeReturned returned, due at once, for a caller that could not
// act on it.
func (d *applyDebt) restoreReturned(returned map[itypes.ResourceReference]v1alpha3.ApplyReason, count int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for target, reason := range returned {
		d.mergeReturnedLocked(reason, []itypes.ResourceReference{target}, count)
	}
}

// foreignOwed reports whether a ForeignCommit is owed, without taking it.
func (d *applyDebt) foreignOwed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foreign
}

// applierRetryInterval is how soon a wake retries after it could not list the branch's appliers.
const applierRetryInterval = 10 * time.Second

// branchApplier is one GitTarget on this branch that names an applier.
type branchApplier struct {
	target      itypes.ResourceReference
	quietPeriod time.Duration
}

// appliersOnBranch lists the GitTargets on this worker's (provider, branch) that name an applier. A
// GitTarget names its GitProvider in its own namespace, so the provider's namespace is the only one
// to look in. The client is cached, so this costs no round trip.
func (w *BranchWorker) appliersOnBranch(ctx context.Context) ([]branchApplier, error) {
	var targets v1alpha3.GitTargetList
	if err := w.Client.List(ctx, &targets, client.InNamespace(w.GitProviderNamespace)); err != nil {
		return nil, fmt.Errorf("list GitTargets: %w", err)
	}
	appliers := make([]branchApplier, 0, len(targets.Items))
	for i := range targets.Items {
		target := &targets.Items[i]
		if !target.DeletionTimestamp.IsZero() || target.Spec.Applier == nil ||
			target.Spec.GitProviderRef.Name != w.GitProviderRef || target.Spec.Branch != w.Branch {
			continue
		}
		appliers = append(appliers, branchApplier{
			target:      itypes.NewResourceReference(target.Name, target.Namespace),
			quietPeriod: target.Spec.Applier.EffectiveQuietPeriod(),
		})
	}
	sort.Slice(appliers, func(i, j int) bool { return appliers[i].target.String() < appliers[j].target.String() })
	return appliers, nil
}

// writeBusy reports whether the loop holds anything live that is not yet on the remote, or is in a
// state where it cannot say. A request is never sent while it does.
func (l *branchWorkerEventLoop) writeBusy() bool {
	return l.openWindow != nil || len(l.pendingWrites) > 0 || len(l.deferredHeals) > 0 ||
		l.recovery.active || l.w.worktreeDirty()
}

// closeWindowForForeignCommit closes the open commit window early when the branch has moved under
// it, or a Git host has said it may have, so its writes are published, and replayed onto the move,
// now rather than when the window's timers allow. See the file comment for why not wait.
func (l *branchWorkerEventLoop) closeWindowForForeignCommit() {
	hinted := l.pushHinted
	l.pushHinted = false
	if l.openWindow == nil || l.w.applyRequester == nil {
		return
	}
	if !hinted && !l.w.debt.foreignOwed() {
		return
	}
	l.w.Log.Info("The branch moved, or may have; closing the commit window to publish it now",
		"branch", l.w.Branch)
	l.finalizeOpenWindowWithReason(windowFinalizeReasonForeignCommit)
}

// serviceAppliers sends whatever apply requests are owed, if the branch is quiet. It runs at the end
// of every wake, after the wake's writes have been decided and committed, so "quiet" is what the
// wake left behind.
func (l *branchWorkerEventLoop) serviceAppliers() {
	w := l.w
	if w.applyRequester == nil {
		return
	}
	if l.writeBusy() {
		l.quietSince = time.Time{}
		l.stopApplierTimer()
		return
	}
	now := time.Now()
	if l.quietSince.IsZero() {
		l.quietSince = now
	}
	if len(w.eventQueue) > 0 {
		// More work is already queued and the next wake will look again. The branch is still quiet
		// until that work opens a window, so quietSince is kept.
		return
	}

	tip, foreign, catchUp := w.debt.take()
	returned, returnedCount, returnedDue := w.debt.takeReturned(now)
	if !foreign && !catchUp && len(returned) == 0 && len(l.reapplyOwed) == 0 && len(l.catchUpOwed) == 0 {
		l.armApplierTimer(now, returnedDue)
		return
	}
	declared, ok := l.declaredAppliers()
	if !ok {
		w.debt.restore(foreign, catchUp)
		w.debt.restoreReturned(returned, returnedCount)
		return
	}
	if catchUp {
		l.oweCatchUp(declared)
	}
	l.sendOwed(tip, foreign, l.oweReturned(returned, returnedCount, declared), declared, now, returnedDue)
}

// sendOwed sends what serviceAppliers found owed on a quiet branch, and arms the timer for what is
// not due yet.
func (l *branchWorkerEventLoop) sendOwed(
	tip string,
	foreign bool,
	returnedForeign []itypes.ResourceReference,
	declared map[itypes.ResourceReference]time.Duration,
	now, returnedDue time.Time,
) {
	if foreign && len(declared) > 0 {
		// The tip is the foreign commit with every pending write replayed onto it: the branch is
		// quiet, so anything that was pending has been published on top. Every applier on the
		// branch is asked, which also answers whatever else they were owed.
		l.requestApply(v1alpha3.ApplyReasonForeignCommit, tip, sortedTargetKeys(declared))
		clear(l.reapplyOwed)
		clear(l.catchUpOwed)
		l.armApplierTimer(now, returnedDue)
		return
	}
	if len(returnedForeign) > 0 {
		for _, target := range returnedForeign {
			delete(l.catchUpOwed, target)
		}
		l.requestApply(v1alpha3.ApplyReasonForeignCommit, tip, returnedForeign)
	}
	l.serviceRefusals(tip, declared)
	l.armApplierTimer(now, l.serviceCatchUp(tip, declared, now), returnedDue)
}

// declaredAppliers maps every GitTarget on this branch that names an applier to its quiet period.
// On a failure to list them a retry is armed, and the caller puts back what it took.
func (l *branchWorkerEventLoop) declaredAppliers() (map[itypes.ResourceReference]time.Duration, bool) {
	appliers, err := l.w.appliersOnBranch(l.w.ctx)
	if err != nil {
		l.w.Log.Error(err, "Cannot list the appliers on this branch; retrying", "branch", l.w.Branch)
		l.resetApplierTimer(applierRetryInterval)
		return nil, false
	}
	declared := make(map[itypes.ResourceReference]time.Duration, len(appliers))
	for _, applier := range appliers {
		declared[applier.target] = applier.quietPeriod
	}
	return declared, true
}

// oweCatchUp records that every declared applier is owed a catch-up with this worker's pushes.
func (l *branchWorkerEventLoop) oweCatchUp(declared map[itypes.ResourceReference]time.Duration) {
	if l.catchUpOwed == nil {
		l.catchUpOwed = map[itypes.ResourceReference]time.Duration{}
	}
	for target, quietPeriod := range declared {
		l.catchUpOwed[target] = quietPeriod
	}
}

// oweReturned owes again what the requester handed back, now that it is due, and returns the
// targets owed a ForeignCommit, which the caller sends at once.
//
// A Refusal is owed again only while the refusal still stands: one that recovered while its
// request was failing has nothing left to revert, and its target is owed a catch-up instead, as is
// a handed-back CatchUp. Both go through the ordinary paths, so a Refusal is sent only with consent
// and a catch-up only after the quiet period.
func (l *branchWorkerEventLoop) oweReturned(
	returned map[itypes.ResourceReference]v1alpha3.ApplyReason,
	count int,
	declared map[itypes.ResourceReference]time.Duration,
) []itypes.ResourceReference {
	var foreign []itypes.ResourceReference
	for _, target := range sortedTargetKeys(returned) {
		quietPeriod, ok := declared[target]
		if !ok {
			continue // its applier was removed since; nothing is owed to it any more
		}
		if l.handBackCount == nil {
			l.handBackCount = map[itypes.ResourceReference]int{}
		}
		l.handBackCount[target] = max(l.handBackCount[target], count)
		switch reason := returned[target]; {
		case reason == v1alpha3.ApplyReasonForeignCommit:
			foreign = append(foreign, target)
		case reason == v1alpha3.ApplyReasonRefusal && l.oweStandingRefusals(target):
		default:
			if l.catchUpOwed == nil {
				l.catchUpOwed = map[itypes.ResourceReference]time.Duration{}
			}
			l.catchUpOwed[target] = quietPeriod
		}
	}
	return foreign
}

// oweStandingRefusals owes target's applier a Refusal request again for every refusal of the
// target that has not recovered, and reports whether there was any.
func (l *branchWorkerEventLoop) oweStandingRefusals(target itypes.ResourceReference) bool {
	standing := l.w.standingRefusals(target)
	if l.reapplyOwed == nil && len(standing) > 0 {
		l.reapplyOwed = map[refusalKey]string{}
	}
	for _, key := range standing {
		if _, owed := l.reapplyOwed[key]; !owed {
			l.reapplyOwed[key] = "a Refusal request that could not be delivered"
		}
	}
	return len(standing) > 0
}

// serviceRefusals sends each owed Refusal request to its target's applier alone, once per target
// however many of its collections are refused.
//
// Consent is read again here. A refusal owed while the branch was busy can wait behind an open
// window or a publication outage for as long as those last, and a target suspended or set back to
// Ignore in the meantime has withdrawn it.
func (l *branchWorkerEventLoop) serviceRefusals(
	tip string, declared map[itypes.ResourceReference]time.Duration,
) {
	targets := map[itypes.ResourceReference]struct{}{}
	for key := range l.reapplyOwed {
		targets[key.target] = struct{}{}
	}
	clear(l.reapplyOwed)
	for _, target := range sortedTargetKeys(targets) {
		if _, ok := declared[target]; !ok {
			continue // its applier was removed since the refusal; consent went with it
		}
		if !l.w.refusalConsent(l.w.ctx, target) {
			continue
		}
		// A reapply applies the tip, which is everything a catch-up would have asked for.
		delete(l.catchUpOwed, target)
		l.requestApply(v1alpha3.ApplyReasonRefusal, tip, []itypes.ResourceReference{target})
	}
}

// serviceCatchUp asks every applier whose quiet period has run out to catch up, and returns when
// the earliest one still waiting comes due (zero when none waits).
func (l *branchWorkerEventLoop) serviceCatchUp(
	tip string, declared map[itypes.ResourceReference]time.Duration, now time.Time,
) time.Time {
	var due []itypes.ResourceReference
	var earliest time.Time
	for target, quietPeriod := range l.catchUpOwed {
		if _, ok := declared[target]; !ok {
			delete(l.catchUpOwed, target)
			continue
		}
		at := l.quietSince.Add(quietPeriod)
		if !now.Before(at) {
			due = append(due, target)
			delete(l.catchUpOwed, target)
			continue
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if len(due) > 0 {
		sortReferences(due)
		l.requestApply(v1alpha3.ApplyReasonCatchUp, tip, due)
	}
	return earliest
}

// armApplierTimer points the applier timer at the earliest of the given deadlines that is set, or
// stops it when none is.
func (l *branchWorkerEventLoop) armApplierTimer(now time.Time, deadlines ...time.Time) {
	var earliest time.Time
	for _, at := range deadlines {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	if earliest.IsZero() {
		l.stopApplierTimer()
		return
	}
	l.resetApplierTimer(earliest.Sub(now))
}

// requestApply hands one request to the requester, which delivers it off this goroutine. It
// carries, and consumes, the hand-back count of its targets.
func (l *branchWorkerEventLoop) requestApply(
	reason v1alpha3.ApplyReason, commit string, targets []itypes.ResourceReference,
) {
	handBacks := 0
	for _, target := range targets {
		handBacks = max(handBacks, l.handBackCount[target])
		delete(l.handBackCount, target)
	}
	l.w.Log.Info("Asking the appliers to apply",
		"branch", l.w.Branch, "reason", string(reason), "commit", commit, "gitTargets", len(targets),
		"handBacks", handBacks)
	l.w.applyRequester.RequestApply(ApplyRequest{
		Branch: BranchKey{
			RepoNamespace: l.w.GitProviderNamespace,
			RepoName:      l.w.GitProviderRef,
			Branch:        l.w.Branch,
		},
		Reason:    reason,
		Commit:    commit,
		Targets:   targets,
		HandBacks: handBacks,
	})
}

func (l *branchWorkerEventLoop) applierTimerC() <-chan time.Time {
	if l.applierTimer == nil {
		return nil
	}
	return l.applierTimer.C
}

func (l *branchWorkerEventLoop) resetApplierTimer(delay time.Duration) {
	l.stopApplierTimer()
	l.applierTimer = time.NewTimer(max(delay, 0))
}

func (l *branchWorkerEventLoop) stopApplierTimer() {
	if l.applierTimer != nil {
		l.applierTimer.Stop()
		l.applierTimer = nil
	}
}

func sortedTargetKeys[V any](set map[itypes.ResourceReference]V) []itypes.ResourceReference {
	targets := make([]itypes.ResourceReference, 0, len(set))
	for target := range set {
		targets = append(targets, target)
	}
	sortReferences(targets)
	return targets
}

func sortReferences(refs []itypes.ResourceReference) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
}
