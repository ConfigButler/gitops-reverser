// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The write boundary's census: one increment per document the writer decided about, by resource
// type and outcome.
//
// Accumulate on the batch; publish once, after a successful flush, under the batch's GitTarget.
// Publishing at the decision site would count writes a later precondition failure discarded, and
// reading identity off the event would leave the resync path unlabelled.
//
// The rationale for this shape is docs/design/metrics-observability-plan.md §2.8.
const (
	// documentWritten is a document created or updated in the worktree.
	documentWritten = "written"
	// documentUnchanged is a document the writer diffed to a genuine no-op.
	documentUnchanged = "unchanged"
	// documentDeletedLive is a document removed because a watch DELETE said the object is gone.
	documentDeletedLive = "deleted_live"
	// documentDeletedSweep is a document removed by a mark-and-sweep resync, which is the path
	// that reconciles a deletion nobody was watching for.
	documentDeletedSweep = "deleted_sweep"
	// documentRetained is a document a mark-and-sweep would have deleted and spec.prune.mode kept.
	// It is the only trace a suppressed drop leaves, and it is configured behaviour, not a fault.
	documentRetained = "retained"
	// documentRefused is a resource the writer declined to place. Distinct from unchanged: one is
	// absent from the mirror, the other is in it and identical. placement_refusals_total carries
	// the reason.
	documentRefused = "refused"
)

// documentKey is one census cell. It holds the TYPE, never the object: the exported labels are
// group/version/resource, so keying by object identity would build one map entry per document for
// a series they all share.
type documentKey struct {
	gvr     schema.GroupVersionResource
	outcome string
}

// documentTypeOf reduces a resource identity to the type the census publishes.
func documentTypeOf(id types.ResourceIdentifier) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: id.Group, Version: id.Version, Resource: id.Resource}
}

// tallyDocument records one document outcome against the batch.
func (wb *writeBatch) tallyDocument(id types.ResourceIdentifier, outcome string) {
	wb.tallyDocumentCount(documentTypeOf(id), outcome, 1)
}

// tallyDocumentCount records n documents of one type under one outcome.
func (wb *writeBatch) tallyDocumentCount(gvr schema.GroupVersionResource, outcome string, n int) {
	if n <= 0 {
		return
	}
	if wb.documents == nil {
		wb.documents = map[documentKey]int64{}
	}
	wb.documents[documentKey{gvr: gvr, outcome: outcome}] += int64(n)
}

// publishDocumentTally emits the batch's census. Called by flush, after the bytes are on disk, and
// clears the tally so a second call cannot double-count.
func (wb *writeBatch) publishDocumentTally(ctx context.Context) {
	if telemetry.GitDocumentsTotal == nil || len(wb.documents) == 0 {
		return
	}
	for key, count := range wb.documents {
		attrs := append(wb.target.attrs(),
			attribute.String("group", key.gvr.Group),
			attribute.String("version", key.gvr.Version),
			attribute.String("resource", key.gvr.Resource),
			attribute.String("outcome", key.outcome),
		)
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
