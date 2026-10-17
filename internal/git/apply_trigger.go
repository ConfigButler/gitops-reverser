// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the branch worker's half of the apply trigger: deciding WHEN the appliers of the
// GitTargets on this branch apply. See docs/design/apply-trigger.md.
//
// An apply is safe only when nothing live is missing from the commit being applied, and only the
// workers on this branch can see the edits that are live and not yet pushed: their open commit
// windows and pending writes. So an attempt goes out only on a QUIET branch, with neither on this
// worker or on any other worker writing the same repository and branch. Three things owe one:
//
//   - ForeignCommit: an observation of the remote found the branch somewhere this worker did not
//     push it. Every applier on the branch is asked as soon as the branch is quiet, and an open
//     window is closed rather than waited out, because a user who keeps editing would otherwise
//     hold a Git-side change back for as long as they type.
//   - Refusal: a refused edit on a target with spec.onRefusal: Reapply (refusal_reapply.go).
//   - CatchUp: this worker's own pushes. Flux applies on its own timers and on a restart from
//     whatever artifact it holds, and an artifact behind these pushes would revert edits already in
//     Git. Asked only once the branch has stayed quiet for the applier's quiet period, and never of
//     an Argo CD Application: with automated sync off it applies nothing on its own, so it holds no
//     stale state to correct (ApplierRef.CatchesUp).
//
// The three relations are kept apart on purpose. The BRANCH is the unit of quietness, because a
// Kustomization applies the tip whichever GitTarget's folder an edit is in. The APPLIER is the unit
// of obligation: one attempt in flight at a time, its own retry clock, and one attempt for all the
// GitTargets that name it. The GITTARGET, by uid, is the unit of configuration and consent.
//
// The worker owns every obligation until it is delivered. The requester carries out one attempt and
// reports back; it never retries, so a retry is decided here like any first attempt, on a quiet
// branch, against what the targets are now.

// applyDebt is what the branch's appliers are owed because of what was OBSERVED about the remote,
// and the reports of attempts that came back. It is written from other goroutines (the first clone
// publishes an observation before the loop starts; the requester reports from its own), hence the
// lock.
type applyDebt struct {
	mu sync.Mutex
	// known is whether anything has been observed yet, and tip is the last commit observed. Empty
	// means the remote does not carry the branch.
	known bool
	tip   string
	// foreign and catchUp are owed until the loop takes them on a quiet branch.
	foreign bool
	catchUp bool
	// results are the reports of finished attempts, until the loop reads them.
	results []ApplyResult
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

// foreignOwed reports whether a ForeignCommit is owed, without taking it.
func (d *applyDebt) foreignOwed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foreign
}

// tipIs reports whether the last commit observed is commit.
func (d *applyDebt) tipIs(commit string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.known && d.tip == commit
}

func (d *applyDebt) addResult(res ApplyResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results = append(d.results, res)
}

func (d *applyDebt) takeResults() []ApplyResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	results := d.results
	d.results = nil
	return results
}

// applierRetryInterval is how soon a wake retries after it could not list the branch's appliers.
const applierRetryInterval = 10 * time.Second

// branchTarget is one GitTarget on this branch that names an applier, as listed on this pass.
type branchTarget struct {
	ref         itypes.ResourceReference
	uid         k8stypes.UID
	applier     ApplierRef
	quietPeriod time.Duration
	// consents is whether it asks for a Refusal request now (ConsentsToReapply).
	consents bool
}

// appliersOnBranch lists the GitTargets on this worker's (provider, branch) that name an applier. A
// GitTarget names its GitProvider in its own namespace, so the provider's namespace is the only one
// to look in. The client is cached, so this costs no round trip.
func (w *BranchWorker) appliersOnBranch(ctx context.Context) ([]branchTarget, error) {
	var targets v1alpha3.GitTargetList
	if err := w.Client.List(ctx, &targets, client.InNamespace(w.GitProviderNamespace)); err != nil {
		return nil, fmt.Errorf("list GitTargets: %w", err)
	}
	listed := make([]branchTarget, 0, len(targets.Items))
	for i := range targets.Items {
		target := &targets.Items[i]
		if !target.DeletionTimestamp.IsZero() || target.Spec.Applier == nil ||
			target.Spec.GitProviderRef.Name != w.GitProviderRef || target.Spec.Branch != w.Branch {
			continue
		}
		listed = append(listed, branchTarget{
			ref:         itypes.NewResourceReference(target.Name, target.Namespace),
			uid:         target.UID,
			applier:     ApplierOf(target),
			quietPeriod: target.Spec.Applier.EffectiveQuietPeriod(),
			consents:    ConsentsToReapply(target),
		})
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].ref.String() < listed[j].ref.String() })
	return listed, nil
}

// applierState is what one applier on this branch is owed, and how its attempts are going.
type applierState struct {
	ref ApplierRef
	// targets are the GitTargets that name it, as listed on the last pass.
	targets []branchTarget
	// foreign and catchUp are owed until an attempt carries them.
	foreign bool
	catchUp bool
	// inflight is the attempt the requester has not reported yet. While it is out, nothing else is
	// sent to this applier: what is owed meanwhile waits for the report.
	inflight *inflightAttempt
	// failures counts attempts that did not deliver since the last that did, and retryAt is when the
	// next attempt may go.
	failures int
	retryAt  time.Time
	// window is the applier's apply window: open from sending an attempt until the applier
	// finished it (apply_window.go). Nil when no attempt was sent, or the last asked for no apply.
	window *applyWindow
}

// inflightAttempt is what an attempt carries away from its applier's obligations. A failed attempt
// gives it back (restoreAttempt); a delivered one keeps it.
type inflightAttempt struct {
	id       uint64
	foreign  bool
	catchUp  bool
	refusals map[refusalKey]owedRefusal
}

// quietPeriod is how long the branch must be quiet before a CatchUp: the longest among the
// GitTargets naming the applier, because its apply reaches every one of their folders.
func (s *applierState) quietPeriod() time.Duration {
	var longest time.Duration
	for _, target := range s.targets {
		longest = max(longest, target.quietPeriod)
	}
	return longest
}

// writeBusy reports whether the loop holds anything live that is not yet on the remote, or is in a
// state where it cannot say. A request is never sent while it does. A registered CommitRequest
// counts: a save is announced before its writes arrive, and an apply landing between the two would
// revert the save's first edits before they reach Git.
func (l *branchWorkerEventLoop) writeBusy() bool {
	return l.openWindow != nil || len(l.pendingWrites) > 0 || len(l.deferredHeals) > 0 ||
		len(l.pendingCRs) > 0 || l.recovery.active || l.w.worktreeDirty()
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

// serviceAppliers settles the reports that came back, and sends whatever attempts are due if the
// branch is quiet. It runs at the end of every wake, after the wake's writes have been decided and
// committed, so "quiet" is what the wake left behind.
func (l *branchWorkerEventLoop) serviceAppliers() {
	w := l.w
	if w.applyRequester == nil {
		return
	}
	now := time.Now()
	l.settleApplyResults(now)
	if !l.branchQuiet(now) || len(w.eventQueue) > 0 {
		// Queued work: the next wake looks again. The branch is still quiet until that work opens
		// a window, so quietSince is kept.
		return
	}

	tip, foreign, catchUp := w.debt.take()
	if !foreign && !catchUp && len(l.reapplyOwed) == 0 && !l.appliersOwe() {
		l.stopApplierTimer()
		return
	}
	targets, err := w.appliersOnBranch(w.ctx)
	if err != nil {
		w.debt.restore(foreign, catchUp)
		w.Log.Error(err, "Cannot list the appliers on this branch; retrying", "branch", w.Branch)
		l.resetApplierTimer(applierRetryInterval)
		return
	}
	l.syncAppliers(targets)
	for _, state := range l.appliers {
		state.foreign = state.foreign || foreign
		state.catchUp = state.catchUp || (catchUp && state.ref.CatchesUp())
	}
	l.armApplierTimer(now, l.sendDue(tip, now))
}

// branchQuiet reports whether the branch holds nothing live that is not yet on the remote, on this
// worker or on any other writing the same repository and branch, and keeps quietSince: when it last
// became quiet, zero while it is busy.
func (l *branchWorkerEventLoop) branchQuiet(now time.Time) bool {
	busy := l.writeBusy()
	l.w.publishWriteBusy(busy)
	if busy || l.w.siblingWriteBusy() {
		l.quietSince = time.Time{}
		l.stopApplierTimer()
		return false
	}
	if l.quietSince.IsZero() {
		l.quietSince = now
	}
	return true
}

// appliersOwe reports whether any applier is owed something or has an attempt out.
func (l *branchWorkerEventLoop) appliersOwe() bool {
	for _, state := range l.appliers {
		if state.foreign || state.catchUp || state.inflight != nil {
			return true
		}
	}
	return false
}

// syncAppliers makes the applier states match the GitTargets listed now. An applier no GitTarget
// names any more is forgotten, with whatever it was owed: nobody asks for it. A refusal owed for a
// GitTarget that is gone, or is now another incarnation, is dropped the same way.
func (l *branchWorkerEventLoop) syncAppliers(targets []branchTarget) {
	if l.appliers == nil {
		l.appliers = map[ApplierRef]*applierState{}
	}
	named := map[ApplierRef][]branchTarget{}
	uids := map[itypes.ResourceReference]k8stypes.UID{}
	for _, target := range targets {
		named[target.applier] = append(named[target.applier], target)
		uids[target.ref] = target.uid
	}
	for ref := range l.appliers {
		if _, ok := named[ref]; !ok {
			delete(l.appliers, ref)
		}
	}
	for ref, named := range named {
		state := l.appliers[ref]
		if state == nil {
			state = &applierState{ref: ref}
			l.appliers[ref] = state
		}
		state.targets = named
	}
	for key, owed := range l.reapplyOwed {
		if uids[key.target] != owed.uid {
			delete(l.reapplyOwed, key)
		}
	}
}

// sendDue sends an attempt to every applier that is owed one, is not waiting on one, and whose
// retry and quiet periods have run out. It returns the earliest time one still waiting comes due.
func (l *branchWorkerEventLoop) sendDue(tip string, now time.Time) time.Time {
	var earliest time.Time
	wait := func(at time.Time) {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	for _, ref := range l.sortedAppliers() {
		state := l.appliers[ref]
		if state.inflight != nil {
			continue
		}
		refusals := l.refusalsOwed(state)
		if !state.foreign && len(refusals) == 0 && !state.catchUp {
			continue
		}
		due := state.retryAt
		if !state.foreign && len(refusals) == 0 {
			due = latest(due, l.quietSince.Add(state.quietPeriod()))
		}
		if now.Before(due) {
			wait(due)
			continue
		}
		l.sendAttempt(state, tip, refusals)
	}
	return earliest
}

// refusalsOwed is the refusals owed for the GitTargets naming this applier that still consent. A
// refusal of a target that no longer consents is dropped: its consent was the obligation.
func (l *branchWorkerEventLoop) refusalsOwed(state *applierState) map[refusalKey]owedRefusal {
	consents := map[itypes.ResourceReference]bool{}
	for _, target := range state.targets {
		consents[target.ref] = target.consents
	}
	owed := map[refusalKey]owedRefusal{}
	for key, refusal := range l.reapplyOwed {
		consenting, named := consents[key.target]
		if !named {
			continue
		}
		if !consenting {
			delete(l.reapplyOwed, key)
			continue
		}
		owed[key] = refusal
	}
	return owed
}

// sendAttempt hands one attempt for an applier to the requester and moves what it carries out of
// the applier's obligations, into the attempt. Each target is asked for its own reason: every one
// for a ForeignCommit; otherwise a Refusal for a target with a refusal owed, and a CatchUp for the
// rest when one is owed. Any delivered attempt applies the tip, so it answers an owed CatchUp too.
func (l *branchWorkerEventLoop) sendAttempt(state *applierState, tip string, refusals map[refusalKey]owedRefusal) {
	refused := map[itypes.ResourceReference]bool{}
	for key := range refusals {
		refused[key.target] = true
	}
	var targets []ApplyTarget
	var reason v1alpha3.ApplyReason
	for _, target := range state.targets {
		var own v1alpha3.ApplyReason
		switch {
		case state.foreign:
			own = v1alpha3.ApplyReasonForeignCommit
		case refused[target.ref]:
			own = v1alpha3.ApplyReasonRefusal
		case state.catchUp:
			own = v1alpha3.ApplyReasonCatchUp
		default:
			continue
		}
		targets = append(targets, ApplyTarget{Ref: target.ref, UID: target.uid, Reason: own})
		reason = StrongerApplyReason(reason, own)
	}

	l.nextApplyID++
	attempt := &inflightAttempt{
		id: l.nextApplyID, foreign: state.foreign, catchUp: state.catchUp, refusals: refusals,
	}
	state.foreign, state.catchUp, state.inflight = false, false, attempt
	state.openApplyWindow(attempt.id, tip, time.Now())
	for key := range refusals {
		delete(l.reapplyOwed, key)
	}

	w := l.w
	w.Log.Info("Asking the applier to apply",
		"branch", w.Branch, "applier", state.ref.String(), "reason", string(reason), "commit", tip,
		"gitTargets", len(targets), "priorFailures", state.failures)
	w.applyRequester.RequestApply(ApplyRequest{
		ID:            attempt.id,
		Branch:        BranchKey{RepoNamespace: w.GitProviderNamespace, RepoName: w.GitProviderRef, Branch: w.Branch},
		Applier:       state.ref,
		Reason:        reason,
		Commit:        tip,
		Targets:       targets,
		PriorFailures: state.failures,
		Current:       func() bool { return w.decisionHolds(tip) },
		Report:        w.reportApplyResult,
	})
}

// settleApplyResults reads the reports that came back. A delivered attempt is done. One that did
// not deliver gives back what it carried and waits out its applier's backoff, so its retry is
// decided again on a quiet branch at whatever the tip is then, never replayed blind. A report for an
// applier no GitTarget names any more is dropped with it.
func (l *branchWorkerEventLoop) settleApplyResults(now time.Time) {
	for _, res := range l.w.debt.takeResults() {
		state := l.applierAwaiting(res.ID)
		if state == nil {
			continue
		}
		attempt := state.inflight
		state.inflight = nil
		state.settleApplyWindow(attempt.id, res.Outcome == ApplyDelivered, now)
		switch res.Outcome {
		case ApplyDelivered, ApplyCurrent:
			state.failures, state.retryAt = 0, time.Time{}
			continue
		case ApplyStale:
			// The decision went stale before the patch: nothing failed, and the next quiet moment
			// decides again, at the tip as it is then.
			l.restoreAttempt(state, attempt)
			continue
		case ApplyFailed, ApplyRejected, ApplyDropped:
		}
		state.failures++
		state.retryAt = now.Add(applierRetryDelay(state.failures, res.Outcome == ApplyRejected))
		l.restoreAttempt(state, attempt)
		l.w.Log.Info("An apply attempt did not deliver; deciding it again on a quiet branch",
			"branch", l.w.Branch, "applier", state.ref.String(), "outcome", string(res.Outcome),
			"failures", state.failures, "retryIn", state.retryAt.Sub(now), "error", errString(res.Err))
	}
}

// restoreAttempt gives an applier back what an attempt that did not deliver carried. A refusal comes
// back only while it still stands: one that recovered while the attempt was out has nothing left to
// revert.
func (l *branchWorkerEventLoop) restoreAttempt(state *applierState, attempt *inflightAttempt) {
	state.foreign = state.foreign || attempt.foreign
	state.catchUp = state.catchUp || attempt.catchUp
	for key, refusal := range attempt.refusals {
		if !l.w.refusalStands(key) {
			continue
		}
		if l.reapplyOwed == nil {
			l.reapplyOwed = map[refusalKey]owedRefusal{}
		}
		if _, owed := l.reapplyOwed[key]; !owed {
			l.reapplyOwed[key] = refusal
		}
	}
}

// applierAwaiting is the applier whose attempt has this id out, or nil.
func (l *branchWorkerEventLoop) applierAwaiting(id uint64) *applierState {
	for _, state := range l.appliers {
		if state.inflight != nil && state.inflight.id == id {
			return state
		}
	}
	return nil
}

// decisionHolds is ApplyRequest.Current for an attempt decided at tip: the branch is still at that
// commit, and nothing live is waiting to be published on this worker or a sibling. Work still in the
// queue counts, because a queued watch event is an edit that is live and not yet decided. It is
// called from the requester's goroutine, so it reads only what is safe to read there.
func (w *BranchWorker) decisionHolds(tip string) bool {
	return w.debt.tipIs(tip) && !w.busyWriting.Load() && len(w.eventQueue) == 0 && !w.siblingWriteBusy()
}

// reportApplyResult is ApplyRequest.Report: it records the report and wakes the loop to settle it.
// It is called from the requester's goroutine.
func (w *BranchWorker) reportApplyResult(res ApplyResult) {
	w.debt.addResult(res)
	w.wakeApplier()
}

// wakeApplier wakes the loop without queueing a work item, so the wake neither counts as queued
// work nor makes the branch look busy. A wake already pending is enough.
func (w *BranchWorker) wakeApplier() {
	select {
	case w.applyWake <- struct{}{}:
	default:
	}
}

func (l *branchWorkerEventLoop) sortedAppliers() []ApplierRef {
	refs := make([]ApplierRef, 0, len(l.appliers))
	for ref := range l.appliers {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs
}

// armApplierTimer points the applier timer at a deadline, or stops it when there is none.
func (l *branchWorkerEventLoop) armApplierTimer(now, at time.Time) {
	if at.IsZero() {
		l.stopApplierTimer()
		return
	}
	l.resetApplierTimer(at.Sub(now))
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

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
