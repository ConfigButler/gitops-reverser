// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// This file holds the worker-loop side of CommitRequest attach. All methods run on the single
// event loop goroutine, so pendingCRs needs no locking; only the resolved-outcome and phase tables
// (BranchWorker.crOutcomes, crPhases) cross goroutines and are mutex-guarded.
//
// The lifecycle of one CommitRequest, worker-side. A window is only ever opened by a write; a
// request attaches to one and never opens one.
//
//   register (handleAttachCommitRequest) → attach: Next closes the author's open window first
//                                        → [matching window open, CurrentOrNext] Attached
//                                        → otherwise WaitingForWindow
//   a same-author window opens before the attach deadline → Attached (first come, first served)
//   Attached: the request's timers replace the window's; whichever path closes the window
//             carries the request's message → WaitingForPush → resolved on push
//   attach deadline passes while WaitingForWindow → resolved NoOpenWindow, or WindowMismatch when
//             a window it could not attach to was open meanwhile; never a later window

// commitRequestOutcomeTTL bounds how long a resolved CommitRequest outcome is
// retained for the controller to poll before it is GC'd. It comfortably exceeds
// the controller's poll cadence. It is not what keeps a withdrawn request withdrawn:
// an outcome is never collected while an attach for it is still queued (crQueuedAttaches),
// and the controller sends no attach once it has started withdrawing.
const commitRequestOutcomeTTL = 15 * time.Minute

// ErrBranchWorkerStopped is the outcome of a request whose branch worker stopped before it could
// finish it. Once the loop has exited nothing can publish the request, so this answer is final.
var ErrBranchWorkerStopped = errors.New("the GitTarget's branch worker stopped before it finished the CommitRequest")

// recordCommitRequestOutcome stores a resolved outcome and GCs stale entries. The
// event loop is the only caller, but it takes the mutex because the controller
// reads concurrently via LookupCommitRequestOutcome.
func (w *BranchWorker) recordCommitRequestOutcome(id commitRequestID, result FinalizeResult) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	w.recordCommitRequestOutcomeLocked(id, result)
}

// recordCommitRequestOutcomeLocked is recordCommitRequestOutcome for a caller holding crOutcomesMu.
func (w *BranchWorker) recordCommitRequestOutcomeLocked(id commitRequestID, result FinalizeResult) {
	if w.crOutcomes == nil {
		w.crOutcomes = map[commitRequestID]commitRequestOutcomeEntry{}
	}
	now := time.Now()
	w.crOutcomes[id] = commitRequestOutcomeEntry{result: result, resolvedAt: now}
	for k, entry := range w.crOutcomes {
		if now.Sub(entry.resolvedAt) > commitRequestOutcomeTTL && w.crQueuedAttaches[k] == 0 {
			delete(w.crOutcomes, k)
			w.crOwners.release(k, w)
		}
	}
}

// countQueuedAttach records an attach entering the FIFO. The caller holds pendingResyncsMu, so the
// count is raised before the loop can receive the item.
func (w *BranchWorker) countQueuedAttach(id commitRequestID) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if w.crQueuedAttaches == nil {
		w.crQueuedAttaches = map[commitRequestID]int{}
	}
	w.crQueuedAttaches[id]++
}

// uncountQueuedAttach records an attach leaving the FIFO: handled by the loop, dropped on a full
// queue, or drained by an exiting worker. A request the worker then knows nothing else about is
// given back, so the controller's next poll finds the worker its GitTarget names.
func (w *BranchWorker) uncountQueuedAttach(id commitRequestID) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if n := w.crQueuedAttaches[id]; n > 1 {
		w.crQueuedAttaches[id] = n - 1
		return
	}
	delete(w.crQueuedAttaches, id)
	_, resolved := w.crOutcomes[id]
	_, registered := w.crPhases[id]
	if !resolved && !registered {
		w.crOwners.release(id, w)
	}
}

// settleCommitRequestsAfterExit runs once the event loop has returned: after a shutdown, after a
// retirement, or when the loop never started because its GitProvider could not be read. Either way
// nothing will act on a request again, and every request the worker knew must get an answer
// rather than wait for one that cannot come.
//
//   - A request the worker held (attached, or committed) fails with ErrBranchWorkerStopped. A clean
//     shutdown has already settled those through its last push; this catches what it could not.
//   - A request it never acted on is given back. The old loop can no longer commit it, so the
//     controller is free to send it to the worker the GitTarget names now, or to withdraw it.
//
// From then on a withdraw is answered at once (answerWithdrawAfterExit).
func (w *BranchWorker) settleCommitRequestsAfterExit() {
	w.pendingResyncsMu.Lock()
	w.stoppingState = true
	w.pendingResyncsMu.Unlock()

	// A loop that never ran left its queue as the producers filled it.
	w.drainQueue()

	w.crOutcomesMu.Lock()
	for id, phase := range w.crPhases {
		if phase.Held() {
			w.recordCommitRequestOutcomeLocked(id, FinalizeResult{Branch: w.Branch, Err: ErrBranchWorkerStopped})
		} else {
			w.crOwners.release(id, w)
		}
		delete(w.crPhases, id)
	}
	w.crOutcomesMu.Unlock()

	w.pendingResyncsMu.Lock()
	w.exitedState = true
	w.pendingResyncsMu.Unlock()
}

// answerWithdrawAfterExit resolves a withdraw sent to a worker whose loop has exited. Nothing can
// act on the request any more, so it is withdrawn for good, unless it already has an outcome.
func (w *BranchWorker) answerWithdrawAfterExit(id commitRequestID) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if _, resolved := w.crOutcomes[id]; resolved {
		return
	}
	w.recordCommitRequestOutcomeLocked(id, FinalizeResult{
		Branch: w.Branch,
		Err:    errors.Join(ErrCommitRequestWithdrawn, ErrBranchWorkerStopped),
	})
}

// LookupCommitRequestOutcome returns a resolved CommitRequest outcome, or ok=false
// when the request is still in flight (or already GC'd). The controller polls this
// after sending its AttachCommitRequest.
func (w *BranchWorker) LookupCommitRequestOutcome(namespace, name, uid string) (FinalizeResult, bool) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	entry, ok := w.crOutcomes[commitRequestID{Namespace: namespace, Name: name, UID: uid}]
	return entry.result, ok
}

// setCommitRequestPhase records where an unresolved request stands; an empty phase forgets it.
func (w *BranchWorker) setCommitRequestPhase(id commitRequestID, phase CommitRequestPhase) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if phase == "" {
		delete(w.crPhases, id)
		return
	}
	if w.crPhases == nil {
		w.crPhases = map[commitRequestID]CommitRequestPhase{}
	}
	w.crPhases[id] = phase
}

// LookupCommitRequestPhase returns where an unresolved request stands on this worker, or "" when
// the worker has not registered it (yet) or it is resolved.
func (w *BranchWorker) LookupCommitRequestPhase(namespace, name, uid string) CommitRequestPhase {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	return w.crPhases[commitRequestID{Namespace: namespace, Name: name, UID: uid}]
}

// hasCommitRequestOutcome reports whether a request is already resolved, so a late
// idempotent re-send of its attach is a no-op.
func (w *BranchWorker) hasCommitRequestOutcome(id commitRequestID) bool {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	_, ok := w.crOutcomes[id]
	return ok
}

// handleAttachCommitRequest registers a CommitRequest with the worker. Everything is stamped once:
// an idempotent re-send restarts nothing and closes nothing.
func (l *branchWorkerEventLoop) handleAttachCommitRequest(req *AttachCommitRequest) {
	id := req.id()
	// Uncounted only once the request is registered or resolved, so the worker never looks as if it
	// had forgotten a request it is in the middle of taking.
	defer l.w.uncountQueuedAttach(id)
	if l.w.hasCommitRequestOutcome(id) {
		return // already resolved: a late idempotent re-send.
	}
	if l.pendingCRs == nil {
		l.pendingCRs = map[commitRequestID]*pendingCommitRequest{}
	}
	if _, exists := l.pendingCRs[id]; exists {
		return // idempotent re-send: keep the first registration.
	}
	// Anchor the wait at registration (≈ the attribution moment), not at object creation: under a
	// delayed ingestion pipeline this lets attachTimeout cover only the inter-stream spread
	// instead of the absolute latency.
	l.crSeq++
	pcr := &pendingCommitRequest{
		id:                 id,
		author:             req.Author,
		attribution:        req.Attribution,
		gitTargetName:      req.GitTargetName,
		gitTargetNamespace: req.GitTargetNamespace,
		message:            req.Message,
		seq:                l.crSeq,
		attachDeadline:     time.Now().Add(req.AttachTimeout),
		idleTimeout:        req.IdleTimeout,
		maxDuration:        req.MaxDuration,
		commitEmpty:        req.CommitEmpty,
	}
	l.pendingCRs[id] = pcr
	l.w.setCommitRequestPhase(id, PhaseWaitingForWindow)
	l.w.Log.Info("CommitRequest registered with worker",
		"request", id.Namespace+"/"+id.Name,
		"author", req.Author,
		"target", req.GitTargetNamespace+"/"+req.GitTargetName,
		"attach", string(req.Attach),
		"attachTimeout", req.AttachTimeout.String())

	if l.openWindow == nil {
		return
	}
	if !pcr.matchesWindow(l.openWindow) {
		// Noted here and not left to noteForeignWindow, which skips a request whose deadline has
		// passed: with attachTimeout: 0s that is every request, so a foreign window open at
		// registration would go unseen and the request would resolve NoWindow — and, with
		// CommitEmpty, record an empty commit while another author's work is in flight.
		pcr.sawForeignWindow = true
		return
	}
	if req.Attach == v1alpha3.AttachNext {
		// Start clean: close the author's window under whatever message it already carries,
		// including one another request attached, and wait for a write to open the next. The
		// registration above is what makes this happen once per request.
		l.finalizeOpenWindowWithReason(windowFinalizeReasonAttachNext)
		l.maybeSchedulePush()
		return
	}
	if l.openWindow.pendingCR == nil {
		// CurrentOrNext attaches to a window already open at registration before any deadline is
		// consulted, which is what lets attachTimeout: 0s attach at all.
		l.attachToOpenWindow(pcr)
	}
}

// serviceCommitRequests runs after every loop wake: note foreign windows, resolve waiting requests
// whose attach deadline passed, attach the next waiting request to an open window, and re-arm the
// attach timer.
//
// Expiry runs BEFORE attach. When a request's deadline and a matching write are ready on the same
// wake, the request has already run out: it resolves, and the write keeps its own window.
func (l *branchWorkerEventLoop) serviceCommitRequests() {
	if len(l.pendingCRs) == 0 {
		l.stopAttachTimer()
		return
	}
	l.noteForeignWindow()
	l.expireWaitingCommitRequests()
	l.attachWaitingCommitRequests()
	l.rearmAttachTimer()
}

// waiting reports whether a request is still looking for a window it may attach to at now.
func (p *pendingCommitRequest) waiting(now time.Time) bool {
	return !p.attached && !p.committed && p.attachDeadline.After(now)
}

// noteForeignWindow records, on every waiting request, that a window it cannot attach to is open.
//
// It runs BEFORE attachWaitingCommitRequests and independently of it, because that function
// returns early when the window is already taken — so the scan inside it cannot be the place
// this is observed. It also runs on every pass rather than at expiry: the foreign window is
// finalized on its own timer, usually before this request's wait runs out.
//
// A window that MATCHES but already carries another request is not a mismatch. That is
// contention between two of one author's own saves, it resolves on the next window, and calling
// it a mismatch would attribute a queueing delay to the wrong cause.
//
// A request whose deadline has ALREADY passed is skipped: an event arriving at the moment of expiry
// would otherwise open a window, mark the overdue request, and resolve it as a mismatch in the same
// pass. Nothing refused that request; it waited out its whole wait with nothing open, which is the
// benign outcome.
func (l *branchWorkerEventLoop) noteForeignWindow() {
	if l.openWindow == nil {
		return
	}
	now := time.Now()
	for _, pcr := range l.pendingCRs {
		if !pcr.waiting(now) || pcr.sawForeignWindow || pcr.matchesWindow(l.openWindow) {
			continue
		}
		pcr.sawForeignWindow = true
		l.w.Log.Info("CommitRequest is waiting on a window that belongs to someone else",
			"request", pcr.id.Namespace+"/"+pcr.id.Name,
			"requestAuthor", pcr.author,
			"requestTarget", pcr.gitTargetNamespace+"/"+pcr.gitTargetName,
			"windowAuthor", l.openWindow.Author,
			"windowTarget", l.openWindow.GitTargetNamespace+"/"+l.openWindow.GitTarget)
	}
}

// attachWaitingCommitRequests attaches the first-registered waiting request that matches the open
// window, when the window carries none yet. A window carries at most one request; a second waits
// for the next window, and a request whose deadline has passed takes none.
func (l *branchWorkerEventLoop) attachWaitingCommitRequests() {
	if l.openWindow == nil || l.openWindow.pendingCR != nil {
		return
	}
	now := time.Now()
	var first *pendingCommitRequest
	for _, pcr := range l.pendingCRs {
		if !pcr.waiting(now) || !pcr.matchesWindow(l.openWindow) {
			continue
		}
		if first == nil || pcr.seq < first.seq {
			first = pcr
		}
	}
	if first != nil {
		l.attachToOpenWindow(first)
	}
}

// attachToOpenWindow binds a request's message to the open window and replaces the window's timers
// with the request's own: the request's maxDuration runs from this attach, and so does its first
// idle interval. A zero maxDuration closes the window right here.
func (l *branchWorkerEventLoop) attachToOpenWindow(pcr *pendingCommitRequest) {
	now := time.Now()
	l.openWindow.pendingMessage = pcr.message
	id := pcr.id
	l.openWindow.pendingCR = &id
	pcr.attached = true
	timers := windowTimers{noIdle: pcr.idleTimeout == nil, maxAt: now.Add(pcr.maxDuration)}
	if pcr.idleTimeout != nil {
		timers.idle = *pcr.idleTimeout
	}
	l.openWindow.timers = timers
	l.openWindow.lastWriteAt = now
	l.w.setCommitRequestPhase(id, PhaseCollectingWindow)
	l.w.Log.Info("CommitRequest attached to open window",
		"request", id.Namespace+"/"+id.Name,
		"author", pcr.author,
		"target", pcr.gitTargetNamespace+"/"+pcr.gitTargetName,
		"maxDuration", pcr.maxDuration.String())
	l.closeOrArmWindow()
}

// expireWaitingCommitRequests resolves every request whose attach deadline passed while it was
// still waiting. An attached request is not here: its window's timers close it, and whichever path
// closes the window resolves it.
func (l *branchWorkerEventLoop) expireWaitingCommitRequests() {
	now := time.Now()
	for id, pcr := range l.pendingCRs {
		if pcr.attached || pcr.committed || pcr.attachDeadline.After(now) {
			continue
		}
		// Which of the two refusals this is depends on whether anything was open that this
		// request could not have: see pendingCommitRequest.expiryOutcome.
		outcome := pcr.expiryOutcome()
		if outcome == FinalizeNoOpenWindow {
			// NoWindow says the save saw no writes. On a target that refuses them that is not the
			// cause, and with CommitEmpty the record would say so in Git: see write_gate.go.
			if err := l.w.requestWriteRefusal(l.w.ctx, pcr); err != nil {
				l.resolveCommitRequest(id, FinalizeResult{Err: err})
				continue
			}
		}
		if outcome == FinalizeNoOpenWindow && pcr.commitEmpty {
			recorded, err := l.recordCommitRequest(pcr)
			if err != nil {
				// The request asked for its message to be recorded and it was not: a failure, not
				// a benign NoWindow that would report Ready=True for a save that left no trace.
				l.resolveCommitRequest(id, FinalizeResult{Err: err})
				continue
			}
			if recorded {
				continue // it now rides the record write, and the push resolves it
			}
		}
		l.resolveCommitRequest(id, FinalizeResult{Outcome: outcome})
	}
}

// recordCommitRequest commits a request's message with an untouched tree, for a request that ran
// out of time to attach and asked for whenNothingToCommit: CommitEmpty. Every reason no record was
// made is returned, including a write gate (an empty commit is a write too), so the request fails
// instead of reporting a save it did not make.
func (l *branchWorkerEventLoop) recordCommitRequest(pcr *pendingCommitRequest) (bool, error) {
	pendingWrite, err := l.w.buildRequestRecordWrite(l.w.ctx, pcr)
	if err != nil {
		l.w.Log.Error(err, "Cannot build the empty commit recording a CommitRequest",
			"request", pcr.id.Namespace+"/"+pcr.id.Name)
		return false, fmt.Errorf("record the message in an empty commit: %w", err)
	}
	// An empty commit is only empty on a clean worktree: a failed write's staged leftovers would
	// otherwise ride along in it.
	if err := l.materialize(""); err != nil {
		l.w.Log.Error(err, "Cannot recover the worktree for the empty commit recording a CommitRequest",
			"request", pcr.id.Namespace+"/"+pcr.id.Name)
		return false, fmt.Errorf("record the message in an empty commit: %w", err)
	}
	batch := []PendingWrite{*pendingWrite}
	if err := l.commit(batch); err != nil {
		l.w.Log.Error(err, "The empty commit recording a CommitRequest failed",
			"request", pcr.id.Namespace+"/"+pcr.id.Name)
		return false, fmt.Errorf("record the message in an empty commit: %w", err)
	}
	l.retain(batch[0])
	pcr.committed = true
	l.w.setCommitRequestPhase(pcr.id, PhaseWaitingForPush)
	l.maybeSchedulePush()
	return true, nil
}

// buildRequestRecordWrite assembles the record commit's write, phrased and signed like every other
// commit the target makes and authored by the request's submitter. A target that refuses writes
// gets none, and the refusal is returned (see write_gate.go).
func (w *BranchWorker) buildRequestRecordWrite(
	ctx context.Context,
	pcr *pendingCommitRequest,
) (*PendingWrite, error) {
	metadata, err := w.resolveTargetMetadata(ctx, pcr.gitTargetName, pcr.gitTargetNamespace)
	if err != nil {
		return nil, err
	}
	if err := w.targetWriteRefusal(pcr.gitTargetName, pcr.gitTargetNamespace, metadata.Suspend); err != nil {
		return nil, err
	}
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, err
	}
	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, err
	}
	id := pcr.id
	return &PendingWrite{
		Kind:          PendingWriteRequestRecord,
		CommitMessage: pcr.message,
		CommitConfig: ResolveCommitConfig(provider.Spec.Commit).
			WithTargetMessage(metadata.CommitMessage),
		Signer:             signer,
		GitTargetName:      pcr.gitTargetName,
		GitTargetNamespace: pcr.gitTargetNamespace,
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: pcr.gitTargetName, Namespace: pcr.gitTargetNamespace}: metadata,
		},
		RequestAuthor:      UserInfo{Username: pcr.author},
		RequestAttribution: pcr.attribution,
		CommitRequest:      &id,
	}, nil
}

// handleWithdrawCommitRequest cancels a request the worker has not acted on: one that is only
// waiting for a window, or that was never registered at all. A request attached to a window or
// already committed is held, and withdrawing it is a no-op; the controller reads the held phase and
// keeps waiting. Either way the worker's answer and the controller's agree.
func (l *branchWorkerEventLoop) handleWithdrawCommitRequest(req *AttachCommitRequest) {
	id := req.id()
	if l.w.hasCommitRequestOutcome(id) {
		return // already resolved, or already withdrawn.
	}
	if pcr, ok := l.pendingCRs[id]; ok && (pcr.attached || pcr.committed) {
		l.w.Log.Info("CommitRequest withdraw ignored: the worker already holds it",
			"request", id.Namespace+"/"+id.Name)
		return
	}
	l.resolveCommitRequest(id, FinalizeResult{Err: ErrCommitRequestWithdrawn})
	l.rearmAttachTimer()
}

// resolveCommitRequest records a request's terminal outcome for the controller to
// poll and forgets it from the pending set.
func (l *branchWorkerEventLoop) resolveCommitRequest(id commitRequestID, result FinalizeResult) {
	if result.Branch == "" {
		result.Branch = l.w.Branch
	}
	l.w.recordCommitRequestOutcome(id, result)
	delete(l.pendingCRs, id)
	l.w.setCommitRequestPhase(id, "")
	l.w.Log.Info("CommitRequest resolved",
		"request", id.Namespace+"/"+id.Name,
		"outcome", string(result.Outcome),
		"commit", result.Commit,
		"err", result.Err)
}

// rearmAttachTimer arms the timer for the earliest attach deadline among waiting requests, so a
// request no window reaches still resolves on time.
func (l *branchWorkerEventLoop) rearmAttachTimer() {
	var earliest time.Time
	for _, pcr := range l.pendingCRs {
		if pcr.attached || pcr.committed {
			continue
		}
		if earliest.IsZero() || pcr.attachDeadline.Before(earliest) {
			earliest = pcr.attachDeadline
		}
	}
	if earliest.IsZero() {
		l.stopAttachTimer()
		return
	}
	delay := time.Until(earliest)
	if delay < 0 {
		delay = 0
	}
	if l.attachTimer == nil {
		l.attachTimer = time.NewTimer(delay)
		return
	}
	if !l.attachTimer.Stop() {
		select {
		case <-l.attachTimer.C:
		default:
		}
	}
	l.attachTimer.Reset(delay)
}

func (l *branchWorkerEventLoop) stopAttachTimer() {
	if l.attachTimer == nil {
		return
	}
	if !l.attachTimer.Stop() {
		select {
		case <-l.attachTimer.C:
		default:
		}
	}
	l.attachTimer = nil
}
