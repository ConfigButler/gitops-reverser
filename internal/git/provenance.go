// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strconv"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Provenance names the target-watch cell that produced a queued work item, and which
// incarnation of that cell produced it.
//
// # It is diagnostic, and deliberately stays that way
//
// Once an item is on the branch worker's FIFO it WILL be applied. Nothing filters the queue on
// provenance, and nothing should: a branch worker serves one GitProvider and branch rather
// than one GitTarget, so its queue is shared across tenants, and judging an item would mean
// reaching back into the watch plan of whichever GitTarget the item names, per item, per
// tenant. The plan is applied by starting and canceling streams instead, at the producer.
//
// The consequence is accepted: a canceled stream's goroutine can still be in flight, so a
// configuration change may be followed by a short tail of writes from a cell that is no longer
// selected. That tail is bounded by the queue and the files it touches are retained anyway.
// See docs/design/target-watch-plan.md, "Cut at the producer".
//
// The GitTarget is deliberately NOT repeated here. Every item that carries provenance already
// names its target (an Event's GitTargetName/GitTargetNamespace, a ResyncRequest's), and a
// second copy is a second answer waiting to disagree — the coalescing fence already had to
// pick between a request-level and an event-level target, and reading the wrong one made it
// silently never fire.
//
// # What it is for
//
// Reading a storm: which cell produced a write, in logs and in the drop records a saturated
// queue leaves behind.
//
// The Lease field is SLATED FOR REMOVAL: it was the stream incarnation a consumer-side fence
// would have judged, that fence is not being built, and this type then collapses to the cell
// it carries. See docs/design/target-watch-plan.md, "What this removes".
//
// If a fence is ever genuinely needed, it belongs on the producer, as a refusal to enqueue
// once the stream's context is canceled. Document the specific failure first.
//
// The zero value means "no cell claimed this": a reconcile, a bootstrap, or any control-plane
// path that speaks for the whole GitTarget rather than for one watched slice.
type Provenance struct {
	// Cell is the watched slice this item speaks for.
	Cell types.CellKey
	// Lease is the stream incarnation that produced it. Zero means unclaimed.
	Lease uint64
}

// Claimed reports whether a cell claimed this item. An unclaimed item is not suspicious: it is
// how every non-stream producer queues work.
func (p Provenance) Claimed() bool {
	return p.Lease != 0
}

// String renders the provenance for logs: "configmaps in team-a#7", or "unclaimed".
func (p Provenance) String() string {
	if !p.Claimed() {
		return "unclaimed"
	}
	return p.Cell.String() + "#" + strconv.FormatUint(p.Lease, 10)
}

// provenance is the write request's provenance: the one its events agree on, or the zero
// value when they do not. The live path wraps exactly one event, which is the path that
// carries a cell; an atomic reconcile carries many events and speaks for no single cell, so it
// reads unclaimed — which is what it is.
func (r *WriteRequest) provenance() Provenance {
	if r == nil || len(r.Events) == 0 {
		return Provenance{}
	}
	first := r.Events[0].Provenance
	for i := range r.Events {
		if r.Events[i].Provenance != first {
			return Provenance{}
		}
	}
	return first
}
