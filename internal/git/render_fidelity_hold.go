// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sort"
	"time"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file holds a GitTarget's live events while its render fidelity is pending, rather than drop
// them (Fix 2 in docs/design/live-edit-dropped-while-render-fidelity-pending.md).
//
// The gate is per GitTarget and its scopes per collection, so one collection that is still
// replaying (a new or restarted collection, every collection at operator start, a snapshot F3
// discarded) closes live writes for all of them. A dropped event never comes back: the watch has
// moved past it. So an event that meets a pending gate is judged at once, as on arrival (the
// ownership check, the apply windows), and then HELD:
//
//   - When the gate turns True, the GitTarget's held events are released in arrival order, before
//     any later event of it, and committed with their authors. A create is noted for adoption then,
//     not on arrival: an apply sent before the create is in a commit stamps nothing, and spends the
//     adopt (F-c, which lost it outright: the gate returned before the create was noted at all).
//   - When it turns False, a divergence, they are dropped and counted, as before: that is the gate
//     doing its job.
//   - An applied snapshot of a collection supersedes the events of that collection held before it
//     was taken off the queue: they are older than its list, which holds their effect. A create it
//     supersedes is noted for adoption, since the snapshot's commit holds the object. A DELETE of an
//     object the snapshot does not hold stays held, because a snapshot removes a document only under
//     prune mode Always.
//   - The hold is bounded, per GitTarget by renderFidelityHoldLimit, and per branch by the
//     retained-byte budget. Past either, the GitTarget's held events are dropped and counted, and a
//     fresh snapshot of it is asked for, which brings the state back as the operator's commit. A
//     DELETE among them comes back only under prune mode Always; the log says how many there were.
//
// Held events count against the intake budget like the open window and the pending writes.

// renderFidelityHoldLimit is how long a GitTarget's live events may be held, from the first one: at
// least as long as F3 holds its snapshots (snapshotHoldLimit), so a discarded snapshot's fresh one
// supersedes them rather than the limit.
const renderFidelityHoldLimit = 2 * snapshotHoldLimit

// renderFidelityRecheckInterval is how soon a GitTarget whose events are held looks at its gate
// again: the gate changes on other goroutines (the watch layer's resync drain, a plan change) without
// waking the loop.
const renderFidelityRecheckInterval = time.Second

// heldWrites is one GitTarget's live events held while its render fidelity is pending.
type heldWrites struct {
	// since is when the first of them was held.
	since  time.Time
	events []heldEvent
	// deletes counts the DELETEs among them.
	deletes int
}

// heldEvent is one live event held, numbered in the order the loop took it.
type heldEvent struct {
	seq    uint64
	event  Event
	charge int64
}

// holdAtRenderFidelity holds a live event of a GitTarget whose render fidelity is pending.
func (l *branchWorkerEventLoop) holdAtRenderFidelity(target itypes.ResourceReference, event Event,
	status RenderFidelityStatus,
) {
	held := l.held[target]
	if held == nil {
		if l.held == nil {
			l.held = map[itypes.ResourceReference]*heldWrites{}
		}
		held = &heldWrites{since: l.w.now()}
		l.held[target] = held
		l.w.Log.Info("Holding the GitTarget's live writes until its render fidelity is established; they "+
			"are committed in order once it is", "gitTarget", target.String(), "renderFidelity", status.Message)
	}
	l.heldSeq++
	charge := eventRetainedCharge(&event)
	held.events = append(held.events, heldEvent{seq: l.heldSeq, event: event, charge: charge})
	if event.Operation == string(itypes.OperationDelete) {
		held.deletes++
	}
	l.heldEventBytes += charge
	l.w.Log.V(1).Info("Held a live write while the GitTarget's render fidelity is pending",
		"gitTarget", target.String(), "resource", event.Identifier.String(), "held", len(held.events))
	if budget := l.w.branchBufferMaxBytes; budget > 0 && l.heldEventBytes > budget {
		for _, ref := range l.heldTargets() {
			l.giveUpHold(ref, "the live writes held for the branch exceed its retained-byte budget")
		}
	}
}

// serviceHeldWrites settles every GitTarget whose events are held, at the start of each wake, and
// arms the recheck while any are.
func (l *branchWorkerEventLoop) serviceHeldWrites() {
	for _, target := range l.heldTargets() {
		l.serviceHeld(target)
	}
	l.stopHoldTimer()
	if len(l.held) > 0 {
		l.holdTimer = time.NewTimer(renderFidelityRecheckInterval)
	}
}

// serviceHeld settles one GitTarget's held events by its gate now: released once it is True,
// dropped once it is False, given up once the hold reached its limit.
func (l *branchWorkerEventLoop) serviceHeld(target itypes.ResourceReference) {
	held := l.held[target]
	if held == nil {
		return
	}
	status, closed := l.w.renderFidelityClosed(target.Name, target.Namespace)
	switch {
	case !closed:
		l.releaseHeld(target, held)
	case status.State == RenderFidelityFalse:
		l.takeHeld(target)
		for _, h := range held.events {
			l.dropAtRenderFidelity(target, status, writeDroppedEvent, h.event.Identifier.String())
		}
	case l.w.now().Sub(held.since) >= renderFidelityHoldLimit:
		l.giveUpHold(target, "the GitTarget's render fidelity has been pending for "+
			renderFidelityHoldLimit.String())
	}
}

// releaseHeld commits a GitTarget's held events, in the order they were held.
func (l *branchWorkerEventLoop) releaseHeld(target itypes.ResourceReference, held *heldWrites) {
	l.takeHeld(target)
	l.w.Log.Info("The GitTarget's render fidelity is established; committing the live writes held for it",
		"gitTarget", target.String(), "writes", len(held.events),
		"heldFor", l.w.now().Sub(held.since).Round(time.Millisecond).String())
	for _, h := range held.events {
		if h.event.Operation == string(itypes.OperationCreate) {
			l.noteCreated(h.event)
		}
		l.appendLiveEvent(h.event)
	}
}

// giveUpHold drops a GitTarget's held events past the hold's bounds, and asks for a fresh snapshot of
// it, which re-derives what they changed.
func (l *branchWorkerEventLoop) giveUpHold(target itypes.ResourceReference, why string) {
	held := l.takeHeld(target)
	if held == nil {
		return
	}
	for range held.events {
		l.recordWriteDropped(writeDroppedEvent, writeDropRenderFidelityPending)
	}
	l.w.Log.Info("Dropped the live writes held for a GitTarget, and asked for a fresh snapshot of it, which "+
		"commits the current state as the operator. A deletion comes back only under prune mode Always",
		"gitTarget", target.String(), "reason", why, "writes", len(held.events), "deletions", held.deletes)
	if l.w.resyncRecheck != nil {
		l.w.resyncRecheck(target)
	}
}

// supersedeHeld drops the events of a GitTarget's collection held before a snapshot of that
// collection was taken off the queue (req.heldUpTo), once the snapshot is applied: it is newer than
// each of them, and holds their effect. A DELETE of an object the snapshot does not hold is kept,
// since only prune mode Always removes a document the snapshot lacks. Events of other collections,
// and every event held since, stay held.
func (l *branchWorkerEventLoop) supersedeHeld(req *ResyncRequest) {
	target := itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace)
	held := l.held[target]
	if held == nil {
		return
	}
	inSnapshot := make(map[string]bool, len(req.Desired))
	for _, dr := range req.Desired {
		inSnapshot[objectKey(dr.Resource)] = true
	}
	kept := held.events[:0]
	superseded := 0
	for _, h := range held.events {
		if !supersededBy(req, h, inSnapshot) {
			kept = append(kept, h)
			continue
		}
		switch itypes.OperationType(h.event.Operation) {
		case itypes.OperationDelete:
			held.deletes--
		case itypes.OperationCreate:
			l.noteCreated(h.event)
		case itypes.OperationUpdate:
		}
		l.heldEventBytes -= h.charge
		superseded++
	}
	held.events = kept
	if superseded == 0 {
		return
	}
	l.w.Log.V(1).Info("A snapshot superseded live writes held for its collection",
		"gitTarget", target.String(), "scope", req.Scope.String(), "superseded", superseded,
		"stillHeld", len(kept))
	if len(kept) == 0 {
		delete(l.held, target)
	}
}

// supersededBy reports whether an applied resync's snapshot supersedes a held event: the event was
// held before the snapshot was taken off the queue, is of its collection, and is not a DELETE of an
// object the snapshot does not hold (inSnapshot, by objectKey).
func supersededBy(req *ResyncRequest, h heldEvent, inSnapshot map[string]bool) bool {
	if h.seq > req.heldUpTo || !snapshotCovers(req, h.event) {
		return false
	}
	return h.event.Operation != string(itypes.OperationDelete) || inSnapshot[objectKey(h.event.Identifier)]
}

// snapshotCovers reports whether a resync's snapshot is of the collection an event came from: the
// stream's collection, selector included, when the event names it, and its boundary otherwise.
func snapshotCovers(req *ResyncRequest, event Event) bool {
	if req.Scope == nil {
		return true
	}
	if event.SourceCollection != (itypes.CollectionKey{}) {
		return event.SourceCollection == req.Scope.Collection
	}
	return req.Scope.Matches(event.Identifier)
}

// takeHeld removes a GitTarget's held events from the hold, and returns them.
func (l *branchWorkerEventLoop) takeHeld(target itypes.ResourceReference) *heldWrites {
	held := l.held[target]
	if held == nil {
		return nil
	}
	delete(l.held, target)
	for _, h := range held.events {
		l.heldEventBytes -= h.charge
	}
	return held
}

// heldTargets is the GitTargets whose events are held, in a stable order.
func (l *branchWorkerEventLoop) heldTargets() []itypes.ResourceReference {
	targets := make([]itypes.ResourceReference, 0, len(l.held))
	for target := range l.held {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].String() < targets[j].String() })
	return targets
}

// dropHeldAtShutdown says what an exiting loop leaves held: nothing replays it.
func (l *branchWorkerEventLoop) dropHeldAtShutdown() {
	for _, target := range l.heldTargets() {
		held := l.takeHeld(target)
		l.w.Log.Info("Shutting down with live writes held for a GitTarget whose render fidelity is pending; "+
			"they are missing from Git until the objects change again or the collections replay",
			"gitTarget", target.String(), "writes", len(held.events))
	}
}

func (l *branchWorkerEventLoop) holdTimerC() <-chan time.Time {
	if l.holdTimer == nil {
		return nil
	}
	return l.holdTimer.C
}

func (l *branchWorkerEventLoop) stopHoldTimer() {
	if l.holdTimer != nil {
		l.holdTimer.Stop()
		l.holdTimer = nil
	}
}
