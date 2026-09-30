// SPDX-License-Identifier: Apache-2.0

package git

import (
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
// the controller's poll cadence and safety bound.
const commitRequestOutcomeTTL = 15 * time.Minute

// recordCommitRequestOutcome stores a resolved outcome and GCs stale entries. The
// event loop is the only caller, but it takes the mutex because the controller
// reads concurrently via LookupCommitRequestOutcome.
func (w *BranchWorker) recordCommitRequestOutcome(id commitRequestID, result FinalizeResult) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	if w.crOutcomes == nil {
		w.crOutcomes = map[commitRequestID]commitRequestOutcomeEntry{}
	}
	now := time.Now()
	w.crOutcomes[id] = commitRequestOutcomeEntry{result: result, resolvedAt: now}
	for k, entry := range w.crOutcomes {
		if now.Sub(entry.resolvedAt) > commitRequestOutcomeTTL {
			delete(w.crOutcomes, k)
		}
	}
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
	}
	l.pendingCRs[id] = pcr
	l.w.setCommitRequestPhase(id, PhaseWaitingForWindow)
	l.w.Log.Info("CommitRequest registered with worker",
		"request", id.Namespace+"/"+id.Name,
		"author", req.Author,
		"target", req.GitTargetNamespace+"/"+req.GitTargetName,
		"attach", string(req.Attach),
		"attachTimeout", req.AttachTimeout.String())

	if l.openWindow == nil || !pcr.matchesWindow(l.openWindow) {
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
		l.resolveCommitRequest(id, FinalizeResult{Outcome: pcr.expiryOutcome()})
	}
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
