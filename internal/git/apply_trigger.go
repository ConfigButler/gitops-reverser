// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//   - CatchUp: this worker's own pushes. A suspended Kustomization applies nothing on its own, so
//     this is convergence, not safety: Flux's inventory learns of an object only from an apply of a
//     commit that holds it, and an object created through the API and never applied is not pruned
//     when Git deletes it. Asked only once the branch has stayed quiet for the trigger's quiet
//     period, and never of an Argo CD Application (decided without it; ApplierRef.CatchesUp).
//   - Repair: the apply window withheld a late apply over a newer API edit (apply_window.go), so
//     the cluster runs an older value than Git. Asked like a Refusal: on a quiet branch, with no
//     quiet period, and of either kind of applier, because nothing else would put the edit back on
//     an Argo CD Application.
//
// The three relations are kept apart on purpose. The BRANCH is the unit of quietness, because a
// Kustomization applies the tip whichever GitTarget's folder an edit is in. The APPLIER is the unit
// of obligation: one attempt in flight at a time, its own retry clock, and one attempt for all the
// GitTargets that name it. The GITTARGET, by uid, is the unit of configuration and consent.
//
// The worker owns every obligation until an operation completes it. The requester carries out one
// attempt and reports its operation as it moves; it never retries, so a retry is decided here like
// any first attempt, on a quiet branch, against what the targets are now. An operation is followed
// for as long as it takes: one past the requester's budget is Unresolved, and keeps its window open
// and its applier's next attempt back, because a timer that closes the window does not stop the
// apply.

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
	// foreign and catchUp are owed until the loop takes them on a quiet branch. prefetch is a push
	// somebody else made, until the loop takes it (apply_groups.go).
	foreign  bool
	catchUp  bool
	prefetch bool
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
	d.foreign, d.prefetch = true, true
}

// takePrefetch returns whether a push somebody else made is to be fetched ahead, with the tip, and
// clears it.
func (d *applyDebt) takePrefetch() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	due := d.prefetch
	d.prefetch = false
	return d.tip, due
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
	// recorded and repairOwed are the trigger's status.operation and status.repairOwed, as listed:
	// what an earlier process, or the trigger's reconciler, recorded (apply_recovery.go).
	recorded   *v1alpha3.ApplyOperation
	repairOwed bool
	// group names the GitRepository a Flux trigger's Kustomization applies, in its cluster; empty
	// for an Argo CD Application or a source not read yet. prefetch is the trigger's
	// status.source.prefetch (apply_groups.go).
	group    string
	prefetch bool
}

// appliersOnBranch lists the GitTargets on this worker's (provider, branch) that name an apply
// trigger that exists, with the applier each trigger holds. A GitTarget names its GitProvider and
// its trigger in its own namespace, so the provider's namespace is the only one to look in. The
// client is cached, so this costs no round trip. A GitTarget whose trigger does not exist (yet)
// names no applier: its ApplyTriggerReady condition says so.
func (w *BranchWorker) appliersOnBranch(ctx context.Context) ([]branchTarget, error) {
	var targets v1alpha3.GitTargetList
	if err := w.Client.List(ctx, &targets, client.InNamespace(w.GitProviderNamespace)); err != nil {
		return nil, fmt.Errorf("list GitTargets: %w", err)
	}
	listed := make([]branchTarget, 0, len(targets.Items))
	for i := range targets.Items {
		target := &targets.Items[i]
		if !target.DeletionTimestamp.IsZero() || target.Spec.ApplyTriggerRef == nil ||
			target.Spec.GitProviderRef.Name != w.GitProviderRef || target.Spec.Branch != w.Branch {
			continue
		}
		trigger, err := ResolveApplyTrigger(ctx, w.Client, target)
		if err != nil {
			return nil, err
		}
		if trigger == nil || !trigger.GetDeletionTimestamp().IsZero() {
			continue
		}
		listed = append(listed, branchTarget{
			ref:         itypes.NewResourceReference(target.Name, target.Namespace),
			uid:         target.UID,
			applier:     ApplierOf(trigger),
			quietPeriod: trigger.EffectiveQuietPeriod(),
			consents:    ConsentsToReapply(target),
			recorded:    trigger.Operation(),
			repairOwed:  trigger.RepairOwed(),
			group:       sourceGroup(trigger),
			prefetch:    prefetchesSource(trigger),
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
	// foreign, catchUp and repair are owed until an attempt carries them.
	foreign bool
	catchUp bool
	repair  bool
	// inflight is the attempt whose report has not ended it yet. While it is out, nothing else is
	// sent to this applier: what is owed meanwhile waits for it.
	inflight *inflightAttempt
	// failures counts attempts that did not succeed since the last that did, and retryAt is when
	// the next attempt may go.
	failures int
	retryAt  time.Time
	// lastTurn is the id of the last attempt sent to it, zero before the first: a group's next
	// attempt goes to the applier whose turn is longest ago (turnOrder).
	lastTurn uint64
	// window is the applier's apply window: open from sending an attempt until the applier
	// finished it (apply_window.go). Nil when no attempt was sent, or the last asked for no apply.
	window *applyWindow
	// operation is the last operation an attempt started, as its trigger's status.operation shows
	// it, and operationID the attempt it belongs to. Nil until this worker started one.
	operation   *v1alpha3.ApplyOperation
	operationID uint64
	// published is what was last handed to the trigger status writer, nil before the first.
	published *ApplyTriggerStatus
	// withdrawn is an operation an attempt recorded on the trigger and then did not start, until
	// it is published as withdrawn.
	withdrawn *v1alpha3.ApplyOperation
	// recovered is whether the trigger's recorded status was read back for this applier, once,
	// when this process first listed it; followedFence is the fence token of the last operation
	// the trigger's reconciler recorded that this worker followed (apply_recovery.go).
	recovered     bool
	followedFence string
}

// repairOwed reports whether a Repair is owed: not yet carried, or carried by the attempt out.
func (s *applierState) repairOwed() bool {
	return s.repair || (s.inflight != nil && s.inflight.repair)
}

// inflightAttempt is what an attempt carries away from its applier's obligations. One that did not
// succeed gives it back (restoreAttempt); one that succeeded keeps it.
type inflightAttempt struct {
	id uint64
	// reason, commit and sentAt are what the attempt asked for, and when, for its operation record.
	reason   v1alpha3.ApplyReason
	commit   string
	sentAt   time.Time
	foreign  bool
	catchUp  bool
	repair   bool
	refusals map[refusalKey]owedRefusal
	// phase is the last report of its operation, empty before the first.
	phase ApplyOutcome
	// previous is the operation recorded before this attempt's, with the attempt it belonged to,
	// put back should this attempt ask for no apply.
	previous   *v1alpha3.ApplyOperation
	previousID uint64
	// recovered is an attempt that asks nothing: it follows an operation recorded on the trigger,
	// by an earlier process or by the trigger's reconciler, to its end (apply_recovery.go).
	recovered bool
	// recorded is set, on the requester's goroutine and before its reports, once the requester
	// called the attempt's Record: from then on the trigger may hold its operation, so an ending
	// that asked for no apply withdraws it.
	recorded atomic.Bool
	// abandon closes the request's Abandoned channel.
	abandon context.CancelFunc
}

// quietPeriod is how long the branch must be quiet before a CatchUp: the trigger's, as listed with
// each GitTarget naming it. The longest wins should a listing straddle an edit of the trigger.
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
	defer l.publishTriggerStatus()
	now := time.Now()
	l.settleApplyResults(now)
	l.followRecordedOperations(now)
	if !l.branchQuiet(now) || l.workWaiting() {
		// Queued work, here or on a sibling: the next wake looks again, and a sibling's loop wakes
		// this one once it has handled its last item. The branch is still quiet until that work
		// opens a window, so quietSince is kept. A push observed meanwhile is fetched ahead of the
		// apply.
		l.prefetchSources()
		return
	}
	// Quiet: the attempts sent now fetch what they apply themselves.
	w.debt.takePrefetch()

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
	siblingWriting, _ := l.w.siblingBusy()
	if busy || siblingWriting {
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
		if state.foreign || state.catchUp || state.repair || state.inflight != nil {
			return true
		}
	}
	return false
}

// syncAppliers makes the applier states match the GitTargets listed now. An applier no GitTarget
// names any more is forgotten, with whatever it was owed: nobody asks for it, and its operation, if
// one is out, is abandoned. A refusal owed for a GitTarget that is gone, or is now another
// incarnation, is dropped the same way.
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
	for ref, state := range l.appliers {
		if _, ok := named[ref]; !ok {
			if state.inflight != nil {
				state.inflight.abandon()
			}
			if state.withdrawn != nil {
				l.publishStatusOf(state)
			}
			delete(l.appliers, ref)
		}
	}
	l.publishUnresolved()
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
// retry and quiet periods have run out. The appliers are taken in turn order, so in a group, which
// has one attempt out at a time, the attempt goes to the applier whose turn is longest ago. It
// returns the earliest time one still waiting comes due.
func (l *branchWorkerEventLoop) sendDue(tip string, now time.Time) time.Time {
	var earliest time.Time
	wait := func(at time.Time) {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	for _, ref := range l.turnOrder() {
		state := l.appliers[ref]
		if l.attemptOut(state) {
			continue
		}
		refusals := l.refusalsOwed(state)
		if !state.foreign && !state.repair && len(refusals) == 0 && !state.catchUp {
			continue
		}
		due := state.retryAt
		if !state.foreign && !state.repair && len(refusals) == 0 {
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
// for a ForeignCommit, then for a Repair; otherwise a Refusal for a target with a refusal owed, and
// a CatchUp for the rest when one is owed. Any operation that succeeds applies the tip, so it
// answers an owed CatchUp or Repair too.
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
		case state.repair:
			own = v1alpha3.ApplyReasonRepair
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
	abandoned, abandon := context.WithCancel(l.w.ctx)
	sentAt := time.Now()
	attempt := &inflightAttempt{
		id: l.nextApplyID, reason: reason, commit: tip, sentAt: sentAt,
		foreign: state.foreign, catchUp: state.catchUp, repair: state.repair,
		refusals: refusals, abandon: abandon,
		previous: state.operation, previousID: state.operationID,
	}
	state.foreign, state.catchUp, state.repair, state.inflight = false, false, false, attempt
	state.lastTurn = attempt.id
	state.openApplyWindow(attempt.id, tip, sentAt)
	for key := range refusals {
		delete(l.reapplyOwed, key)
	}
	// What the trigger shows now goes out before the request, so the record replaces the latest of
	// it: the operation before this one may have completed on this very pass.
	l.publishStatusOf(state)
	token := operationToken(sentAt)
	state.operation = &v1alpha3.ApplyOperation{
		Phase: v1alpha3.ApplyOperationRequested, Reason: reason, Commit: tip, Token: token,
		RequestedAt: metav1.NewTime(sentAt),
	}
	state.operationID = attempt.id
	record := l.recorder(state, attempt)

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
		Token:         token,
		Record:        record,
		Targets:       targets,
		PriorFailures: state.failures,
		Current:       func() bool { return w.decisionHolds(tip) },
		Report:        w.reportApplyResult,
		Abandoned:     abandoned.Done(),
	})
}

// settleApplyResults reads the reports that came back. A report of an operation still out records
// where it stands, and holds the attempt. An operation that succeeded is done. Any other ending
// gives back what the attempt carried and, unless it was only stale, waits out its applier's
// backoff, so its retry is decided again on a quiet branch at whatever the tip is then, never
// replayed blind. A report for an applier no GitTarget names any more is dropped with it.
func (l *branchWorkerEventLoop) settleApplyResults(now time.Time) {
	defer l.publishUnresolved()
	for _, res := range l.w.debt.takeResults() {
		state := l.applierAwaiting(res.ID)
		if state == nil {
			continue
		}
		attempt := state.inflight
		state.recordOperation(attempt, res, now)
		if !res.Outcome.Ends() {
			l.advanceOperation(state, attempt, res.Outcome)
			continue
		}
		state.inflight = nil
		attempt.abandon()
		state.settleApplyWindow(attempt.id, res.Outcome.Operated(), now)
		if attempt.phase == ApplyUnresolved {
			l.w.Log.Info("An unresolved apply completed; its apply window closes after the grace",
				"branch", l.w.Branch, "applier", state.ref.String(), "outcome", string(res.Outcome))
		}
		switch res.Outcome {
		case ApplySucceeded, ApplyCurrent:
			state.failures, state.retryAt = 0, time.Time{}
			continue
		case ApplyStale:
			// The decision went stale before the patch: nothing failed, and the next quiet moment
			// decides again, at the tip as it is then.
			l.restoreAttempt(state, attempt)
			continue
		case ApplyFailed, ApplyRequestFailed, ApplyRejected, ApplyDropped:
		case ApplyRequested, ApplyRunning, ApplySettling, ApplyUnresolved:
		}
		state.failures++
		state.retryAt = now.Add(applierRetryDelay(state.failures, res.Outcome == ApplyRejected))
		l.restoreAttempt(state, attempt)
		message := "An apply attempt did not deliver; deciding it again on a quiet branch"
		if res.Outcome == ApplyFailed {
			message = "The applier did not complete the apply; deciding it again on a quiet branch"
		}
		l.w.Log.Info(message,
			"branch", l.w.Branch, "applier", state.ref.String(), "outcome", string(res.Outcome),
			"failures", state.failures, "retryIn", state.retryAt.Sub(now), "error", errString(res.Err))
	}
}

// advanceOperation records where an attempt's operation stands. Unresolved is said once, and stays
// until the operation ends: it is the requester's verdict that the operation is overdue, which a
// later sight of it running does not take back.
func (l *branchWorkerEventLoop) advanceOperation(state *applierState, attempt *inflightAttempt, phase ApplyOutcome) {
	if attempt.phase == ApplyUnresolved {
		return
	}
	attempt.phase = phase
	if phase != ApplyUnresolved {
		return
	}
	l.w.Log.Info("The applier has not completed the apply in time; its apply window stays open, and "+
		"nothing more is asked of it until the apply completes", "branch", l.w.Branch,
		"applier", state.ref.String(), "attempt", attempt.id)
}

// operationPhase is the phase of the operation an attempt's report describes, and false for the
// outcomes that started no operation, which leave the last one recorded as it is.
func operationPhase(outcome ApplyOutcome) (v1alpha3.ApplyOperationPhase, bool) {
	switch outcome {
	case ApplyRequested:
		return v1alpha3.ApplyOperationRequested, true
	case ApplyRunning:
		return v1alpha3.ApplyOperationRunning, true
	case ApplySettling:
		return v1alpha3.ApplyOperationSettling, true
	case ApplyUnresolved:
		return v1alpha3.ApplyOperationUnresolved, true
	case ApplySucceeded:
		return v1alpha3.ApplyOperationSucceeded, true
	case ApplyFailed:
		return v1alpha3.ApplyOperationFailed, true
	case ApplyCurrent, ApplyRequestFailed, ApplyRejected, ApplyDropped, ApplyStale:
	}
	return "", false
}

// recordOperation records an attempt's report in the operation its trigger's status shows, which
// sendAttempt started as Requested when it sent the attempt. A report that ends an attempt that
// asked for no apply puts back the operation before it, and withdraws its own from the trigger if
// the requester recorded it there. Unresolved stays until the operation completes, as
// advanceOperation keeps it.
func (s *applierState) recordOperation(attempt *inflightAttempt, res ApplyResult, now time.Time) {
	if s.operationID != attempt.id {
		return
	}
	phase, operated := operationPhase(res.Outcome)
	if !operated {
		if res.Outcome.Ends() {
			// It asked for no apply: its operation stands for nothing.
			if attempt.recorded.Load() {
				s.withdrawn = s.operation
			}
			s.operation, s.operationID = attempt.previous, attempt.previousID
		}
		return
	}
	operation := s.operation.DeepCopy()
	if res.FenceToken != "" {
		operation.FenceToken = res.FenceToken
	}
	if operation.Phase != v1alpha3.ApplyOperationUnresolved || phase.Completed() {
		operation.Phase = phase
	}
	switch {
	case phase.Completed():
		finished := metav1.NewTime(now)
		operation.FinishedAt, operation.AppliedRevision = &finished, res.AppliedRevision
		operation.Message = errString(res.Err)
		if res.Err == nil {
			operation.Message = res.Message
		}
	case phase == v1alpha3.ApplyOperationUnresolved:
		operation.Message = "the applier has not completed the apply in the time it was given; it is still " +
			"followed, and nothing more is asked of it until it completes"
	}
	s.operation, s.operationID = operation, attempt.id
}

// operationToken is the token an operation started at sentAt carries: the time, to the nanosecond,
// as `flux reconcile` stamps a requestedAt.
func operationToken(sentAt time.Time) string {
	return sentAt.UTC().Format(time.RFC3339Nano)
}

// recorder is the attempt's ApplyRequest.Record: the requester writes the operation the attempt
// starts through to the trigger, just before it starts the apply. That is the only way it reaches
// the trigger before the requester reports it, so an attempt that ends before it leaves the trigger
// as it was. Nil without a trigger status writer.
func (l *branchWorkerEventLoop) recorder(state *applierState, attempt *inflightAttempt) func(context.Context) error {
	writer := l.w.triggerStatus
	if writer == nil {
		return nil
	}
	trigger, operation := state.ref.Trigger, state.operation.DeepCopy()
	return func(ctx context.Context) error {
		attempt.recorded.Store(true)
		return writer.RecordApplyOperation(ctx, trigger, operation)
	}
}

// shownOperation is the operation the applier's trigger is to show: the last one an attempt
// started, once the requester reported it, and the one before it until then. An attempt that
// follows a recorded operation shows it from the start: the trigger holds it already.
func (s *applierState) shownOperation() *v1alpha3.ApplyOperation {
	if attempt := s.inflight; attempt != nil && !attempt.recovered && attempt.phase == "" &&
		s.operationID == attempt.id {
		return attempt.previous
	}
	return s.operation
}

// publishTriggerStatus hands each applier's trigger what its status should show, when that changed
// since the last time.
func (l *branchWorkerEventLoop) publishTriggerStatus() {
	for _, ref := range l.sortedAppliers() {
		l.publishStatusOf(l.appliers[ref])
	}
}

// publishStatusOf hands one applier's trigger what its status should show, and the operation it
// withdraws, when that changed since the last time. A worker that has started no operation and
// owes no Repair has nothing to say yet, and says nothing: the status may hold what an earlier
// process recorded.
func (l *branchWorkerEventLoop) publishStatusOf(state *applierState) {
	writer := l.w.triggerStatus
	if writer == nil {
		return
	}
	next := ApplyTriggerStatus{
		Operation: state.shownOperation(), Withdrawn: state.withdrawn, RepairOwed: state.repairOwed(),
	}
	if next.Withdrawn == nil {
		if state.published == nil && next.Operation == nil && !next.RepairOwed {
			return
		}
		if state.published != nil && state.published.RepairOwed == next.RepairOwed &&
			equality.Semantic.DeepEqual(state.published.Operation, next.Operation) {
			return
		}
	}
	writer.PublishApplyTriggerStatus(state.ref.Trigger, next)
	state.published, state.withdrawn = &next, nil
}

// publishUnresolved publishes how many of this branch's operations are Unresolved, for the
// apply_operations_unresolved gauge.
func (l *branchWorkerEventLoop) publishUnresolved() {
	var unresolved int64
	for _, state := range l.appliers {
		if state.inflight != nil && state.inflight.phase == ApplyUnresolved {
			unresolved++
		}
	}
	l.w.unresolvedOperations.Store(unresolved)
}

// restoreAttempt gives an applier back what an attempt that did not succeed carried. A refusal comes
// back only while it still stands: one that recovered while the attempt was out has nothing left to
// revert.
func (l *branchWorkerEventLoop) restoreAttempt(state *applierState, attempt *inflightAttempt) {
	state.foreign = state.foreign || attempt.foreign
	state.catchUp = state.catchUp || attempt.catchUp
	state.repair = state.repair || attempt.repair
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

// workWaiting reports whether work items are queued on this worker, or queued or in hand on another
// writing this repository and branch.
func (l *branchWorkerEventLoop) workWaiting() bool {
	_, siblingWorking := l.w.siblingBusy()
	return len(l.w.eventQueue) > 0 || siblingWorking
}

// decisionHolds is ApplyRequest.Current for an attempt decided at tip: the branch is still at that
// commit, and nothing live is waiting to be published on this worker or a sibling. A work item
// counts from its enqueue until its wake has published what it left behind (inflightItems), not
// only while it waits in the queue: a queued watch event is an edit that is live and not yet
// decided, and the item the loop is handling may be committing and pushing while busyWriting still
// says what the last wake left. It is called from the requester's goroutine, so it reads only what
// is safe to read there.
//
// An attempt sent from the wake of a queue item can find that item still counted, should the
// requester ask within moments. It is then Stale, which costs no backoff, and the report wakes the
// loop without an item, so the next attempt is not held by one of its own.
func (w *BranchWorker) decisionHolds(tip string) bool {
	siblingWriting, siblingWorking := w.siblingBusy()
	return w.debt.tipIs(tip) && !w.busyWriting.Load() && w.inflightItems.Load() == 0 &&
		!siblingWriting && !siblingWorking
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
