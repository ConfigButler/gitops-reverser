// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// This file is the branch worker's half of the applier claim: deciding WHEN the appliers of the
// GitTargets on this branch apply. See docs/design/apply-trigger.md and
// docs/design/applier-echoes.md.
//
// An apply must not revert an edit that is live and not yet in Git, and only the worker can see
// those: its open commit window and pending writes. So an attempt goes out after a FLUSH: the open
// window is closed, the writes decided up to that moment (the flush mark) are published, and the
// attempt is sent for the tip that publication produced. An edit decided after the mark is recorded
// in the apply window, so an apply that overwrites it is withheld and a Repair puts it back
// (apply_window.go). Four reasons ask for an attempt:
//
//   - ForeignCommit: an observation of the remote found the branch somewhere this worker did not
//     push it. Every applier on the branch is asked.
//   - Refusal: a refused edit on a target with spec.onRefusal: Reapply (refusal_reapply.go).
//   - CatchUp: ownership, not safety, the same for Flux and Argo CD. An applier prunes only what it
//     applied: an object the API created in its folder carries no owner labels and no tracking
//     annotation, so when Git later deletes its file the applier leaves it on the cluster. A CatchUp
//     ADOPTS it: it is pending once the worker observes an API create in a folder an applier applies,
//     or a resync adds an object to Git, and is never skipped as already applied, because applying
//     the same commit again is what stamps the object. A CatchUp also CONVERGES: once when the worker
//     starts, and once when a GitTarget first names the applier claim; that one asks nothing of an applier
//     that already applied the tip (S2 in docs/design/applier-echoes.md).
//   - Repair: the apply window withheld a late apply over a newer API edit (apply_window.go), so
//     the cluster runs an older value than Git. Asked like a Refusal, and of
//     either kind of applier, because nothing else would put the edit back on an Argo CD
//     Application.
//
// Two relations are kept apart on purpose. The APPLIER is the unit of pending reasons: one attempt in
// flight at a time, its own retry clock, and one attempt for all the GitTargets that name it. The
// GITTARGET, by uid, is the unit of configuration and consent.
//
// No applier is asked for anything, for any reason, before every initial snapshot of the GitTargets
// it covers is published: at startup the edits made while the operator was down are then in Git, and
// a newly named GitTarget's folder holds the cluster. An empty tip asks nothing.
//
// The worker keeps every pending reason until an operation answers it. The requester carries out one
// attempt and reports its operation as it moves; it never retries, so a retry is decided here like
// any first attempt, after a flush, against what the targets are now. An operation is followed for as
// long as it takes: one past the requester's budget is Unresolved, and keeps its window open and its
// applier's next attempt back, because a timer that closes the window does not stop the apply.

// applyInbox holds what other goroutines tell the loop about applies: the reasons pending for the
// branch's appliers because of what was OBSERVED about the remote, and the reports of attempts that
// came back. It is written from other goroutines (the first clone
// publishes an observation before the loop starts; the requester reports from its own), hence the
// lock.
type applyInbox struct {
	mu sync.Mutex
	// known is whether anything has been observed yet, and tip is the last commit observed. Empty
	// means the remote does not carry the branch.
	known bool
	tip   string
	// foreign and converge are pending until the loop takes them onto the appliers.
	foreign  bool
	converge bool
	// results are the reports of finished attempts, until the loop reads them.
	results []ApplyResult
}

// observe classifies one observation. The first one is a baseline, not a move: nothing says what
// the appliers last applied, so the startup converge is pending for them, never a ForeignCommit. A
// request on a restart would otherwise apply the tip before the watches have captured the edits made
// while the operator was down.
//
// After that, a commit that differs from the last one observed is either this worker's push, proved
// by the push itself, which leaves nothing pending, or somebody else's, because every other
// observation is a fetch. When in doubt the classification errs towards foreign, which costs one
// apply of the tip.
func (d *applyInbox) observe(observed RemoteObservation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.known {
		d.known, d.tip, d.converge = true, observed.Commit, true
		return
	}
	if observed.Commit == d.tip {
		return
	}
	d.tip = observed.Commit
	if observed.By != ObservedByPush {
		d.foreign = true
	}
}

// take returns the pending reasons and clears them. The caller restores it if it cannot act on it.
func (d *applyInbox) take() (string, bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tip, foreign, converge := d.tip, d.foreign, d.converge
	d.foreign, d.converge = false, false
	return tip, foreign, converge
}

// restore puts back what take returned, for a caller that could not act on it.
func (d *applyInbox) restore(foreign, converge bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.foreign = d.foreign || foreign
	d.converge = d.converge || converge
}

// foreignPending reports whether a ForeignCommit is pending, without taking it.
func (d *applyInbox) foreignPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foreign
}

// lastTip is the last commit observed, empty before the first observation or when the remote does
// not carry the branch.
func (d *applyInbox) lastTip() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tip
}

// tipIs reports whether the last commit observed is commit.
func (d *applyInbox) tipIs(commit string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.known && d.tip == commit
}

func (d *applyInbox) addResult(res ApplyResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results = append(d.results, res)
}

func (d *applyInbox) takeResults() []ApplyResult {
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
	ref     itypes.ResourceReference
	uid     k8stypes.UID
	applier ApplierRef
	// consents is whether it asks for a Refusal request now (ConsentsToReapply).
	consents bool
	// recorded and repairPending are the applier claim's status.operation and status.repairPending, as listed:
	// what an earlier process, or the applier claim's reconciler, recorded (apply_recovery.go).
	recorded      *v1alpha3.ApplyOperation
	repairPending bool
	// group names the GitRepository a Flux applier claim's Kustomization applies, in its cluster; empty
	// for an Argo CD Application or a source not read yet (apply_groups.go).
	group string
	// folder is the GitTarget's spec.path, sanitized: the folder its applier is asked to apply.
	folder string
}

// appliersOnBranch lists the GitTargets that write this worker's branch through its GitProvider and
// name an applier claim that exists, with the applier each applier claim holds. A GitTarget may name a
// GitProvider in another namespace, so every namespace is looked in. The client is cached, so this
// costs no round trip. A GitTarget whose applier claim does not exist (yet) names no applier: its
// ApplierClaimReady condition says so, and so does one its providers do not admit.
func (w *BranchWorker) appliersOnBranch(ctx context.Context) ([]branchTarget, error) {
	var targets v1alpha3.GitTargetList
	if err := w.Client.List(ctx, &targets); err != nil {
		return nil, fmt.Errorf("list GitTargets: %w", err)
	}
	listed := make([]branchTarget, 0, len(targets.Items))
	for i := range targets.Items {
		target := &targets.Items[i]
		if !target.DeletionTimestamp.IsZero() || target.Spec.ApplierClaimRef == nil ||
			!WritesThrough(target, w.GitBranch) {
			continue
		}
		applierClaim, err := ResolveApplierClaim(ctx, w.Client, target)
		if err != nil {
			return nil, err
		}
		if applierClaim == nil || !applierClaim.GetDeletionTimestamp().IsZero() {
			continue
		}
		// The dispatcher refuses an attempt for a GitTarget its providers do not admit. Listed, it
		// would make the worker flush, raise its apply epoch and hold the applier, or its whole
		// source group, for an attempt that cannot go.
		if w.applierAdmitted != nil {
			decision, err := w.applierAdmitted(ctx, w.Client, target, applierClaim)
			if err != nil {
				return nil, fmt.Errorf("check whether %s may ask its applier: %w", target.Name, err)
			}
			if !decision.Allowed {
				continue
			}
		}
		listed = append(listed, branchTarget{
			ref:           itypes.NewResourceReference(target.Name, target.Namespace),
			uid:           target.UID,
			applier:       ApplierOf(applierClaim),
			consents:      ConsentsToReapply(target),
			recorded:      applierClaim.Operation(),
			repairPending: applierClaim.RepairPending(),
			group:         sourceGroup(applierClaim),
			folder:        sanitizePath(target.Spec.Path),
		})
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].ref.String() < listed[j].ref.String() })
	return listed, nil
}

// applierState is the reasons pending for one applier on this branch, and how its attempts are going.
type applierState struct {
	ref ApplierRef
	// targets are the GitTargets that name it, as listed on the last pass.
	targets []branchTarget
	// foreign, adopt, converge and repair are pending until an attempt carries them. adopt and
	// converge are both asked as a CatchUp.
	foreign  bool
	adopt    bool
	converge bool
	repair   bool
	// named is the GitTarget incarnations that named the applier so far: one naming it for the first
	// time leaves a converge pending.
	named map[k8stypes.UID]bool
	// convergeInterval is how long the applier may stay behind the tip before a converge is pending
	// for it, zero for never, as last read from its applier claim, and behindSince is when this worker first found it behind, zero while
	// it is not, or since the last attempt was sent (scheduleConverges).
	convergeInterval time.Duration
	behindSince      time.Time
	// lag is whether the converge pending is only scheduleConverges': nothing else asks for the tip,
	// so its attempt yields to an edit (convergeHolds).
	lag bool
	// appliedRevision is the revision the last operation that succeeded applied, as the applier
	// reported it, recovered on restart from the applier claim's status.operation. A converge of the commit
	// it names asks nothing.
	appliedRevision string
	// inflight is the attempt whose report has not ended it yet. While it is out, nothing else is
	// sent to this applier: a reason that becomes pending meanwhile waits for it.
	inflight *inflightAttempt
	// failures counts attempts that did not succeed since the last that did, and retryAt is when
	// the next attempt may go.
	failures int
	retryAt  time.Time
	// lastTurn is the id of the last attempt sent to it, zero before the first: a group's next
	// attempt goes to the applier whose turn is longest ago (turnOrder).
	lastTurn uint64
	// window is the applier's apply window: open from sending an attempt until the applier
	// finished it (apply_window.go). Nil when no attempt was sent, or the last asked for no apply
	// after the window before it had closed.
	window *applyWindow
	// operation is the last operation an attempt started, as its applier claim's status.operation shows
	// it, and operationID the attempt it belongs to. Nil until this worker started one.
	operation   *v1alpha3.ApplyOperation
	operationID uint64
	// published is what was last handed to the applier claim status writer, nil before the first.
	published *ApplierClaimStatus
	// withdrawn is an operation an attempt recorded on the applier claim and then did not start, until
	// it is published as withdrawn.
	withdrawn *v1alpha3.ApplyOperation
	// held is why the barrier holds back a reason pending for it, as last logged; empty while
	// nothing is held (barrier.go).
	held string
	// recovered is whether the applier claim's recorded status was read back for this applier, once,
	// when this process first listed it; followedSuspend is the suspend token of the last operation
	// the applier claim's reconciler recorded that this worker followed (apply_recovery.go).
	recovered       bool
	followedSuspend string
}

// onlyLag reports whether scheduleConverges' converge is all that is pending for the applier, given its
// unsent refusals: an attempt carrying nothing else yields to an edit (convergeHolds).
func (s *applierState) onlyLag(refusals map[refusalKey]unsentRefusal) bool {
	return s.lag && !s.foreign && !s.adopt && !s.repair && len(refusals) == 0
}

// repairPending reports whether a Repair is pending: not yet carried, or carried by the attempt out.
func (s *applierState) repairPending() bool {
	return s.repair || (s.inflight != nil && s.inflight.repair)
}

// inflightAttempt is what an attempt carries away from its applier's pending reasons. One that did
// not succeed gives them back (restoreAttempt); one that succeeded keeps it.
type inflightAttempt struct {
	id       uint64
	foreign  bool
	adopt    bool
	converge bool
	repair   bool
	refusals map[refusalKey]unsentRefusal
	// lag is whether it carries only scheduleConverges' converge.
	lag bool
	// phase is the last report of its operation, empty before the first.
	phase ApplyOutcome
	// previous is the operation recorded before this attempt's, with the attempt it belonged to,
	// put back should this attempt ask for no apply.
	previous   *v1alpha3.ApplyOperation
	previousID uint64
	// recovered is an attempt that asks nothing: it follows an operation recorded on the applier claim,
	// by an earlier process or by the applier claim's reconciler, to its end (apply_recovery.go).
	recovered bool
	// recorded is set, on the requester's goroutine and before its reports, once the requester
	// called the attempt's Record: from then on the applier claim may hold its operation, so an ending
	// that asked for no apply withdraws it.
	recorded atomic.Bool
	// abandon closes the request's Abandoned channel.
	abandon context.CancelFunc
}

// hasReason reports whether a reason is pending for the applier, given its unsent refusals.
func (s *applierState) hasReason(refusals map[refusalKey]unsentRefusal) bool {
	return s.foreign || s.repair || s.adopt || s.converge || len(refusals) > 0
}

// barrierRecheckInterval is how soon a reason held back by the initial-snapshot barrier is looked at
// again: the barrier lifts when the watch side and a publication say so, neither of which wakes the
// loop on its own.
const barrierRecheckInterval = time.Second

// writeBusy reports whether the loop is in a state in which no flush can be made, and so no request
// sent: a registered CommitRequest, whose save defines its own commit boundary (a save is announced
// before its writes arrive, and an apply landing between the two would revert its first edits before
// they reach Git), a parent recovery, or a checkout a failed write left dirty. In the last two the
// worker cannot say what is published.
func (l *branchWorkerEventLoop) writeBusy() bool {
	return len(l.pendingCRs) > 0 || l.recovery.active || l.w.worktreeDirty()
}

// closeWindowForPushHint closes the open commit window early when a Git host has said the branch may
// have moved, so its writes are published, and replayed onto the move, now rather than when the
// window's timers allow. A window a CommitRequest holds is left to its save.
func (l *branchWorkerEventLoop) closeWindowForPushHint() {
	hinted := l.pushHinted
	l.pushHinted = false
	if !hinted || l.openWindow == nil || l.openWindow.pendingCR != nil || l.w.applyRequester == nil {
		return
	}
	l.w.Log.Info("A Git host reported a push; closing the commit window to publish it now",
		"branch", l.w.Branch)
	l.finalizeOpenWindowWithReason(windowFinalizeReasonForeignCommit)
}

// serviceAppliers settles the reports that came back, and sends whatever attempts are due, after a
// flush (docs/design/applier-echoes.md, S1). It runs at the end of every wake, after the wake's
// writes have been decided and committed.
//
// When an apply comes due, the open commit window is closed, so its edits become a decided write,
// and the flush mark is taken: the last write decided so far. From the mark, the edits the due
// appliers' next apply windows will hold are recorded (withholdLateApply). The writes up to the mark
// are published, at once, and the attempts are sent for the tip that publication produced. Writes
// decided after the mark do not hold the send, so continuous editing cannot starve an apply: an edit
// decided after it is recorded, and an apply that overwrites it is withheld and repaired.
func (l *branchWorkerEventLoop) serviceAppliers() {
	w := l.w
	if w.applyRequester == nil {
		return
	}
	defer l.publishClaimStatus()
	defer l.publishHeld()
	now := time.Now()
	l.settleApplyResults(now)
	l.followRecordedOperations(now)
	converging := l.scheduleConverges(now)
	work := l.takeReasons()
	// A listing that failed is retried on the applier timer (listRetryAt), which every arming below
	// keeps.
	closing := earliest(earliest(l.reportApplyWindows(now), l.snapshotHoldDeadline()), l.listRetryAt)
	closing = earliest(closing, converging)

	if !work {
		l.endFlush()
		l.armApplierTimer(now, closing)
		return
	}
	due, wait := l.dueAppliers(now)
	wait = earliest(wait, closing)
	if len(due) == 0 {
		l.endFlush()
		l.armApplierTimer(now, wait)
		return
	}
	if !l.flushed() {
		// The publication wakes the loop when it lands (a push, or its retry); a save, a parent
		// recovery or a dirty checkout end with a wake of their own.
		l.armApplierTimer(now, wait)
		return
	}
	// Publishing may have observed news: a rejected push fetches, and a foreign commit it found is
	// pending for every applier now. The attempts carry it, for the tip that holds both sides.
	l.takeReasons()
	tip := w.inbox.lastTip()
	for _, state := range due {
		if l.attemptOut(state) {
			continue
		}
		l.sendAttempt(state, tip, l.unsentRefusalsFor(state))
	}
	l.endFlush()
	l.armApplierTimer(now, wait)
}

// scheduleConverges leaves a converge pending for each applier that has stayed behind the tip for its
// convergeInterval, and returns when the next one still waiting comes due, zero when none waits.
//
// A push of the worker's own leaves nothing pending (applyInbox.observe): the flush put its edits on
// the cluster already. But the applier's own state, its applied revision and its source, stays at the
// commit before, and other things read it. The interval bounds that lag. The clock starts on the first
// pass that finds the applier behind, and starts again when an attempt is sent, so a burst of pushes
// inside one interval costs one apply, and an applier at the tip costs nothing. The converge then
// waits like any other: for the barrier, the backoff and the flush (dueAppliers), and asks nothing
// of an applier that applied the tip meanwhile.
func (l *branchWorkerEventLoop) scheduleConverges(now time.Time) time.Time {
	tip := l.w.inbox.lastTip()
	var next time.Time
	for _, state := range l.appliers {
		if tip == "" || revisionNamesCommit(state.appliedRevision, tip) {
			state.behindSince = time.Time{}
			continue
		}
		state.convergeInterval = l.w.convergeIntervalOf(state)
		if state.convergeInterval <= 0 {
			state.behindSince = time.Time{}
			continue
		}
		if state.behindSince.IsZero() {
			state.behindSince = now
		}
		if state.converge || l.attemptOut(state) {
			continue
		}
		due := state.behindSince.Add(state.convergeInterval)
		if now.Before(due) {
			next = earliest(next, due)
			continue
		}
		state.converge, state.lag = true, true
		l.w.Log.V(1).Info("The applier stayed behind the tip for its converge interval; a converge is pending",
			"branch", l.w.Branch, "applier", state.ref.String(), "applied", state.appliedRevision, "tip", tip,
			"interval", state.convergeInterval.String())
	}
	return next
}

// convergeIntervalOf reads the applier's spec.convergeInterval from its applier claim, on every pass
// that finds it behind, so a change applies from the next wake rather than from the next listing,
// which a push of the worker's own does not make. The client is cached, so this costs no round trip.
// An applier claim that cannot be read keeps the interval last read.
func (w *BranchWorker) convergeIntervalOf(state *applierState) time.Duration {
	ref := state.ref.ApplierClaim
	applierClaim := v1alpha3.NewApplierClaim(ref.Kind)
	if applierClaim == nil {
		return 0
	}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := w.Client.Get(w.ctx, key, applierClaim); err != nil {
		return state.convergeInterval
	}
	return applierClaim.ConvergeInterval()
}

// takeReasons moves the reasons the inbox holds onto the appliers listed now, and reports whether
// any applier has work. A listing that fails leaves them in the inbox, and sets listRetryAt, when
// the applier timer looks again; only a listing that succeeds clears it (followRecordedOperations
// lists the same appliers first, every pass).
func (l *branchWorkerEventLoop) takeReasons() bool {
	w := l.w
	_, foreign, converge := w.inbox.take()
	if !foreign && !converge && len(l.unsentRefusals) == 0 && !l.appliersHaveWork() {
		return false
	}
	targets, err := w.appliersOnBranch(w.ctx)
	if err != nil {
		w.inbox.restore(foreign, converge)
		w.Log.Error(err, "Cannot list the appliers on this branch; retrying", "branch", w.Branch)
		l.listRetryAt = time.Now().Add(applierRetryInterval)
		return false
	}
	l.listRetryAt = time.Time{}
	l.syncAppliers(targets)
	for _, state := range l.appliers {
		state.foreign = state.foreign || foreign
		state.converge = state.converge || converge
		state.lag = state.lag && !converge
	}
	return true
}

// dueAppliers is the appliers an attempt is due for now, in turn order, and the earliest time one
// still waiting comes due. The appliers are taken in turn order, so in a group, which has one attempt
// out at a time, the attempt goes to the applier whose turn is longest ago.
//
// A reason waits for the applier's backoff, and for the initial-snapshot barrier (pastBarrier). An
// empty tip asks nothing: the remote does not carry the branch, and the source reports that itself;
// the reason stays pending for when it does. A converge alone of the commit the applier last applied
// asks nothing, and is answered.
func (l *branchWorkerEventLoop) dueAppliers(now time.Time) ([]*applierState, time.Time) {
	var due []*applierState
	var wait time.Time
	tip := l.w.inbox.lastTip()
	for _, ref := range l.turnOrder() {
		state := l.appliers[ref]
		if l.attemptOut(state) {
			continue
		}
		refusals := l.unsentRefusalsFor(state)
		if l.alreadyConverged(state, refusals, tip) {
			state.converge, state.lag = false, false
			l.w.Log.V(1).Info("The applier already applied the tip; the converge asks nothing",
				"branch", l.w.Branch, "applier", state.ref.String(), "commit", tip)
		}
		held := ""
		switch {
		case !state.hasReason(refusals) || tip == "":
		case l.barrierHold(state) != "":
			held = l.barrierHold(state)
		case now.Before(state.retryAt):
			wait = earliest(wait, state.retryAt)
		default:
			// Looked at last: it reads the branch, and only an attempt that would go now needs it.
			held = l.missingFolders(state)
			if held == "" {
				due = append(due, state)
			}
		}
		if held != "" {
			wait = earliest(wait, now.Add(barrierRecheckInterval))
		}
		l.noteHeld(state, held)
	}
	return due, wait
}

// alreadyConverged reports whether only a converge is pending for the applier, and it already applied
// the tip. An adopt is never skipped this way: the applier may be at the tip and still not own an
// object the API created, and applying the same commit again is what stamps it.
func (l *branchWorkerEventLoop) alreadyConverged(
	state *applierState, refusals map[refusalKey]unsentRefusal, tip string,
) bool {
	return state.converge && !state.adopt && !state.foreign && !state.repair && len(refusals) == 0 &&
		revisionNamesCommit(state.appliedRevision, tip)
}

// pastBarrier reports whether the barrier lets the applier be asked for something (barrierHold),
// leaving out its folders, which only an attempt about to go reads (missingFolders). A folder already
// found missing still holds it.
func (l *branchWorkerEventLoop) pastBarrier(state *applierState) bool {
	return l.barrierHold(state) == "" && !l.awaitsFolder(state)
}

// revisionNamesCommit reports whether an applier's revision names commit: Flux writes main@sha1:<hash>,
// sha1:<hash>, main/<hash> or a bare hash, Argo CD a bare hash.
func revisionNamesCommit(revision, commit string) bool {
	if revision == "" || commit == "" {
		return false
	}
	return strings.EqualFold(revisionCommit(revision), commit)
}

// revisionCommit is the commit an applier's revision names: the hash after its last ':' or '/'.
func revisionCommit(revision string) string {
	if i := strings.LastIndexAny(revision, ":/"); i >= 0 {
		return revision[i+1:]
	}
	return revision
}

// flushed makes the flush an apply needs (see serviceAppliers), and reports whether the writes up to
// its mark are published, so the attempts may go. The first call of a flush closes the open window
// and takes the mark; later calls only look whether the publication landed.
//
// From the mark on, every edit is recorded for its GitTarget (flushEdits), not for the appliers due
// when the flush began: an applier that comes due while the flush waits for its publication is sent
// in the same flush, and an edit decided before it came due is not in the commit it applies either.
func (l *branchWorkerEventLoop) flushed() bool {
	if l.writeBusy() {
		return false
	}
	if !l.flushing {
		l.finalizeOpenWindowWithReason(windowFinalizeReasonApplyDue)
		// A new mark publishes every edit decided before it, so those are in the commit the attempts
		// apply: carried into their windows, they would withhold the apply's own write.
		l.flushing, l.flushMark, l.flushEdits = true, l.decisions, map[itypes.ResourceReference]map[string]time.Time{}
		if l.pendingUpTo(l.flushMark) {
			l.w.Log.Info("An apply is due; publishing the pending writes first", "branch", l.w.Branch,
				"pendingWrites", len(l.pendingWrites))
			l.advance()
		}
	}
	return !l.pendingUpTo(l.flushMark)
}

// pendingUpTo reports whether a write decided up to mark is still pending.
func (l *branchWorkerEventLoop) pendingUpTo(mark uint64) bool {
	for i := range l.pendingWrites {
		if l.pendingWrites[i].decision <= mark {
			return true
		}
	}
	return false
}

// endFlush ends the flush once its attempts went, or nothing is due any more: the edits recorded
// since its mark are dropped with it, and the next flush records from its own mark.
func (l *branchWorkerEventLoop) endFlush() {
	l.flushing, l.flushEdits = false, nil
}

// recordUnpublishedEdits records in the window of an attempt being sent the edits its commit does
// not hold, which the apply may overwrite: those decided after the flush mark, and the live events
// held for a GitTarget's render fidelity.
func (l *branchWorkerEventLoop) recordUnpublishedEdits(state *applierState) {
	for _, target := range state.targets {
		for key, at := range l.flushEdits[target.ref] {
			state.window.recordEdit(key, at)
		}
		if held := l.held[target.ref]; held != nil {
			for _, h := range held.events {
				state.window.recordEdit(objectKey(h.event.Identifier), h.event.LastFieldManagedAt)
			}
		}
	}
}

// recordFlushEdit records a live edit decided after the flush mark for its GitTarget, while a flush
// is waiting for its publication: it is not in the commit the flush's attempts apply.
func (l *branchWorkerEventLoop) recordFlushEdit(event Event) {
	if !l.flushing {
		return
	}
	target := itypes.NewResourceReference(event.GitTargetName, event.GitTargetNamespace)
	edits := l.flushEdits[target]
	if edits == nil {
		edits = map[string]time.Time{}
		l.flushEdits[target] = edits
	}
	key := objectKey(event.Identifier)
	edits[key] = latest(edits[key], event.LastFieldManagedAt)
}

// earliest is the earlier of two deadlines, a zero one being none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// appliersHaveWork reports whether any applier has a reason pending or an attempt out.
func (l *branchWorkerEventLoop) appliersHaveWork() bool {
	for _, state := range l.appliers {
		if state.foreign || state.adopt || state.converge || state.repair || state.inflight != nil {
			return true
		}
	}
	return false
}

// syncAppliers makes the applier states match the GitTargets listed now. An applier no GitTarget
// names any more is forgotten, with whatever was pending for it: nobody asks for it, and its
// operation, if one is out, is abandoned. A GitTarget that names an applier for the first time leaves
// a converge pending, for Flux and Argo CD alike: a commit observed before was pending for nobody,
// and was taken, so without one it would not be applied until some later push. It waits for that
// GitTarget's initial snapshots (pastBarrier), and asks nothing of an applier that already applied
// the tip. An applier listed for the first time starts from what its applier claim recorded as last
// applied. An unsent refusal of a GitTarget that is gone, or is now another incarnation, is dropped
// the same way.
func (l *branchWorkerEventLoop) syncAppliers(targets []branchTarget) {
	if l.appliers == nil {
		l.appliers = map[ApplierRef]*applierState{}
	}
	named := map[ApplierRef][]branchTarget{}
	uids := map[itypes.ResourceReference]k8stypes.UID{}
	listed := map[itypes.ResourceReference]bool{}
	for _, target := range targets {
		named[target.applier] = append(named[target.applier], target)
		uids[target.ref] = target.uid
		listed[target.ref] = true
	}
	l.forgetFolders(listed)
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
		l.listApplier(ref, named)
	}
	for key, refusal := range l.unsentRefusals {
		if uids[key.target] != refusal.uid {
			delete(l.unsentRefusals, key)
		}
	}
}

// listApplier records the GitTargets that name an applier now, and leaves a converge pending for each
// one that names it for the first time.
func (l *branchWorkerEventLoop) listApplier(ref ApplierRef, named []branchTarget) {
	state := l.appliers[ref]
	if state == nil {
		state = &applierState{ref: ref, named: map[k8stypes.UID]bool{}, appliedRevision: lastApplied(named)}
		l.appliers[ref] = state
	}
	state.targets = named
	for _, target := range named {
		if !state.named[target.uid] {
			state.named[target.uid], state.converge, state.lag = true, true, false
		}
	}
}

// lastApplied is the revision the last operation that succeeded applied, as an applier's applier claim
// recorded it in status.operation: its appliedRevision, or the commit it was asked for when the
// applier reported none. Empty when the last recorded operation did not succeed.
func lastApplied(targets []branchTarget) string {
	recorded, _ := recordedStatus(&applierState{targets: targets})
	if recorded == nil || recorded.Phase != v1alpha3.ApplyOperationSucceeded {
		return ""
	}
	if recorded.AppliedRevision != "" {
		return recorded.AppliedRevision
	}
	return recorded.Commit
}

// unsentRefusalsFor is the unsent refusals of the GitTargets naming this applier that still consent.
// A refusal of a target that no longer consents is dropped: its consent was the only reason to send it.
func (l *branchWorkerEventLoop) unsentRefusalsFor(state *applierState) map[refusalKey]unsentRefusal {
	consents := map[itypes.ResourceReference]bool{}
	for _, target := range state.targets {
		consents[target.ref] = target.consents
	}
	unsent := map[refusalKey]unsentRefusal{}
	for key, refusal := range l.unsentRefusals {
		consenting, named := consents[key.target]
		if !named {
			continue
		}
		if !consenting {
			delete(l.unsentRefusals, key)
			continue
		}
		unsent[key] = refusal
	}
	return unsent
}

// sendAttempt hands one attempt for an applier to the requester and moves what it carries out of
// the applier's pending reasons, into the attempt. Each target is asked for its own reason: every
// one for a ForeignCommit, then for a Repair; otherwise a Refusal for a target with an unsent
// refusal, and a CatchUp for the rest when an adopt or a converge is pending. Any operation that
// succeeds applies the tip, so it answers a pending CatchUp or Repair too.
func (l *branchWorkerEventLoop) sendAttempt(state *applierState, tip string, refusals map[refusalKey]unsentRefusal) {
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
		case state.adopt || state.converge:
			own = v1alpha3.ApplyReasonCatchUp
		default:
			continue
		}
		targets = append(targets, ApplyTarget{Ref: target.ref, UID: target.uid, Reason: own})
		reason = StrongerApplyReason(reason, own)
	}

	l.nextApplyID++
	l.w.raiseApplyEpoch()
	abandoned, abandon := context.WithCancel(l.w.ctx)
	sentAt := time.Now()
	attempt := &inflightAttempt{
		id: l.nextApplyID, foreign: state.foreign, adopt: state.adopt, converge: state.converge,
		repair: state.repair, refusals: refusals, abandon: abandon,
		previous: state.operation, previousID: state.operationID,
		lag: state.onlyLag(refusals),
	}
	state.foreign, state.adopt, state.converge, state.repair, state.lag = false, false, false, false, false
	state.behindSince = time.Time{}
	state.inflight = attempt
	state.lastTurn = attempt.id
	l.reportApplyWindow(state)
	state.openApplyWindow(attempt.id, tip, sentAt)
	l.recordUnpublishedEdits(state)
	for key := range refusals {
		delete(l.unsentRefusals, key)
	}
	// What the applier claim shows now goes out before the request, so the record replaces the latest of
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
		Branch:        w.branchKey(),
		Provider:      w.provider(),
		GitBranch:     w.GitBranch,
		Applier:       state.ref,
		Reason:        reason,
		Commit:        tip,
		Token:         token,
		Record:        record,
		Targets:       targets,
		PriorFailures: state.failures,
		Current:       w.currentFor(tip, attempt.lag),
		Report:        w.reportApplyResult,
		Abandoned:     abandoned.Done(),
	})
}

// settleApplyResults reads the reports that came back. A report of an operation still out records
// where it stands, and holds the attempt. An operation that succeeded is done. Any other ending
// gives back what the attempt carried and, unless it was only stale, waits out its applier's
// backoff, so its retry is decided again after a flush at whatever the tip is then, never
// replayed blind. A report for an applier no GitTarget names any more is dropped with it.
func (l *branchWorkerEventLoop) settleApplyResults(now time.Time) {
	defer l.publishUnresolved()
	for _, res := range l.w.inbox.takeResults() {
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
		l.w.raiseApplyEpoch()
		state.settleApplyWindow(attempt.id, res.Outcome.Operated(), now)
		if attempt.phase == ApplyUnresolved {
			l.w.Log.Info("An unresolved apply completed; its apply window closes after the grace",
				"branch", l.w.Branch, "applier", state.ref.String(), "outcome", string(res.Outcome))
		}
		switch res.Outcome {
		case ApplySucceeded:
			state.failures, state.retryAt = 0, time.Time{}
			state.appliedRevision = res.AppliedRevision
			if state.appliedRevision == "" && state.operation != nil {
				state.appliedRevision = state.operation.Commit
			}
			continue
		case ApplyStale:
			if attempt.lag {
				// It yielded to an edit, or to a newer tip: it only catches the applier's state
				// up, so it waits a fresh interval rather than going again after the next flush.
				state.behindSince = now
				l.w.Log.V(1).Info("A converge yielded to an edit before its resume; waiting a fresh interval",
					"branch", l.w.Branch, "applier", state.ref.String())
				continue
			}
			// The decision went stale before the write: nothing failed, and the next wake
			// decides again, at the tip as it is then.
			l.restoreAttempt(state, attempt)
			continue
		case ApplyFailed, ApplyRequestFailed, ApplyRejected, ApplyDropped:
		case ApplyRequested, ApplyRunning, ApplySettling, ApplyUnresolved:
		}
		state.failures++
		state.retryAt = now.Add(applierRetryDelay(state.failures, res.Outcome == ApplyRejected))
		l.restoreAttempt(state, attempt)
		l.lookIfSourceMoved(state, res)
		message := "An apply attempt did not deliver; deciding it again after its backoff"
		if res.Outcome == ApplyFailed {
			message = "The applier did not complete the apply; deciding it again after its backoff"
		}
		l.w.Log.Info(message,
			"branch", l.w.Branch, "applier", state.ref.String(), "outcome", string(res.Outcome),
			"failures", state.failures, "retryIn", state.retryAt.Sub(now), "error", errString(res.Err))
	}
}

// lookIfSourceMoved takes an applier's source holding another commit than the tip the worker knows
// as a hint that the branch moved, and looks at the remote, as a Git host's push notice is looked at.
// A worker with no receiver otherwise learns of a Git-side push only through its own rejected push,
// or the refresher, and asks again for a commit the source no longer serves until then.
func (l *branchWorkerEventLoop) lookIfSourceMoved(state *applierState, res ApplyResult) {
	if res.SourceRevision == "" || revisionNamesCommit(res.SourceRevision, l.w.inbox.lastTip()) {
		return
	}
	l.w.Log.Info("The applier's source holds another commit than the tip known here; looking at the remote",
		"branch", l.w.Branch, "applier", state.ref.String(), "sourceRevision", res.SourceRevision)
	l.w.EnqueuePushNotification(revisionCommit(res.SourceRevision))
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
	case ApplyRequestFailed, ApplyRejected, ApplyDropped, ApplyStale:
	}
	return "", false
}

// recordOperation records an attempt's report in the operation its applier claim's status shows, which
// sendAttempt started as Requested when it sent the attempt. A report that ends an attempt that
// asked for no apply puts back the operation before it, and withdraws its own from the applier claim if
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
	if res.SuspendToken != "" {
		operation.SuspendToken = res.SuspendToken
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
// starts through to the applier claim, just before it starts the apply. That is the only way it reaches
// the applier claim before the requester reports it, so an attempt that ends before it leaves the applier claim
// as it was. Nil without an applier claim status writer.
func (l *branchWorkerEventLoop) recorder(state *applierState, attempt *inflightAttempt) func(context.Context) error {
	writer := l.w.claimStatus
	if writer == nil {
		return nil
	}
	applierClaim, operation := state.ref.ApplierClaim, state.operation.DeepCopy()
	return func(ctx context.Context) error {
		attempt.recorded.Store(true)
		return writer.RecordApplyOperation(ctx, applierClaim, operation)
	}
}

// shownOperation is the operation the applier's applier claim is to show: the last one an attempt
// started, once the requester reported it, and the one before it until then. An attempt that
// follows a recorded operation shows it from the start: the applier claim holds it already.
func (s *applierState) shownOperation() *v1alpha3.ApplyOperation {
	if attempt := s.inflight; attempt != nil && !attempt.recovered && attempt.phase == "" &&
		s.operationID == attempt.id {
		return attempt.previous
	}
	return s.operation
}

// publishClaimStatus hands each applier's applier claim what its status should show, when that changed
// since the last time.
func (l *branchWorkerEventLoop) publishClaimStatus() {
	for _, ref := range l.sortedAppliers() {
		l.publishStatusOf(l.appliers[ref])
	}
}

// publishStatusOf hands one applier's applier claim what its status should show, and the operation it
// withdraws, when that changed since the last time. A worker that has started no operation and
// has no Repair pending has nothing to say yet, and says nothing: the status may hold what an earlier
// process recorded.
func (l *branchWorkerEventLoop) publishStatusOf(state *applierState) {
	writer := l.w.claimStatus
	if writer == nil {
		return
	}
	next := ApplierClaimStatus{
		Operation: state.shownOperation(), Withdrawn: state.withdrawn, RepairPending: state.repairPending(),
	}
	if next.Withdrawn == nil {
		if state.published == nil && next.Operation == nil && !next.RepairPending {
			return
		}
		if state.published != nil && state.published.RepairPending == next.RepairPending &&
			equality.Semantic.DeepEqual(state.published.Operation, next.Operation) {
			return
		}
	}
	writer.PublishApplierClaimStatus(state.ref.ApplierClaim, next)
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
	state.adopt = state.adopt || attempt.adopt
	// The converge given back is only scheduleConverges' when that is all either side holds.
	switch {
	case attempt.converge && !attempt.lag:
		state.lag = false
	case attempt.lag && !state.converge:
		state.lag = true
	}
	state.converge = state.converge || attempt.converge
	state.repair = state.repair || attempt.repair
	for key, refusal := range attempt.refusals {
		if !l.w.refusalStands(key) {
			continue
		}
		if l.unsentRefusals == nil {
			l.unsentRefusals = map[refusalKey]unsentRefusal{}
		}
		if _, unsent := l.unsentRefusals[key]; !unsent {
			l.unsentRefusals[key] = refusal
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

// branchKey is the repository and branch this worker writes.
func (w *BranchWorker) branchKey() BranchKey {
	return BranchKeyFor(w.repo.URL, w.Branch)
}

// provider is the GitProvider this worker writes through: the one that owns its branch.
func (w *BranchWorker) provider() k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Namespace: w.GitProviderNamespace, Name: w.GitProviderRef}
}

// decisionHolds is ApplyRequest.Current for an attempt decided at tip: the branch is still at that
// commit. Nothing else holds it: every edit is either published before the send, or recorded in the
// apply window and repaired should the apply overwrite it (S1). It is called from the requester's
// goroutine, so it reads only what is safe to read there.
func (w *BranchWorker) decisionHolds(tip string) bool {
	return w.inbox.tipIs(tip)
}

// currentFor is ApplyRequest.Current for an attempt decided at tip: decisionHolds, and for an attempt
// that only catches the applier's state up (lag), convergeHolds as well.
func (w *BranchWorker) currentFor(tip string, lag bool) func() bool {
	if !lag {
		return func() bool { return w.decisionHolds(tip) }
	}
	edits := w.liveEdits.Load()
	return func() bool { return w.convergeHolds(tip, edits) }
}

// convergeHolds is the decision of a converge that only catches the applier's state up
// (scheduleConverges): the branch is still at tip, and no live edit was mirrored since the send, the
// count of which was edits then. The fetch before the resume writes nothing to the cluster; the apply
// after it would put tip there, and an edit made meanwhile is not in tip, so the apply would revert it
// and a Repair would put it back: a flip on the cluster, for an apply nothing needed. Such a converge
// yields instead, and waits a fresh interval. An apply with any other reason does not: S1 keeps
// continuous editing from starving it, and the apply window repairs what it overwrites.
func (w *BranchWorker) convergeHolds(tip string, edits uint64) bool {
	return w.decisionHolds(tip) && w.liveEdits.Load() == edits
}

// reportApplyResult is ApplyRequest.Report: it records the report and wakes the loop to settle it.
// It is called from the requester's goroutine.
func (w *BranchWorker) reportApplyResult(res ApplyResult) {
	w.inbox.addResult(res)
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
