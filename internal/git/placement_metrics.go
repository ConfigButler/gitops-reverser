// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// placementTarget is the GitTarget a write batch belongs to, carried purely so the
// placement metrics can name it. A placement counter that cannot say WHICH target and
// WHICH type resolved how is not actionable — the fix for a fall-back to canonical is one
// `placement.byType` line on one GitTarget — and it is the objection the design doc raised
// against shipping a bare `placement_fell_back_total`. See
// docs/design/open-asks-priority.md and docs/interpreting-metrics.md.
//
// The label keys are gittarget_namespace / gittarget_name, never namespace / name: a
// Prometheus pod scrape with honor_labels=false overwrites a metric's `namespace`
// attribute with the scraped pod's own, which silently breaks every per-target selector.
// That convention is set by TargetReconcileCompletedTotal and PruneRetainedDocumentsTotal.
type placementTarget struct {
	namespace string
	name      string
}

func (t placementTarget) attrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("gittarget_namespace", t.namespace),
		attribute.String("gittarget_name", t.name),
	}
}

// placementTargetForEvents reads the GitTarget identity off a batch's events. Every event
// in one flush shares a base and therefore a GitTarget, so the first event that carries an
// identity describes the whole batch. Events built by tests and by the CLI carry none, and
// then the labels are empty rather than absent — an unlabelled series is still a truthful
// count of placements, and inventing a placeholder would make one look like a real target.
func placementTargetForEvents(events []Event) placementTarget {
	for _, ev := range events {
		if ev.GitTargetName != "" || ev.GitTargetNamespace != "" {
			return placementTarget{namespace: ev.GitTargetNamespace, name: ev.GitTargetName}
		}
	}
	return placementTarget{}
}

// resourceAttrs labels a placement by the type whose rule may be missing. group/version/
// resource is exactly the shape of a placement.byType key (PlacementTypeKey), so a
// `source="canonical"` series reads directly as the line the GitTarget needs. The resource
// NAME is deliberately not a label: it is unbounded, and it is in the log line.
func resourceAttrs(id types.ResourceIdentifier) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("group", id.Group),
		attribute.String("version", id.Version),
		attribute.String("resource", id.Resource),
	}
}

const (
	// placementDispositionNewFile is a placement that wrote a file of its own.
	placementDispositionNewFile = "new_file"
	// placementDispositionAppended is a placement that added a document to a file that
	// already held one — only ever reachable through a declared bundling template now that
	// sibling inference is gone, which is what makes the two dispositions worth splitting:
	// `disposition="appended"` with a source other than `by_type` or `default` should not exist.
	placementDispositionAppended = "appended"
)

// recordPlacement counts one new document actually written at a resolved path. It is
// recorded after the write, not at resolution: a refused or skipped resource is counted by
// recordPlacementRefusal instead, so the two counters partition the population — every new
// resource is mirrored (here) or not mirrored (there), and never both.
func recordPlacement(
	ctx context.Context,
	target placementTarget,
	id types.ResourceIdentifier,
	source manifestanalyzer.PlacementSource,
	appended bool,
) {
	if telemetry.PlacementsTotal == nil {
		return
	}
	disposition := placementDispositionNewFile
	if appended {
		disposition = placementDispositionAppended
	}
	attrs := append(target.attrs(), resourceAttrs(id)...)
	attrs = append(attrs,
		attribute.String("source", string(source)),
		attribute.String("disposition", disposition),
	)
	telemetry.PlacementsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// recordPlacementRefusal counts one resource the writer declined to place. Every increment
// is a resource absent from the mirror, which is why it is a counter of its own rather than
// a `source` value: a dashboard that added the two together would report a skipped Secret
// as a successful placement.
func recordPlacementRefusal(
	ctx context.Context,
	target placementTarget,
	id types.ResourceIdentifier,
	reason manifestanalyzer.PlacementRefusalReason,
) {
	if telemetry.PlacementRefusalsTotal == nil {
		return
	}
	attrs := append(target.attrs(), resourceAttrs(id)...)
	attrs = append(attrs, attribute.String("reason", string(reason)))
	telemetry.PlacementRefusalsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

const (
	kustomizationEntryAdded    = "added"
	kustomizationEntryNoChange = "no_change"
	kustomizationEntryFailed   = "failed"
)

// recordKustomizationEntry counts one attempt to register a newly placed file in the
// resources: list of the kustomization that governs it. `failed` is the outcome worth
// alerting on and the one with no other trace: the document is committed and the entry is
// not, so kustomize never builds the file — the object is in Git, looks mirrored, and is
// applied by nothing.
//
// It carries no path label on purpose: a file path is unbounded, and the log line at the
// site already names the kustomization, the entry and the resource.
func recordKustomizationEntry(ctx context.Context, target placementTarget, outcome string) {
	if telemetry.PlacementKustomizationEntriesTotal == nil {
		return
	}
	attrs := append(target.attrs(), attribute.String("outcome", outcome))
	telemetry.PlacementKustomizationEntriesTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// placementRefusalReason classifies a LocateNew error for the refusal counter. A refusal
// the analyzer raised carries its own bounded reason; anything else is an error shape that
// did not exist when this was written, and it is counted as `unclassified` rather than
// dropped — a refusal missing from the metric is worse than one with a vague label.
func placementRefusalReason(err error) manifestanalyzer.PlacementRefusalReason {
	var refusal *manifestanalyzer.PlacementRefusedError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return "unclassified"
}
