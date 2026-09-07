// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// This is the write boundary's census: one increment per DOCUMENT the writer decided about.
//
// It replaced three counters that measured something else. objects_written_total (and its exact
// twin git_operations_total) incremented by the number of INPUT EVENTS in a flush, so a flush of
// one event that rewrote six files counted one, and an event that changed nothing counted the same
// as an event that wrote. resync_sweep_deletes_total covered only the sweep, so a steady-state
// watch delete left no trace at all; prune_retained_documents_total was the retention twin of the
// sweep counter under a third name.
//
// One counter with one bounded outcome instead, so the writer's whole population partitions and a
// dashboard can read it as one stacked series.
const (
	// documentWritten is a document created or updated in the working tree.
	documentWritten = "written"
	// documentUnchanged is a document the writer diffed to a no-op. Expected in volume: it is
	// where /status churn that survives the ingest filter comes to rest.
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
)

// recordDocument counts one document outcome, labelled by the GitTarget it belongs to and the type
// it is. Both come off the event, which is the only place they are both known.
//
// Events built by tests and by the CLI carry no GitTarget identity, and then those labels are empty
// rather than absent: an unlabelled series is still a truthful count, and inventing a placeholder
// would make one look like a real target.
func recordDocument(ctx context.Context, event Event, outcome string) {
	if telemetry.GitDocumentsTotal == nil {
		return
	}
	// The identity is read off the event directly rather than through placementTargetForEvents,
	// which would allocate a one-element slice per document. This runs once per document in a
	// resync, which for a wildcard target is thousands per pass.
	recordDocumentCount(ctx,
		placementTarget{namespace: event.GitTargetNamespace, name: event.GitTargetName},
		event, outcome, 1)
}

// recordDocumentCount counts n documents under one outcome, for the paths that decide in bulk.
func recordDocumentCount(ctx context.Context, target placementTarget, event Event, outcome string, n int64) {
	if telemetry.GitDocumentsTotal == nil || n == 0 {
		return
	}
	attrs := append(target.attrs(), resourceAttrs(event.Identifier)...)
	attrs = append(attrs, attribute.String("outcome", outcome))
	telemetry.GitDocumentsTotal.Add(ctx, n, metric.WithAttributes(attrs...))
}

// documentOutcomeForUpsert maps what an upsert did to the bytes onto the census vocabulary.
func documentOutcomeForUpsert(outcome upsertOutcome) string {
	if wroteBytes(outcome) {
		return documentWritten
	}
	// upsertSkippedUnsafe included: a placement refusal wrote nothing, and it is counted as a
	// refusal by placement_refusals_total, which is where "not in the mirror" belongs. Counting it
	// as a document here would make a skipped Secret look like a mirrored one.
	return documentUnchanged
}
