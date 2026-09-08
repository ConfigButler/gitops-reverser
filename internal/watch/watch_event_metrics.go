// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The ingest stage's instruments. Until these existed the first stage of the pipeline emitted
// nothing at all, so "nothing is happening", "the rules filtered everything" and "delivery is
// failing" were indistinguishable from outside — and the funnel could not be drawn at all.
//
// routeLiveTargetWatchEvent is the single honest boundary: one switch carrying every terminal
// branch an event can take. Counting there rather than at each branch's site is what makes the
// census exhaustive by construction.

// Ingest outcomes. See telemetry.WatchEventsTotal for what each class means; the short version is
// that only routeFailed is loss, and unchanged/operationFiltered/bookmark are the pipeline working.
const (
	watchOutcomeRouted            = "routed"
	watchOutcomeUnchanged         = "unchanged"
	watchOutcomeOperationFiltered = "operation_filtered"
	watchOutcomeNotObject         = "not_object"
	watchOutcomeBookmark          = "bookmark"
	watchOutcomeRouteFailed       = "route_failed"
	watchOutcomeShutdown          = "shutdown"
	watchOutcomeStreamError       = "stream_error"
)

// Watch session end reasons.
const (
	sessionEndedExpired       = "expired"
	sessionEndedError         = "error"
	sessionEndedStopped       = "stopped"
	recoveryModeCursorResume  = "cursor_resume"
	recoveryModeTypeReconcile = "type_reconcile"
	recoveryModeReplay        = "replay"
	recoveryModeListFallback  = "list_fallback"
)

// gvrAttrs is the {group, version, resource} triple every ingest instrument carries. It is the same
// shape a placement.byType key and the audit census use, so one type reads the same across the
// whole surface.
func gvrAttrs(gvr schema.GroupVersionResource) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("group", gvr.Group),
		attribute.String("version", gvr.Version),
		attribute.String("resource", gvr.Resource),
	}
}

// groupResourceAttrs is the {group, resource} pair, for the instruments whose unit of work is a
// CELL rather than a served version. CellKey carries no version, so a recovery counted from the
// per-type reconcile path genuinely does not know one; an empty `version` on that arm beside a
// populated one on the cursor-resume arm would be worse than no label at all.
func groupResourceAttrs(group, resource string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("group", group),
		attribute.String("resource", resource),
	}
}

// gitTargetAttrs is the prefixed GitTarget identity. Never bare namespace/name: a Prometheus pod
// scrape with honor_labels=false overwrites a bare `namespace` attribute with the scraped pod's own
// and silently breaks every per-target selector.
func gitTargetIdentityAttrs(gitDest types.ResourceReference) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
	}
}

// recordWatchEvent counts one delivered watch event under its terminal outcome.
func recordWatchEvent(
	ctx context.Context,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	outcome string,
) {
	if telemetry.WatchEventsTotal == nil {
		return
	}
	attrs := append(gitTargetIdentityAttrs(gitDest), gvrAttrs(gvr)...)
	attrs = append(attrs, attribute.String("outcome", outcome))
	telemetry.WatchEventsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// recordWatchEventHandling times how long the stream was busy on one event.
//
// Occupancy, not queue delay: see telemetry.WatchEventHandlingSeconds for why the wait itself
// cannot be measured honestly from this side of a client-go watch channel.
func recordWatchEventHandling(ctx context.Context, gvr schema.GroupVersionResource, started time.Time) {
	if telemetry.WatchEventHandlingSeconds == nil {
		return
	}
	telemetry.WatchEventHandlingSeconds.Record(ctx, time.Since(started).Seconds(),
		metric.WithAttributes(gvrAttrs(gvr)...))
}

// sessionEndReason classifies why a watch session ended.
//
// `expired` is the one to watch: the stored resourceVersion fell out of watch history, so the next
// session cannot resume and must replay the whole type. A restart storm is a rebuild storm, and the
// rebuild cost is what recordWatchReplayDuration then measures.
func sessionEndReason(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return sessionEndedStopped
	case errors.Is(err, errTargetWatchExpired):
		return sessionEndedExpired
	case err != nil:
		return sessionEndedError
	default:
		// A session that returns nil without cancellation ended because the plan retired it.
		return sessionEndedStopped
	}
}

// recordWatchSessionEnded counts one watch session ending, under the reason it ended for.
func recordWatchSessionEnded(ctx context.Context, gvr schema.GroupVersionResource, reason string) {
	if telemetry.WatchSessionsEndedTotal == nil {
		return
	}
	attrs := append(gvrAttrs(gvr), attribute.String("reason", reason))
	telemetry.WatchSessionsEndedTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// recordWatchReplayDuration times one initial-events replay to initial-events-end. It is what a
// 410 storm charges: the cost of every rebuild it forces.
func recordWatchReplayDuration(ctx context.Context, gvr schema.GroupVersionResource, started time.Time) {
	if telemetry.WatchReplayDurationSeconds == nil {
		return
	}
	telemetry.WatchReplayDurationSeconds.Record(ctx, time.Since(started).Seconds(),
		metric.WithAttributes(gvrAttrs(gvr)...))
}
