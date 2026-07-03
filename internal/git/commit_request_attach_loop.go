// SPDX-License-Identifier: Apache-2.0

package git

import (
	"time"
)

// This file holds the worker-loop side of CommitRequest eager attach (§6.4 of
// docs/design/stream/commitrequest-design.md). All methods run on the single event
// loop goroutine, so pendingCRs needs no locking; only the resolved-outcome table
// (BranchWorker.crOutcomes) crosses goroutines and is mutex-guarded.
//
// The lifecycle of one CommitRequest, worker-side:
//
//   register (handleAttachCommitRequest) → [no matching window] WaitingForWindow
//                                        → [matching window]     Attached
//   a same-author window opens           → Attached
//   finalize deadline fires while Attached → finalize the window with its message → resolved
//   any other path finalizes the Attached window → same (the message rides the window)
//   deadline fires while WaitingForWindow → resolved NoOpenWindow

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

// hasCommitRequestOutcome reports whether a request is already resolved, so a late
// idempotent re-send of its attach is a no-op.
func (w *BranchWorker) hasCommitRequestOutcome(id commitRequestID) bool {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	_, ok := w.crOutcomes[id]
	return ok
}

// handleAttachCommitRequest registers a CommitRequest with the worker. It stamps
// the finalize deadline once (idempotent re-sends keep the first one) and parks the
// request; serviceCommitRequests — run after every loop wake — does the actual
// attaching and finalizing.
func (l *branchWorkerEventLoop) handleAttachCommitRequest(req *AttachCommitRequest) {
	id := req.id()
	if l.w.hasCommitRequestOutcome(id) {
		return // already resolved: a late idempotent re-send.
	}
	if l.pendingCRs == nil {
		l.pendingCRs = map[commitRequestID]*pendingCommitRequest{}
	}
	if _, exists := l.pendingCRs[id]; exists {
		return // idempotent re-send: keep the first finalize deadline.
	}
	// Anchor the grace at receipt (≈ the attribution moment, §6.4.4), not at object
	// creation: under a delayed ingestion pipeline this lets delaySeconds cover only
	// the inter-stream spread instead of the absolute latency.
	l.pendingCRs[id] = &pendingCommitRequest{
		id:                 id,
		author:             req.Author,
		gitTargetName:      req.GitTargetName,
		gitTargetNamespace: req.GitTargetNamespace,
		message:            req.Message,
		finalizeAt:         time.Now().Add(time.Duration(req.CloseDelaySeconds) * time.Second),
	}
	l.w.Log.Info("CommitRequest registered with worker",
		"request", id.Namespace+"/"+id.Name,
		"author", req.Author,
		"target", req.GitTargetNamespace+"/"+req.GitTargetName,
		"closeDelaySeconds", req.CloseDelaySeconds)
}

// serviceCommitRequests runs after every loop wake: bind any waiting request to an
// open same-author window, finalize/reject any whose grace has elapsed, and re-arm
// the deadline timer for the next one.
func (l *branchWorkerEventLoop) serviceCommitRequests() {
	if len(l.pendingCRs) == 0 {
		l.stopAttachTimer()
		return
	}
	l.attachWaitingCommitRequests()
	l.processDueCommitRequests()
	l.rearmAttachTimer()
}

// attachWaitingCommitRequests binds the oldest waiting same-author request to the
// open window when it is unclaimed. A window carries at most one request; a second
// waits for the next window (§6.4.6).
func (l *branchWorkerEventLoop) attachWaitingCommitRequests() {
	if l.openWindow == nil || l.openWindow.pendingCR != nil {
		return
	}
	var oldest *pendingCommitRequest
	for _, pcr := range l.pendingCRs {
		if pcr.attached || !pcr.matchesWindow(l.openWindow) {
			continue
		}
		if oldest == nil || pcr.finalizeAt.Before(oldest.finalizeAt) {
			oldest = pcr
		}
	}
	if oldest != nil {
		l.attachToOpenWindow(oldest)
	}
}

// attachToOpenWindow binds a request's message to the currently-open window.
func (l *branchWorkerEventLoop) attachToOpenWindow(pcr *pendingCommitRequest) {
	l.openWindow.pendingMessage = pcr.message
	id := pcr.id
	l.openWindow.pendingCR = &id
	pcr.attached = true
	l.w.Log.Info("CommitRequest attached to open window",
		"request", id.Namespace+"/"+id.Name,
		"author", pcr.author,
		"target", pcr.gitTargetNamespace+"/"+pcr.gitTargetName)
}

// processDueCommitRequests finalizes the windows of attached requests whose grace
// has elapsed and rejects parked requests whose grace elapsed without a window.
func (l *branchWorkerEventLoop) processDueCommitRequests() {
	now := time.Now()
	var due []commitRequestID
	for id, pcr := range l.pendingCRs {
		if !pcr.finalizeAt.After(now) {
			due = append(due, id)
		}
	}
	for _, id := range due {
		pcr := l.pendingCRs[id]
		if pcr == nil {
			continue
		}
		if l.openWindow != nil && l.openWindow.pendingCR != nil && *l.openWindow.pendingCR == id {
			// The attached window's grace elapsed: finalize it. finalizeOpenWindowWithMessage
			// resolves the request from the window's pendingCR; push so the commit lands.
			l.finalizeOpenWindowWithReason(windowFinalizeReasonFinalizeSignal)
			l.maybeSchedulePush()
			// Belt-and-suspenders: a window it claimed always resolves it on finalize.
			if _, still := l.pendingCRs[id]; still {
				l.resolveCommitRequest(id, FinalizeResult{Outcome: FinalizeNoOpenWindow})
			}
			continue
		}
		// Grace elapsed with no matching same-author window collected.
		l.resolveCommitRequest(id, FinalizeResult{Outcome: FinalizeNoOpenWindow})
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
	l.w.Log.Info("CommitRequest resolved",
		"request", id.Namespace+"/"+id.Name,
		"outcome", string(result.Outcome),
		"sha", result.SHA,
		"err", result.Err)
}

// rearmAttachTimer arms the deadline timer for the earliest pending finalize, so an
// attached window is finalized at the end of its grace even with no further events.
func (l *branchWorkerEventLoop) rearmAttachTimer() {
	var earliest time.Time
	for _, pcr := range l.pendingCRs {
		if earliest.IsZero() || pcr.finalizeAt.Before(earliest) {
			earliest = pcr.finalizeAt
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
