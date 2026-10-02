// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the branch worker's log of decided writes, and the one executor that turns it into
// commits. See docs/design/gittarget-branch-worker-log.md.
//
// Every write the loop makes, whatever produced it (a closed window, an atomic batch, a resync, a
// save's empty record, a refusal's empty commit), takes one path:
//
//   - decide adds it to the log. The write gates and building the write have passed, so the
//     decision stands from here on.
//   - materialize makes the checkout the projection of the log: the remote tip the writes were
//     planned on plus one commit for each, in order. It rebuilds the writes committed before when
//     the checkout is behind them, then commits each decided write not committed yet.
//   - settleCommitted, settleFailed and settleUnreachable are the one place an attempt's outcome
//     is classified and acted on: committed; failed for good, because the write itself was refused
//     or cannot be made; or left in the log, because the remote could not be reached.
//
// materialize is never re-entered. Settling an outcome can decide more work (a refused write's
// empty commit), and that work is appended to the log for the pass already running to commit in
// order, instead of running a second executor inside the first.

// errWriteFailed marks a failure of the write itself, as opposed to reaching the remote or reading
// the GitProvider: the plan was refused or could not be applied. It is terminal for the write, while
// any other commit failure leaves a decided write in the log for the retry. Match it with errors.Is.
var errWriteFailed = errors.New("the write failed")

// writeFailedError carries errWriteFailed without changing the message of the error it wraps.
type writeFailedError struct{ err error }

func (e writeFailedError) Error() string        { return e.err.Error() }
func (e writeFailedError) Unwrap() error        { return e.err }
func (e writeFailedError) Is(target error) bool { return target == errWriteFailed }

// writeOrigin is what a write's outcome is settled against: the caller waiting on a resync, the
// atomic request, or the refusal an empty commit answers. They are process objects, kept beside
// the write's data rather than in it.
type writeOrigin struct {
	resync  *ResyncRequest
	atomic  *WriteRequest
	refusal *refusalTouchOrigin
	// answered records that the resync's caller was answered already: the remote held it back,
	// and it is applied later with nobody waiting.
	answered bool
}

// refusalTouchOrigin is the refusal an empty commit answers, recorded once the commit exists.
type refusalTouchOrigin struct {
	key         refusalKey
	observation string
}

// errAdmissionClosed answers a resync refused at admission. It is ErrFinalizeQueueFull to every
// caller: the work was not accepted, and the producer that sent it keeps it.
var errAdmissionClosed = fmt.Errorf(
	"%w: the branch holds its retained-byte budget while the remote cannot be reached", ErrFinalizeQueueFull)

// syncAdmission closes admission while the log holds the retained-byte budget and a failed attempt
// is waiting for its retry, and opens it otherwise. Run once per loop iteration.
//
// Keeping decided writes through an outage makes the log grow for as long as the outage lasts, and
// nothing in it may be evicted: a decided write can carry a save, and dropping a write without one
// would need a snapshot to re-derive it. So the bound is at admission instead. A refused live write
// is not lost: the watch keeps its cursor and delivers it again, exactly as for a full queue; a save
// is sent again by the controller; a refused resync's collection is gathered again. A healthy branch
// never closes: its log drains at the next push, so the budget only matters while the remote
// refuses. The bound is the budget, plus the window being collected, plus whatever the FIFO already
// holds.
func (l *branchWorkerEventLoop) syncAdmission() {
	budget := l.w.branchBufferMaxBytes
	l.w.admissionClosed.Store(budget > 0 && l.retry.pending() && l.pendingWritesBytes >= budget)
}

// admitWork reports whether new work may enter the queue. Callers hold pendingResyncsMu.
func (w *BranchWorker) admitWork() bool { return !w.admissionClosed.Load() }

// decide adds a write the loop has decided to make to the log, and commits it when it can.
//
// Deciding is not committing. A remote that cannot be reached when the commit would be made leaves
// the write in the log, and the publication retry commits and pushes it. A request riding it is
// held from here on, so the controller never fails a save whose write can still land. Until a pending
// retry is due, a decision that would need a connection to commit waits for that retry instead of
// spending one; a resync is the exception, because its caller is waiting to hear what it found.
//
// It reports whether the write is still in the log afterwards, which is false only when its
// outcome is already settled: it failed for good, or it was a resync that committed nothing.
func (l *branchWorkerEventLoop) decide(pendingWrite PendingWrite) bool {
	l.decisions++
	pendingWrite.seq = l.decisions
	l.pendingWrites = append(l.pendingWrites, pendingWrite)
	l.pendingWritesBytes += pendingWrite.ByteSize
	if id := pendingWrite.CommitRequest; id != nil {
		l.w.setCommitRequestPhase(*id, PhaseWaitingForPush)
		// Bound to its write from here on: see pendingCommitRequest.attached.
		if pcr := l.pendingCRs[*id]; pcr != nil {
			pcr.attached = true
		}
	}
	if l.materializing {
		l.followUps = true
		return true // the pass already running commits it, in order; its starter pushes it
	}
	if l.awaitingRetry() && pendingWrite.Kind != PendingWriteResync &&
		!l.materializeIsLocal() && !l.w.awaitingParentProbe() {
		return true
	}
	l.deciding = pendingWrite.seq
	err := l.materialize()
	l.deciding = 0
	if l.followUps {
		// Settling this write decided more (a refusal's empty commit) that its own caller knows
		// nothing about, and nothing is pushed from inside a pass: schedule the push now that the
		// pass is done, so none of it is left in the checkout.
		l.followUps = false
		l.maybeSchedulePush()
	}
	if err != nil {
		l.noteParentUnavailable(err)
		if (len(l.pendingWrites) > 0 || l.recovery.active) && !l.retry.pending() {
			l.scheduleRetry()
		}
		l.w.Log.Error(err, "Cannot commit the decided writes yet; they wait in the log for the retry",
			"pendingWrites", len(l.pendingWrites), "retryAt", l.retry.due)
	}
	return l.inLog(pendingWrite.seq)
}

// inLog reports whether the decision numbered seq is still in the log.
func (l *branchWorkerEventLoop) inLog(seq uint64) bool {
	for i := range l.pendingWrites {
		if l.pendingWrites[i].seq == seq {
			return true
		}
	}
	return false
}

// materialize makes the checkout the projection of the log, committing every decided write not
// committed yet. It returns the error that stopped it when the remote could not be reached; the
// writes it could not commit stay in the log.
func (l *branchWorkerEventLoop) materialize() error {
	if l.materializing {
		return nil
	}
	l.materializing = true
	defer func() { l.materializing = false }()
	for {
		i := l.materializedPrefix()
		// A resync judges its snapshot against the newest remote tree, so it fetches first whatever
		// the checkout holds.
		refetch := ""
		if i < len(l.pendingWrites) && l.pendingWrites[i].Kind == PendingWriteResync {
			refetch = fetchReasonForcedRecheck
		}
		if err := l.materializePrefix(refetch); err != nil {
			l.settleUnreachable(err)
			return err
		}
		if i == len(l.pendingWrites) {
			return nil
		}
		// A write committed later than it was decided is planned on a newer tree under the policy in
		// force now, exactly as a replay is: an operator who tightened pruning meanwhile is obeyed.
		if l.pendingWrites[i].seq != l.deciding {
			if err := l.w.tightenPendingPruneModes(l.w.ctx, l.pendingWrites[i:i+1]); err != nil {
				l.settleUnreachable(err)
				return err
			}
		}
		err := l.w.commitPendingWrites(l.pendingWrites[i : i+1])
		switch {
		case err == nil:
			l.settleCommitted(i)
		case errors.Is(err, errWriteFailed):
			l.settleFailed(i, err)
		default:
			l.settleUnreachable(err)
			return err
		}
	}
}

// materializePrefix rebuilds the writes committed before when the checkout is behind them: a write
// failed part-way and could not be undone, a reset discarded their commits and the replay did not
// finish, or their root was chosen under an older parent configuration. A parent change is read
// from the generations, not taken from a flag: a rebuild that fails leaves them apart, so the next
// attempt rebuilds again.
//
// refetch, when set, is the fetch series of a caller that needs the remote tip whatever the checkout
// holds. With nothing committed and no refetch there is nothing to project: commitPendingWrites' own
// base check fetches when the base is untrusted or the worktree dirty, which it can do safely
// because nothing is lost.
func (l *branchWorkerEventLoop) materializePrefix(refetch string) error {
	m := l.materializedPrefix()
	if refetch == "" && (m == 0 || l.w.checkoutHolds(m) && !l.w.rootParentStale()) {
		return nil
	}
	// The rebuild fetches, and a parent known to be missing is not fetched again before its probe
	// is due: every commit path comes through here, so without the check each write arriving in the
	// meantime would cost a connection.
	if l.w.awaitingParentProbe() {
		return errAwaitingParentProbe
	}
	if m == 0 {
		// Nothing to replay, so fetch and reset directly. Leaving it to ensureBaseForCycle would
		// work, but that call records `publication`, and a resync is not one.
		return l.w.syncWithRemote(l.w.ctx, refetch)
	}
	reason := refetch
	if reason == "" {
		// The same series the nothing-committed case records in ensureBaseForCycle: this is one
		// event, and which half of it an operator sees must not depend on whether a push happened
		// to be in cooldown at the time.
		reason = fetchReasonRecovery
		l.w.Log.Info("Rebuilding retained writes onto the remote tip",
			"pendingWrites", m,
			"worktreeDirty", l.w.worktreeDirty(),
			"committedWrites", l.w.checkoutApplied.Load())
	}
	return l.w.refreshRemoteAndRebuildPendingWrites(l.w.ctx, l.pendingWrites[:m], reason)
}

// materializedPrefix is how many writes at the head of the log have been committed before. Writes
// not committed yet are always the tail: they are decided in order and committed in order.
func (l *branchWorkerEventLoop) materializedPrefix() int {
	for i := range l.pendingWrites {
		if !l.pendingWrites[i].materialized {
			return i
		}
	}
	return len(l.pendingWrites)
}

// materializeIsLocal reports whether committing a decided write now needs no connection: the
// checkout already holds the writes before it, on a base the worker can vouch for.
func (l *branchWorkerEventLoop) materializeIsLocal() bool {
	if m := l.materializedPrefix(); m > 0 {
		return l.w.checkoutHolds(m) && !l.w.rootParentStale()
	}
	return l.w.baseTrusted() && !l.w.worktreeDirty()
}

// checkoutCurrent reports whether the checkout is the projection of the whole log, so it may be
// pushed as it stands.
func (l *branchWorkerEventLoop) checkoutCurrent() bool {
	m := l.materializedPrefix()
	if m < len(l.pendingWrites) {
		return false
	}
	return m == 0 || l.w.checkoutHolds(m) && !l.w.rootParentStale()
}

// removeAt takes the write at i out of the log.
func (l *branchWorkerEventLoop) removeAt(i int) PendingWrite {
	pendingWrite := l.pendingWrites[i]
	l.pendingWrites = append(l.pendingWrites[:i], l.pendingWrites[i+1:]...)
	l.pendingWritesBytes -= pendingWrite.ByteSize
	return pendingWrite
}

// settleCommitted acts on a write whose commit was made. A resync answers its caller with what it
// found, and one that committed nothing leaves the log: it is neither retained nor pushed. A
// refusal's empty commit records the refusal it covered, so the same observation is not a new
// trigger.
func (l *branchWorkerEventLoop) settleCommitted(i int) {
	l.pendingWrites[i].materialized = true
	pendingWrite := l.pendingWrites[i]
	switch {
	case pendingWrite.origin.resync != nil:
		l.settleResyncApplied(i)
	case pendingWrite.origin.refusal != nil:
		l.w.recordRefusalObservation(pendingWrite.origin.refusal.key, pendingWrite.origin.refusal.observation)
	}
}

// settleResyncApplied answers a resync whose plan was accepted.
func (l *branchWorkerEventLoop) settleResyncApplied(i int) {
	req := l.pendingWrites[i].origin.resync
	answered := l.pendingWrites[i].origin.answered
	stats := l.pendingWrites[i].ResyncStats
	committed := l.pendingWrites[i].retained()
	if !committed {
		l.removeAt(i)
	}
	// The plan was accepted, so the refusal standing over THIS SCOPE is over: the observation the
	// last empty commit covered is forgotten and any commit still queued for it is cancelled. This
	// is the recovery path the dedupe depends on: a per-type reconcile runs after the reconciler
	// reverts the edit, and it is what makes the NEXT refusal, including a re-made byte-identical
	// one, a new trigger rather than a repeat. Scoped to the collection this request evaluated,
	// because a ConfigMap resync succeeding is no evidence about a Deployment that is still refused.
	l.refusalRecovered(
		itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace), req.refusalCollection())
	l.w.Log.Info("Resync request applied",
		"committed", committed,
		"created", stats.Created,
		"updated", stats.Updated,
		"deleted", stats.Deleted,
		"skipped", stats.Skipped,
		"placementSkipped", stats.PlacementSkipped,
		"pendingWrites", len(l.pendingWrites))
	if !answered {
		answerResync(req, ResyncResult{Stats: *stats})
	}
}

// answerResync answers a resync's caller. A resync the remote held back was answered then
// (writeOrigin.answered), and is applied later with nobody waiting: its outcome is then reported the
// way a live write's is.
func answerResync(req *ResyncRequest, result ResyncResult) {
	req.reply(result)
}

// settleFailed acts on a write whose commit failed for good: the plan was refused, or the write
// cannot be made, and retrying the same broken state helps nobody. It leaves the log, a request
// riding it fails, and a refusal is surfaced as GitPathAccepted=False instead of being logged as a
// write fault (a resync's refusal reaches its caller, which classifies it).
func (l *branchWorkerEventLoop) settleFailed(i int, err error) {
	pendingWrite := l.removeAt(i)
	switch {
	case pendingWrite.origin.resync != nil && !pendingWrite.origin.answered:
		req := pendingWrite.origin.resync
		// A refusal reaches the caller on ResyncResult.Err, where the watch layer classifies it
		// and blocks the collection. spec.onRefusal needs it here too, and this is the path that
		// matters: a per-type reconcile evaluates the same objects a live write would, so it is
		// normally what discovers a write-boundary refusal first.
		var refused *manifestanalyzer.AcceptanceRefusedError
		if errors.As(err, &refused) {
			l.touchBranchForRefusal(req.GitTargetName, req.GitTargetNamespace, err.Error(), refused,
				refusalObservationForDesired(req.Desired, refused), req.refusalCollection())
		}
		l.w.Log.Error(err, "Resync commit failed; dropping request", "resources", len(req.Desired))
		answerResync(req, ResyncResult{Err: err})
	case pendingWrite.origin.resync != nil:
		// Nobody is waiting any more: report it as a live write's refusal is reported.
		req := pendingWrite.origin.resync
		collection := req.refusalCollection()
		if isRefusal, refused := l.w.reportPathRefusal(err, req.GitTargetName, req.GitTargetNamespace,
			collection); isRefusal {
			l.touchBranchForRefusal(req.GitTargetName, req.GitTargetNamespace, err.Error(), refused,
				refusalObservationForDesired(req.Desired, refused), collection)
		}
		l.w.Log.Error(err, "Held-back resync commit failed; dropping it", "resources", len(req.Desired))
	case pendingWrite.origin.refusal != nil:
		l.w.Log.Error(err, "The empty commit for a refused write failed",
			"gitTarget", pendingWrite.origin.refusal.key.target.String())
	case pendingWrite.Kind == PendingWriteRequestRecord:
		l.w.Log.Error(err, "The empty commit recording a CommitRequest failed",
			"gitTarget", pendingWrite.GitTargetNamespace+"/"+pendingWrite.GitTargetName)
		l.failDecidedRequest(pendingWrite, fmt.Errorf("record the message in an empty commit: %w", err))
	default:
		l.settleFailedChange(pendingWrite, err)
	}
}

// settleFailedChange settles a window or an atomic batch whose commit failed for good.
func (l *branchWorkerEventLoop) settleFailedChange(pendingWrite PendingWrite, err error) {
	kind, name, namespace, collection := changeIdentity(pendingWrite)
	if isRefusal, refused := l.w.reportPathRefusal(err, name, namespace, collection); isRefusal {
		l.w.recordCommitFailure(kind, commitFailureRefused)
		l.touchBranchForRefusal(name, namespace, err.Error(), refused,
			refusalObservationForEvents(pendingWrite.Events, refused), collection)
	} else {
		l.w.recordCommitFailure(kind, commitFailureError)
		l.w.Log.Error(err, "Commit failed; dropping the write",
			"kind", kind, "gitTarget", namespace+"/"+name, "events", len(pendingWrite.Events))
	}
	l.failDecidedRequest(pendingWrite, fmt.Errorf("commit failed: %w", err))
}

// changeIdentity is a window's or an atomic batch's metric kind, GitTarget, and source collection.
func changeIdentity(pendingWrite PendingWrite) (string, string, string, itypes.CollectionKey) {
	if request := pendingWrite.origin.atomic; request != nil {
		name, namespace := atomicRefusalTarget(request)
		return commitFailureKindAtomic, name, namespace, request.sourceCollection()
	}
	name, namespace := pendingWrite.windowTarget()
	return commitFailureKindWindow, name, namespace, sourceCollectionForEvents(pendingWrite.Events)
}

// settleUnreachable acts on the decided writes not committed yet when the remote could not be
// reached, or the parent branch a new write branch is created from is missing: they all stay in the
// log for the retry, and admission backpressure bounds the log however long that lasts. A resync's
// caller is answered now with the error, because it is waiting to hear what the resync found and
// that cannot be known yet; the resync itself stays, in its place, and is applied with the rest.
func (l *branchWorkerEventLoop) settleUnreachable(err error) {
	for i := l.materializedPrefix(); i < len(l.pendingWrites); i++ {
		origin := &l.pendingWrites[i].origin
		if origin.resync == nil || origin.answered {
			continue
		}
		req := origin.resync
		l.w.Log.Error(err, "Cannot refresh the remote before resync; it waits in the log",
			"resources", len(req.Desired), "gitTarget", req.GitTargetNamespace+"/"+req.GitTargetName)
		answerResync(req, ResyncResult{Err: fmt.Errorf("refresh remote before resync: %w", err)})
		origin.answered = true
	}
}

// failDecidedRequest fails the request a dropped write carried.
func (l *branchWorkerEventLoop) failDecidedRequest(pendingWrite PendingWrite, cause error) {
	if pendingWrite.CommitRequest != nil {
		l.resolveCommitRequest(*pendingWrite.CommitRequest, FinalizeResult{Branch: l.w.Branch, Err: cause})
	}
}
