// SPDX-License-Identifier: Apache-2.0

// Package outcome defines the single, bounded vocabulary for what the audit
// ingestion pipeline did with one event — its "outcome" — and records it on the
// gitopsreverser_audit_events_total counter. Every successfully decoded,
// converted, and validated audit event ends in exactly one Outcome, recorded
// once by whichever layer terminates it (the webhook handler for pre-queue
// outcomes, the per-type queue for queue outcomes). The Git-materialization fate
// (read-time tail/splice skips) is deliberately NOT an Outcome — those are
// repeatable per (GitTarget, GVR) and would multi-count the same stored entry.
// See docs/architecture.md.
package outcome

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// Category is the coarse grouping of an Outcome. It is fully derivable from the
// Outcome (see Outcome.Category) and is carried as its own metric label only so an
// alert can say category="error" without hardcoding every outcome name, and so a
// future error outcome is covered automatically.
type Category string

const (
	// Stored — the event was written to the per-type Redis log.
	Stored Category = "stored"
	// Held — the event is kept for later (a parked additional body).
	Held Category = "held"
	// Dropped — the event did not reach the log; the "Recovered by" column in the
	// plan says whether that is safe (genuinely not wanted vs. recovered by the
	// next checkpoint).
	Dropped Category = "dropped"
	// Error — should never happen; the e2e invariant gates on this category == 0.
	Error Category = "error"
)

// Outcome is the terminal ingestion fate of one audit event. Values are the
// frozen, snake_case metric label values.
type Outcome string

const (
	// Queued — written to the type stream (numeric in-order, or RV-less pinned to
	// the high-water). A merged event (an additional body merged into the official)
	// is also just Queued — the merge is provenance, not a different fate.
	Queued Outcome = "queued"

	// Parked — an additional body kept until its official event arrives.
	Parked Outcome = "parked"

	// NotNeeded — the type is not currently claimed ∩ followable (demand gate).
	NotNeeded Outcome = "not_needed"
	// NilEvent — no event (defensive).
	NilEvent Outcome = "nil_event"
	// Stage — not the ResponseComplete stage.
	Stage Outcome = "stage"
	// ReadOnlyOrUnknownVerb — get/list/watch (or an unmapped verb) — no mutation.
	ReadOnlyOrUnknownVerb Outcome = "read_only_or_unknown_verb"
	// FailedRequest — the mutation never reached etcd (responseStatus >= 300).
	FailedRequest Outcome = "failed_request"
	// DryRun — a dry-run request; nothing was persisted.
	DryRun Outcome = "dry_run"
	// UnchangedResourceVersion — no state change.
	UnchangedResourceVersion Outcome = "unchanged_resource_version"
	// MalformedAdditional — an additional body without a usable body; the official drives.
	MalformedAdditional Outcome = "malformed_additional"
	// NonScaleSubresource — only /scale is mirrored; other subresources drop before Redis.
	NonScaleSubresource Outcome = "non_scale_subresource"
	// ShallowDropped — identity-shallow official, no body, not deletable.
	ShallowDropped Outcome = "shallow_dropped"
	// RVLessEmptyHighWater — an RV-less event before any high-water exists → no-op.
	RVLessEmptyHighWater Outcome = "rvless_empty_highwater"
	// OlderThanHighWater — RV below the stream high-water (external batch-delivery reorder).
	OlderThanHighWater Outcome = "older_than_high_water"
	// NonNumericRV — an RV that is not a uint64 (aggregated apiservers).
	NonNumericRV Outcome = "non_numeric_rv"
	// MissingClusterAnnotation — an event on the shared (annotation-routed) /audit-webhook endpoint
	// that carries no source-cluster annotation, so it names no ClusterProvider. Never credited to a
	// fallback; a rising rate means a producer is not stamping the annotation.
	MissingClusterAnnotation Outcome = "missing_cluster_annotation"

	// WriteError — a redis/enqueue failure; the event never reached the log.
	WriteError Outcome = "write_error"
)

// Category maps an Outcome to its bucket. A value not listed here is treated as an
// Error so a new, unmapped outcome trips the invariant rather than passing silently.
func (o Outcome) Category() Category {
	switch o {
	case Queued:
		return Stored
	case Parked:
		return Held
	case NotNeeded, NilEvent, Stage, ReadOnlyOrUnknownVerb, FailedRequest, DryRun,
		UnchangedResourceVersion, MalformedAdditional, NonScaleSubresource,
		ShallowDropped, RVLessEmptyHighWater, OlderThanHighWater, NonNumericRV,
		MissingClusterAnnotation:
		return Dropped
	case WriteError:
		return Error
	default:
		return Error
	}
}

// Record increments gitopsreverser_audit_events_total once for the event's
// outcome, labelled {outcome, category, group, version, resource, verb}. It is
// nil-guarded so it is a no-op when telemetry is not initialized (unit tests).
func Record(ctx context.Context, event *auditv1.Event, o Outcome) {
	if telemetry.AuditEventsTotal == nil {
		return
	}
	group, version, resource := gvrParts(event)
	telemetry.AuditEventsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", string(o)),
		attribute.String("category", string(o.Category())),
		attribute.String("group", group),
		attribute.String("version", version),
		attribute.String("resource", resource),
		attribute.String("verb", verb(event)),
	))
}

// gvrParts splits an audit event's objectRef into bounded group/version/resource
// label values, matching the long-standing audit-metric convention ("unknown"
// when the objectRef carries no usable identity).
func gvrParts(event *auditv1.Event) (string, string, string) {
	if event == nil || event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown", "unknown", "unknown"
	}
	group, version, found := strings.Cut(event.ObjectRef.APIVersion, "/")
	if !found {
		version = event.ObjectRef.APIVersion
		group = event.ObjectRef.APIGroup
	}
	resource := event.ObjectRef.Resource
	if resource == "" {
		resource = "unknown"
	}
	return group, version, resource
}

func verb(event *auditv1.Event) string {
	if event == nil {
		return ""
	}
	return event.Verb
}
