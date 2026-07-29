// SPDX-License-Identifier: Apache-2.0

/*
Package telemetry provides the OpenTelemetry-based metrics exporter for GitOps Reverser.
It configures Prometheus-compatible metrics collection for monitoring controller operations.

Every instrument declared here MUST have at least one production recording site. A metric
that is defined but never recorded is a contract the code does not honor; document it in
docs/interpreting-metrics.md only once it actually emits.
*/
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	otelMeter metric.Meter

	// GitOperationsTotal counts git operations performed by branch workers.
	GitOperationsTotal metric.Int64Counter
	// ObjectsWrittenTotal counts objects that resulted in file writes.
	ObjectsWrittenTotal metric.Int64Counter
	// CommitsTotal counts commit batches pushed to git, labelled by the recording
	// BranchWorker's {provider_namespace, provider_name, branch, author_kind} identity.
	// Both the per-event and backfill-resync commit paths feed this one counter.
	CommitsTotal metric.Int64Counter
	// ResyncSweepDeletesTotal counts managed documents deleted by mark-and-sweep
	// resyncs, labelled by the swept resource {group, version, resource}.
	ResyncSweepDeletesTotal metric.Int64Counter
	// PruneRetainedDocumentsTotal counts managed documents a GitTarget's spec.prune.mode
	// KEPT that a mark-and-sweep would otherwise have deleted, labelled by
	// {prune_mode, gittarget_namespace, gittarget_name}. It is the retention twin of
	// ResyncSweepDeletesTotal and the only numeric trace a suppressed drop leaves: such a
	// drop produces no plan action, no commit, and no ResyncStats entry. A non-zero value
	// is the configured behaviour, never a fault.
	PruneRetainedDocumentsTotal metric.Int64Counter

	// TargetReconcileCompletedTotal counts completed watch recovery passes per
	// GitTarget: each increment marks either a streaming-snapshot resync applied on
	// the branch worker or a cursor-backed watch resume (see Manager.recordTargetReconcileCompleted).
	// Labelled by {gittarget_namespace,
	// gittarget_name, trigger} where trigger is `rule_change` (the GVR/rule reconcile
	// path). A counter, not a
	// latched gauge, on purpose: a counter resets to 0 on a fresh pod, so a
	// per-pod `{pod="<new>"} > 0` check after a rollout proves the new pod did
	// its own reconcile — robust to the old pod's stale series that a Prometheus
	// pod scrape may still be holding during the rollout, which a latched gauge
	// (or a cross-pod sum-over-baseline) cannot distinguish.
	// The label keys avoid the reserved `namespace`/`name`: a pod scrape with
	// honor_labels=false would overwrite a metric's `namespace` attribute with the
	// scraped pod's own namespace, making a per-GitTarget `namespace` selector
	// silently match nothing. Load-bearing for the restart-reconcile e2e spec and
	// useful long-term for spotting excessive reconciles via increase(...[5m]);
	// treat the name/labels as a public observability contract.
	TargetReconcileCompletedTotal metric.Int64Counter
	// BranchWorkerQueueDepth gauges pending work for a single branch worker:
	// accepted-but-not-yet-handled items (queued or actively being processed)
	// plus any committed-but-not-yet-pushed work the worker is still holding. It
	// reads 0 only when the worker has fully drained (every accepted item handled
	// and nothing retained for replay), so it never reports drained while a
	// commit/push is still in flight. Labelled by {provider_namespace,
	// provider_name, branch}; the namespace/name keys are prefixed to avoid the
	// reserved Prometheus pod-scrape target labels (see
	// TargetReconcileCompletedTotal). Load-bearing for the restart-reconcile e2e
	// spec's drain wait; treat the name/labels as a public observability contract.
	BranchWorkerQueueDepth metric.Int64Gauge

	// ResyncBackgroundFailuresTotal counts rule-change resyncs whose apply failed or
	// timed out at the worker AFTER being enqueued. Delivery is marked on enqueue (the
	// resync is fire-and-forget to avoid an unbounded re-gather loop — see
	// Manager.recordTargetReconcileCompleted), so a failed background apply is otherwise
	// only logged. This counter makes those failures observable/alertable without
	// triggering an immediate re-gather. Labelled by {gittarget_namespace,
	// gittarget_name}; a sustained increase means snapshots are not committing and the
	// folder is relying on steady-state events to catch up.
	ResyncBackgroundFailuresTotal metric.Int64Counter

	// AuditEventsTotal is the single per-event census: every successfully decoded, converted, and
	// validated audit event increments it exactly once, labelled by {outcome, category, group,
	// version, resource, verb}. Audit is attribution-only — it names the author of a watch-observed
	// change; it never carries object state. Liveness = sum(...) > 0; the e2e invariant gates on
	// category="error" == 0.
	AuditEventsTotal metric.Int64Counter
	// AuditEventListsTotal counts inbound audit EventList requests at the webhook boundary,
	// labelled by bounded outcome (processed/empty/decode_error/process_error).
	AuditEventListsTotal metric.Int64Counter
	// AuditEventListEventsTotal counts decoded audit event items delivered in EventLists,
	// labelled by the same bounded outcome.
	AuditEventListEventsTotal metric.Int64Counter
	// AuditEventListDurationSeconds records how long the webhook takes to answer an
	// EventList request, labelled by outcome.
	AuditEventListDurationSeconds metric.Float64Histogram
	// AttributionResolutionsTotal counts watch-event attribution resolver outcomes, labelled by
	// {tier, actor_kind, group, version, resource}. tier names WHICH evidence answered
	// (sticky_delete/exact/collection_uid_delete/latest/name/collection_scope_delete/resource_version/
	// absent) and actor_kind
	// names WHO it named (user/serviceaccount/none) — two orthogonal questions, so they are two
	// labels. Match coverage is tier!="absent"; anything narrower reads the collection and name
	// tiers as misses.
	AttributionResolutionsTotal metric.Int64Counter
	// AttributionFactsTotal counts attribution fact lifecycle events, labelled by bounded op:
	// "written" is one fact appended to the fact log, "matched" is one joined by a watch event.
	// Together they say how much of what is published is ever used. They are NOT subtractable:
	// written counts every type, matched only the streams this process follows.
	AttributionFactsTotal metric.Int64Counter
	// AttributionResolutionWaitSeconds records resolver wait time by {tier, event_kind, group,
	// version, resource}. event_kind is write or removal, and the split is load-bearing: a removal
	// holds a fallback and keeps waiting where a write does not, so the removal wait is the number
	// --author-attribution-grace is tuned from.
	AttributionResolutionWaitSeconds metric.Float64Histogram
	// AttributionFactIndexEntries gauges the entries the in-memory fact index currently holds across
	// every scope and match structure. Read against the eviction counter it says whether the caps
	// are binding.
	AttributionFactIndexEntries metric.Int64Gauge
	// AttributionFactIndexEvictionsTotal counts facts dropped from the in-memory fact index because
	// it was full, labelled by bounded reason (per_type/total). An attribution lost to a full index
	// has to look different from one that was never published, or a burst is silently absorbed.
	AttributionFactIndexEvictionsTotal metric.Int64Counter
	// AttributionCollectionWithoutUIDSetTotal counts collection facts published without the uid set
	// the precise join would have used, labelled by bounded reason (uid_cap/no_uids). The scope
	// fallback is already correct, so this says how often the precise path was available — not that
	// anything broke.
	AttributionCollectionWithoutUIDSetTotal metric.Int64Counter
	// AttributionFactStreamGapsTotal counts occasions a fact stream was trimmed past this process's
	// follower, labelled by stream. Every gap is facts lost for good, and it is the one loss a log
	// transport can see at all.
	AttributionFactStreamGapsTotal metric.Int64Counter
	// AttributionFactStreamDecodeErrorsTotal counts fact-stream entries the follower could not
	// decode, labelled by transport. Such an entry is skipped and its position passed, so the facts
	// it carried are lost — and unlike a trim gap the loss leaves no other trace, which is why this
	// is the loss path that most needed a counter.
	AttributionFactStreamDecodeErrorsTotal metric.Int64Counter
	// AttributionFactFollowerErrorsTotal counts fact-follower read failures, labelled by transport.
	// The follower retries with a backoff rather than returning, so the errors are otherwise only a
	// log line.
	AttributionFactFollowerErrorsTotal metric.Int64Counter
	// AttributionFactFollowerLastSuccessTimestampSeconds gauges the Unix time of the follower's last
	// successful read, idle rounds included. It matters more than the error counter: only it
	// separates "erroring occasionally while making progress" from "has read nothing in ten
	// minutes", and only the second is an outage. Read it as time() - <gauge>.
	AttributionFactFollowerLastSuccessTimestampSeconds metric.Int64Gauge
	// AttributionTransportInfo is an info gauge, always 1, labelled by the fact transport in force
	// (redis/memory). It is interpretive metadata rather than a signal: a burst of unresolved
	// commits after a restart is EXPECTED under the in-process transport, which drops every fact
	// with the process, and a bug under Redis.
	AttributionTransportInfo metric.Int64Gauge

	// APICatalogResources gauges the count of served top-level resources in the catalog,
	// split by the default-watch-policy allowed/excluded state.
	APICatalogResources metric.Int64Gauge
	// APICatalogGroupVersions gauges discovered group/versions, split into trusted vs degraded.
	APICatalogGroupVersions metric.Int64Gauge
	// APICatalogRefreshTotal counts API resource catalog refreshes by outcome.
	APICatalogRefreshTotal metric.Int64Counter
	// APICatalogRefreshDurationSeconds records the wall time of one catalog refresh.
	APICatalogRefreshDurationSeconds metric.Float64Histogram
	// APICatalogGeneration gauges the current APIResourceCatalog generation.
	APICatalogGeneration metric.Int64Gauge
	// WatchedTypes gauges the number of watched types per GitTarget, labelled by
	// gittarget_namespace and gittarget_name.
	WatchedTypes metric.Int64Gauge

	// SecretEncryptionAttemptsTotal counts total Secret encryption attempts.
	SecretEncryptionAttemptsTotal metric.Int64Counter
	// SecretEncryptionSuccessTotal counts successful Secret encryptions.
	SecretEncryptionSuccessTotal metric.Int64Counter
	// SecretEncryptionFailuresTotal counts failed Secret encryptions.
	SecretEncryptionFailuresTotal metric.Int64Counter
	// SecretEncryptionCacheHitsTotal counts cache hits for encrypted Secret content.
	SecretEncryptionCacheHitsTotal metric.Int64Counter
	// SecretEncryptionMarkerSkipsTotal counts marker-based skips that reused cached Secret content.
	SecretEncryptionMarkerSkipsTotal metric.Int64Counter
)

// InitOTLPExporter initializes the OTLP-to-Prometheus bridge.
func InitOTLPExporter(_ context.Context) (func(context.Context) error, error) {
	fmt.Println("Initializing OTLP exporter")

	// Create a Prometheus exporter that bridges OTLP metrics to Prometheus
	// Configure it to use the controller-runtime registry.
	exporter, err := prometheus.New(
		prometheus.WithRegisterer(metrics.Registry),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus exporter: %w", err)
	}

	// Create a meter provider with the Prometheus exporter.
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(provider)

	// Get the meter from the new provider.
	otelMeter = provider.Meter("gitops-reverser")

	if err := registerInstruments(); err != nil {
		return nil, err
	}

	return func(_ context.Context) error {
		fmt.Println("Shutting down OTLP exporter")
		return nil
	}, nil
}

// InitTestExporter wires the global instruments to a meter provider backed by a
// manual reader, so unit tests can collect and assert recorded metric values.
// It returns the reader to collect from.
func InitTestExporter() (*sdkmetric.ManualReader, error) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	otelMeter = provider.Meter("gitops-reverser")
	if err := registerInstruments(); err != nil {
		return nil, err
	}
	return reader, nil
}

// Instrument registration spec types. Each pairs a metric name with the
// package-level variable that receives the created instrument.
type (
	cSpec struct {
		name string
		dest *metric.Int64Counter
	}
	hSpec struct {
		name    string
		dest    *metric.Float64Histogram
		buckets []float64
	}
	gSpec struct {
		name string
		dest *metric.Int64Gauge
	}
)

// registerInstruments creates every metric instrument against the current
// otelMeter and stores it in its package-level variable, one kind at a time.
func registerInstruments() error {
	if err := registerCounters(); err != nil {
		return err
	}
	if err := registerHistograms(); err != nil {
		return err
	}
	return registerGauges()
}

func registerCounters() error {
	counters := []cSpec{
		{"gitopsreverser_git_operations_total", &GitOperationsTotal},
		{"gitopsreverser_objects_written_total", &ObjectsWrittenTotal},
		{"gitopsreverser_commits_total", &CommitsTotal},
		{"gitopsreverser_resync_sweep_deletes_total", &ResyncSweepDeletesTotal},
		{"gitopsreverser_prune_retained_documents_total", &PruneRetainedDocumentsTotal},
		{"gitopsreverser_target_reconcile_completed_total", &TargetReconcileCompletedTotal},
		{"gitopsreverser_resync_background_failures_total", &ResyncBackgroundFailuresTotal},
		{"gitopsreverser_audit_events_total", &AuditEventsTotal},
		{"gitopsreverser_audit_eventlists_total", &AuditEventListsTotal},
		{"gitopsreverser_audit_eventlist_events_total", &AuditEventListEventsTotal},
		{"gitopsreverser_attribution_resolutions_total", &AttributionResolutionsTotal},
		{"gitopsreverser_attribution_facts_total", &AttributionFactsTotal},
		{"gitopsreverser_attribution_fact_index_evictions_total", &AttributionFactIndexEvictionsTotal},
		{"gitopsreverser_attribution_fact_stream_gaps_total", &AttributionFactStreamGapsTotal},
		{
			"gitopsreverser_attribution_collection_without_uidset_total",
			&AttributionCollectionWithoutUIDSetTotal,
		},
		{
			"gitopsreverser_attribution_fact_stream_decode_errors_total",
			&AttributionFactStreamDecodeErrorsTotal,
		},
		{"gitopsreverser_attribution_fact_follower_errors_total", &AttributionFactFollowerErrorsTotal},
		{"gitopsreverser_api_catalog_refresh_total", &APICatalogRefreshTotal},
		{"gitopsreverser_secret_encryption_attempts_total", &SecretEncryptionAttemptsTotal},
		{"gitopsreverser_secret_encryption_success_total", &SecretEncryptionSuccessTotal},
		{"gitopsreverser_secret_encryption_failures_total", &SecretEncryptionFailuresTotal},
		{"gitopsreverser_secret_encryption_cache_hits_total", &SecretEncryptionCacheHitsTotal},
		{"gitopsreverser_secret_encryption_marker_skips_total", &SecretEncryptionMarkerSkipsTotal},
	}
	for _, s := range counters {
		v, err := otelMeter.Int64Counter(s.name)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return nil
}

func registerHistograms() error {
	// eventListDurationBuckets span the webhook's EventList answer time: sub-millisecond decode
	// up through a slow request, plus headroom for an attribution lookup wait.
	eventListDurationBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 30, 300}
	// catalogRefreshBuckets span discovery latency: two cached GETs on an aggregated
	// apiserver (sub-second) up to a slow per-group fallback (seconds).
	catalogRefreshBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	// attributionWaitBuckets span zero-wait hits up through the default grace window
	// and slower configured waits.
	attributionWaitBuckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10}
	hists := []hSpec{
		{"gitopsreverser_audit_eventlist_duration_seconds", &AuditEventListDurationSeconds, eventListDurationBuckets},
		{
			"gitopsreverser_attribution_resolution_wait_seconds",
			&AttributionResolutionWaitSeconds,
			attributionWaitBuckets,
		},
		{
			"gitopsreverser_api_catalog_refresh_duration_seconds",
			&APICatalogRefreshDurationSeconds,
			catalogRefreshBuckets,
		},
	}
	for _, s := range hists {
		opts := []metric.Float64HistogramOption{}
		if len(s.buckets) > 0 {
			opts = append(opts, metric.WithExplicitBucketBoundaries(s.buckets...))
		}
		v, err := otelMeter.Float64Histogram(s.name, opts...)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return nil
}

func registerGauges() error {
	gauges := []gSpec{
		{"gitopsreverser_api_catalog_resources", &APICatalogResources},
		{"gitopsreverser_api_catalog_group_versions", &APICatalogGroupVersions},
		{"gitopsreverser_api_catalog_generation", &APICatalogGeneration},
		{"gitopsreverser_watched_types", &WatchedTypes},
		{"gitopsreverser_branch_worker_queue_depth", &BranchWorkerQueueDepth},
		{"gitopsreverser_attribution_fact_index_entries", &AttributionFactIndexEntries},
		{
			"gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds",
			&AttributionFactFollowerLastSuccessTimestampSeconds,
		},
		{"gitopsreverser_attribution_transport_info", &AttributionTransportInfo},
	}
	for _, s := range gauges {
		v, err := otelMeter.Int64Gauge(s.name)
		if err != nil {
			return err
		}
		*s.dest = v
	}
	return nil
}
