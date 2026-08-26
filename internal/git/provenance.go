// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strconv"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Provenance names the target-watch cell that produced a queued work item, and which
// incarnation of that cell produced it.
//
// # Why a queued item has to carry it
//
// Once an item is on the branch worker's FIFO the watch manager cannot withdraw it. A
// cancelled stream's goroutine may still be in flight with a live event or a snapshot, and the
// worker has nothing to judge such an item against unless the item says where it came from.
// Provenance is that statement: the cell is the slice of the mirror the item speaks for, and
// the lease is the incarnation of the stream that spoke.
//
// The GitTarget is deliberately NOT repeated here. Every item that carries provenance already
// names its target (an Event's GitTargetName/GitTargetNamespace, a ResyncRequest's), and a
// second copy is a second answer waiting to disagree — the coalescing fence already had to
// pick between a request-level and an event-level target, and reading the wrong one made it
// silently never fire.
//
// # What it is used for today
//
// Diagnostics: which cell produced a write, and which incarnation, in logs and in the storm
// signature a saturated queue leaves behind. Lease ENFORCEMENT — dropping an item whose lease
// is no longer current for its cell — comes with per-cell leasing in
// docs/design/target-watch-plan.md §5, and is the reason the field exists now: it cannot be
// added to items already in flight after the fact.
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
