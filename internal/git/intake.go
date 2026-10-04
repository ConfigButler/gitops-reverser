// SPDX-License-Identifier: Apache-2.0

package git

import (
	"fmt"
	"sync"
)

// This file is the branch's intake gate: whether new payload may enter the worker's FIFO. See
// docs/design/gittarget-branch-worker-log.md, "Recovery contract".
//
// Keeping decided writes through an outage makes the worker's memory grow for as long as the outage
// lasts, and nothing it accepted may be evicted: a decided write can carry a save, and dropping one
// would need a snapshot to re-derive it. So the bound is at intake. While a failed attempt waits
// for its retry, a payload that would take what the branch holds past its retained-byte budget is
// refused, and the refusal pauses the branch: every later payload is refused too, until a
// publication lands. A healthy branch never pauses, because its log drains at the next push.
//
// What the budget counts is everything accepted and not yet published: the payload on the FIFO the
// loop has not handled yet (charged at enqueue, so producers cannot fill the FIFO between two loop
// iterations), and what the loop holds (the open window, the log, deferred heals, saves waiting for
// a window). Every item is charged a fixed overhead beyond its payload, so the budget bounds how
// many items are kept as well as their bytes. The charge is an estimate of the serialized YAML, not
// of the heap: an object's in-memory form is several times larger.
//
// Pausing is a latch, released only when the retry clears: a publication landed, or nothing is owed
// any more. Room under the budget again, a partial replay, or a remote that can be read but still
// refuses the push prove nothing about whether the backlog can drain. A refused producer keeps its
// work: the watch keeps its cursor and delivers again, the controller sends a save again, a refused
// resync's collection is gathered again. Producers wait on IntakePaused for the reopening instead
// of offering the same work again on a timer.
//
// Lifecycle work is not payload and is never refused here: a withdrawal, a refresh tick, shutdown.
// While the branch is paused nothing new enters the FIFO, and the loop keeps draining it, so they
// find room in it.

// intakeGate is the branch's intake state. Enqueue paths take its lock under pendingResyncsMu; the
// loop takes it alone.
type intakeGate struct {
	mu sync.Mutex
	// outage is published by the loop: a failed attempt is waiting for its retry.
	outage bool
	// held is published by the loop: what it holds that is not published yet. See heldBytes.
	held int64
	// queued is the payload accepted onto the FIFO that the loop has not finished handling.
	queued int64
	// paused is non-nil while intake is paused, and closed when it reopens.
	paused chan struct{}
}

// admit charges a payload against the budget and reports whether it may enter. A refused payload
// pauses the branch. A charge that is not admitted is not counted; one admitted is counted until
// release returns it.
func (g *intakeGate) admit(charge, budget int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused != nil {
		return false
	}
	if g.outage && budget > 0 && g.held+g.queued+charge > budget {
		g.paused = make(chan struct{})
		return false
	}
	g.queued += charge
	return true
}

// release returns a charge whose item did not enter the FIFO after all, or that the loop has
// handled: what the item left behind is in held from now on.
func (g *intakeGate) release(charge int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queued -= charge
}

// sync is the loop's publication, once per iteration: whether a retry is pending, and what it
// holds. It pauses a branch whose held work already fills the budget when the outage begins, and
// reopens a paused one once the retry clears.
func (g *intakeGate) sync(outage bool, held, budget int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.outage, g.held = outage, held
	switch {
	case g.paused != nil && !outage:
		close(g.paused)
		g.paused = nil
	case g.paused == nil && outage && budget > 0 && held+g.queued >= budget:
		g.paused = make(chan struct{})
	}
}

// pausedC is the channel closed when a paused branch reopens, or nil while intake is open.
func (g *intakeGate) pausedC() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// IntakePaused reports a branch that has paused intake: it returns a channel closed when intake
// reopens, or nil while it is open. A producer refused while the branch is paused waits on it
// instead of offering its work again on a timer, and checks again once woken, because the branch
// can pause again before it gets there.
func (w *BranchWorker) IntakePaused() <-chan struct{} { return w.intake.pausedC() }

// admitPayload charges an item against the budget. Callers hold pendingResyncsMu.
func (w *BranchWorker) admitPayload(charge int64) bool {
	return w.intake.admit(charge, w.branchBufferMaxBytes)
}

// intakeRefusal is what a refused payload's producer is told: the branch is paused, or this item
// alone is larger than the whole budget, which no amount of draining makes room for while the
// outage lasts.
func (w *BranchWorker) intakeRefusal(charge int64) error {
	if budget := w.branchBufferMaxBytes; budget > 0 && charge > budget {
		return fmt.Errorf("%w: it takes %d bytes, which exceeds the branch's whole retained-byte budget "+
			"of %d; it is accepted once the branch publishes again", ErrFinalizeQueueFull, charge, budget)
	}
	return errAdmissionClosed
}

// eventCharge is what a queued live or atomic write is charged: its events' payload, and the
// overhead of one kept item.
func eventCharge(request *WriteRequest) int64 {
	charge := int64(pendingWriteOverheadBytes)
	for i := range request.Events {
		charge += eventPayloadSize(&request.Events[i])
	}
	return charge
}

// attachCharge is what a save is charged while it is queued and while it waits for a window: its
// message, and the overhead of one kept item.
func attachCharge(message string) int64 {
	return int64(len(message)) + pendingWriteOverheadBytes
}

// eventPayloadSize is the event object's estimated serialized size, computed once: the producer
// computes it at enqueue, off the loop, and the loop reuses it.
func eventPayloadSize(e *Event) int64 {
	if !e.sized {
		e.payloadBytes = estimateObjectSize(e)
		e.sized = true
	}
	return e.payloadBytes
}

// payloadSize is the snapshot's estimated serialized size, computed once like an event's.
func (r *ResyncRequest) payloadSize() int64 {
	if !r.sized {
		r.payloadBytes = estimateDesiredSize(r.Desired)
		r.sized = true
	}
	return r.payloadBytes
}

// charge is what a queued resync is charged: its snapshot, and the overhead of one kept item.
func (r *ResyncRequest) charge() int64 { return r.payloadSize() + pendingWriteOverheadBytes }

// heldBytes is what the loop holds that is not published yet: the open window, the log, the
// deferred heals' snapshots, and the saves waiting for a window. A save bound to a write rides
// that write's charge.
func (l *branchWorkerEventLoop) heldBytes() int64 {
	held := l.windowBytes + l.pendingWritesBytes
	for _, heal := range l.deferredHeals {
		held += heal.charge()
	}
	for _, pcr := range l.pendingCRs {
		if !pcr.attached {
			held += attachCharge(pcr.message)
		}
	}
	return held
}

// syncAdmission publishes the loop's state to the intake gate, and from there to the publication
// report and its gauges. Run once per loop iteration.
func (l *branchWorkerEventLoop) syncAdmission() {
	held := l.heldBytes()
	l.w.intake.sync(l.retry.pending(), held, l.w.branchBufferMaxBytes)
	l.publishPublication(held)
}
