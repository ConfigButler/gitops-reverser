// SPDX-License-Identifier: Apache-2.0

package git

import (
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The source cell on a queued work item names the target-watch cell that produced it.
//
// # It is diagnostic, and deliberately stays that way
//
// Once an item is on the branch worker's FIFO it WILL be applied. Nothing filters the queue on
// the source cell, and nothing should: a branch worker serves one GitProvider and branch rather
// than one GitTarget, so its queue is shared across tenants, and judging an item would mean
// reaching back into the watch plan of whichever GitTarget the item names, per item, per
// tenant. The plan is applied by starting and canceling streams instead, at the producer.
//
// The consequence is accepted: a canceled stream's goroutine can still be in flight, so a
// configuration change may be followed by a short tail of writes from a cell that is no longer
// selected. That tail is bounded by the queue, the files it touches are retained anyway, and
// the Git-side sweep converges the mirror afterwards. See
// docs/design/target-watch-plan.md, "Cut at the producer".
//
// An earlier design stamped a stream LEASE beside the cell, so a consumer could tell a canceled
// stream's in-flight item from its replacement's. There is no such consumer and there will not
// be one, so the lease is gone. If a fence is ever genuinely needed it belongs on the producer,
// as a refusal to enqueue once the stream's context is canceled. Document the specific failure
// first.
//
// The GitTarget is deliberately NOT repeated in it. Every item that carries a source cell
// already names its target (an Event's GitTargetName/GitTargetNamespace, a ResyncRequest's),
// and a second copy is a second answer waiting to disagree: the coalescing fence already had to
// pick between a request-level and an event-level target, and reading the wrong one made it
// silently never fire.
//
// The zero cell means "no cell claimed this": a reconcile, a bootstrap, or any control-plane
// path that speaks for the whole GitTarget rather than for one watched slice. An unclaimed item
// is not suspicious, it is how every non-stream producer queues work.

// sourceCellForLog renders a source cell for logs: "configmaps in team-a", or "unclaimed" for
// the zero cell. Reading a storm means knowing which cell produced a write, and the drop
// records a saturated queue leaves behind are where that is read from.
func sourceCellForLog(c types.CellKey) string {
	if c == (types.CellKey{}) {
		return "unclaimed"
	}
	return c.String()
}

// sourceCell is the write request's source cell: the one its events agree on, or the zero cell
// when they do not. The live path wraps exactly one event, which is the path that carries a
// cell; an atomic reconcile carries many events and speaks for no single cell, so it reads
// unclaimed, which is what it is.
func (r *WriteRequest) sourceCell() types.CellKey {
	if r == nil || len(r.Events) == 0 {
		return types.CellKey{}
	}
	first := r.Events[0].SourceCell
	for i := range r.Events {
		if r.Events[i].SourceCell != first {
			return types.CellKey{}
		}
	}
	return first
}
