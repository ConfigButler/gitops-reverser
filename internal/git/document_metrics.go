// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The write boundary's census: one increment per DOCUMENT that actually reached the worktree.
//
// It replaced three counters that measured something else. objects_written_total (and its exact
// twin git_operations_total) incremented by the number of INPUT EVENTS in a flush, so a flush of
// one event that rewrote six files counted one, and an event that changed nothing counted the same
// as an event that wrote. resync_sweep_deletes_total covered only the sweep, so a steady-state
// watch delete left no trace; prune_retained_documents_total was the sweep counter's twin under a
// third name.
//
// Two properties this deliberately has, both learned from getting them wrong first:
//
//   - It is TALLIED on the batch and published by flush, never at the decision site. A resync
//     applies every document into buffers and can then abort on a precondition, writing nothing;
//     counting at the decision produced positive `written` for a flush that never touched the
//     worktree.
//   - The labels come from the BATCH's GitTarget, not from the event. The resync path builds its
//     events without target fields, so reading identity off the event filed every production
//     snapshot write and every sweep delete under empty labels — which is worse than no labels,
//     because it looks like a real series.
const (
	// documentWritten is a document created or updated in the worktree.
	documentWritten = "written"
	// documentUnchanged is a document the writer diffed to a genuine no-op: it was considered and
	// the bytes did not move. NOT a refusal — see documentRefused.
	documentUnchanged = "unchanged"
	// documentDeletedLive is a document removed because a watch DELETE said the object is gone.
	documentDeletedLive = "deleted_live"
	// documentDeletedSweep is a document removed by a mark-and-sweep resync, which is the path
	// that reconciles a deletion nobody was watching for.
	documentDeletedSweep = "deleted_sweep"
	// documentRetained is a document a mark-and-sweep would have deleted and spec.prune.mode kept.
	// It is the only numeric trace a suppressed drop leaves: no plan action, no commit, no
	// ResyncStats entry. Non-zero is configured behaviour, never a fault.
	documentRetained = "retained"
	// documentRefused is a resource the writer declined to place. It has its own value because
	// folding it into `unchanged` said a skipped Secret was a successful no-op — the opposite of
	// what it is. placement_refusals_total carries the reason; this keeps the census exhaustive.
	documentRefused = "refused"
)

// documentKey is one census cell: a type and what happened to it. The GitTarget is not part of the
// key because a batch writes for exactly one target.
type documentKey struct {
	id      types.ResourceIdentifier
	outcome string
}

// tallyDocument records one document outcome against the batch. Nothing is published until the
// flush succeeds.
func (wb *writeBatch) tallyDocument(id types.ResourceIdentifier, outcome string) {
	if wb.documents == nil {
		wb.documents = map[documentKey]int64{}
	}
	wb.documents[documentKey{id: id, outcome: outcome}]++
}

// tallyDocumentCount records n documents under one outcome, for the paths that decide in bulk.
func (wb *writeBatch) tallyDocumentCount(id types.ResourceIdentifier, outcome string, n int) {
	if n <= 0 {
		return
	}
	if wb.documents == nil {
		wb.documents = map[documentKey]int64{}
	}
	wb.documents[documentKey{id: id, outcome: outcome}] += int64(n)
}

// publishDocumentTally emits the batch's census once its writes are on disk.
//
// Called only from flush, and only after the last buffer has been written or removed. A batch that
// aborts on a precondition publishes nothing, which is correct: nothing reached the worktree.
func (wb *writeBatch) publishDocumentTally(ctx context.Context) {
	if telemetry.GitDocumentsTotal == nil || len(wb.documents) == 0 {
		return
	}
	for key, count := range wb.documents {
		attrs := append(wb.target.attrs(), resourceAttrs(key.id)...)
		attrs = append(attrs, attribute.String("outcome", key.outcome))
		telemetry.GitDocumentsTotal.Add(ctx, count, metric.WithAttributes(attrs...))
	}
	wb.documents = nil
}

// documentOutcomeForUpsert maps what an upsert did to the bytes onto the census vocabulary.
func documentOutcomeForUpsert(outcome upsertOutcome) string {
	switch {
	case wroteBytes(outcome):
		return documentWritten
	case outcome == upsertSkippedUnsafe:
		return documentRefused
	default:
		return documentUnchanged
	}
}
