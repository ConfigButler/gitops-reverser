/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package telemetry provides the OpenTelemetry-based metrics exporter for GitOps Reverser.
It configures Prometheus-compatible metrics collection for monitoring controller operations.
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
	otelMeter              metric.Meter
	GitOperationsTotal     metric.Int64Counter
	GitPushDurationSeconds metric.Float64Histogram

	// ObjectsScannedTotal counts objects scanned by list/polling/informer paths.
	ObjectsScannedTotal metric.Int64Counter
	// ObjectsWrittenTotal counts objects that resulted in file writes.
	ObjectsWrittenTotal metric.Int64Counter
	// FilesDeletedTotal counts deleted files during orphan cleanup.
	FilesDeletedTotal metric.Int64Counter
	// CommitsTotal counts commit batches pushed to git.
	CommitsTotal metric.Int64Counter
	// CommitBytesTotal counts approximate bytes written across commits.
	CommitBytesTotal metric.Int64Counter
	// RebaseRetriesTotal counts retries due to non fast-forward push errors.
	RebaseRetriesTotal metric.Int64Counter
	// OwnershipConflictsTotal counts ownership conflicts (marker/lease).
	OwnershipConflictsTotal metric.Int64Counter
	// LeaseAcquireFailuresTotal counts failures to acquire/renew leases.
	LeaseAcquireFailuresTotal metric.Int64Counter
	// MarkerConflictsTotal counts repository marker conflicts.
	MarkerConflictsTotal metric.Int64Counter

	// RepoBranchActiveWorkers is a gauge for active repo-branch workers.
	RepoBranchActiveWorkers metric.Int64UpDownCounter
	// RepoBranchQueueDepth is a gauge for per-repo-branch queue depth.
	RepoBranchQueueDepth metric.Int64UpDownCounter

	// TargetReconcileCompletedTotal counts completed rule-set snapshot reconcile
	// passes per GitTarget: each increment marks one pass where the streaming-snapshot
	// resync was gathered and ENQUEUED on the branch worker (not waited on to commit —
	// see Manager.recordTargetReconcileCompleted). Labelled by {gittarget_namespace,
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

	// TypeLifecycleReconcileTotal counts M12 per-type reconciles driven by a registry
	// TypeActivated transition: each increment is one (GitTarget, type) reconcile enqueued
	// after the type settled into the followable set. Labelled by {gittarget_namespace,
	// gittarget_name}; an increase tracks types coming online (e.g. a CRD installed) being
	// mirrored without a whole-GitTarget resync.
	TypeLifecycleReconcileTotal metric.Int64Counter
	// TypeLifecycleSweepTotal counts M12 per-type sweeps driven by a registry TypeRemoved
	// transition (a type whose removal grace elapsed): each increment is one (GitTarget,
	// type) scoped sweep enqueued. Labelled by {gittarget_namespace, gittarget_name}; an
	// increase tracks types going away (e.g. a CRD deleted) having only their own documents
	// pruned.
	TypeLifecycleSweepTotal metric.Int64Counter

	// MaterializationSyncEventsTotal counts demand-axis transitions the checkpoint driver
	// handles, labelled by {kind} (SyncRequested/SyncStarted/TypeSynced/SyncFailed/Released).
	// It tracks demand-driven checkpoint activity per the materialization lifecycle
	// (docs/design/stream/demand-driven-type-materialization-lifecycle.md, L-6/L10).
	MaterializationSyncEventsTotal metric.Int64Counter
	// MaterializationCheckpointFillsTotal counts completed per-type checkpoint fills, labelled by
	// {path}: "watch" for the WATCH-first streaming-list (sendInitialEvents + the
	// initial-events-end bookmark) and "list" for the consistent-LIST fallback taken when a
	// backend does not honor streaming-list (e.g. an aggregated API that never emits the
	// bookmark). A rising "list" share is the fallback surface — which clusters/types are not on
	// the cheaper streaming path. Pair it with the per-GVR `objects-mirror: snapshot loaded ...
	// path=...` log line to identify the specific type. See
	// docs/design/stream/watch-list-checkpoint-plan.md.
	MaterializationCheckpointFillsTotal metric.Int64Counter

	// WatchDuplicatesSkippedTotal counts watch events skipped due to duplicate sanitized content.
	WatchDuplicatesSkippedTotal metric.Int64Counter
	// AuditEventsTotal is the single per-event census: every successfully decoded, converted, and
	// validated audit event increments it exactly once, labelled by {outcome, category, group,
	// version, resource, verb}. It subsumes the former received/quality/parked/emitted/shallow/
	// filtered/late-lane counters — see internal/audit/outcome and
	// docs/design/stream/audit-diagnostic-streams-plan.md. Liveness = sum(...) > 0; the e2e
	// invariant gates on category="error" == 0.
	AuditEventsTotal metric.Int64Counter
	// AuditJoinSkewSeconds records the arrival skew between an official audit event and its
	// matching additional body, labelled by which arrived first and how the join resolved.
	AuditJoinSkewSeconds metric.Float64Histogram
	// AuditOfficialGateWaitSeconds records how long an official audit event waited to acquire
	// the in-pod mirror ordering gate before processing.
	AuditOfficialGateWaitSeconds metric.Float64Histogram
	// AuditEventListsTotal counts inbound audit EventList requests at the webhook boundary,
	// labelled by source and bounded outcome.
	AuditEventListsTotal metric.Int64Counter
	// AuditEventListEventsTotal counts decoded audit event items delivered in EventLists,
	// labelled by source and bounded outcome.
	AuditEventListEventsTotal metric.Int64Counter
	// AuditEventListDurationSeconds records how long the webhook takes to answer an
	// EventList request, including in-pod join wait work.
	AuditEventListDurationSeconds metric.Float64Histogram

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
	// MaterializationTypePhase gauges how many types sit in each materialization phase,
	// labelled by {phase} (dormant/requested/syncing/synced/resyncing/failing) — the per-type
	// phase distribution of the demand axis (L-6/L10).
	MaterializationTypePhase metric.Int64Gauge
	// MaterializationClaimedTypes gauges how many types currently hold ≥1 GitTarget claim
	// (the demand surface: how much of the catalog is actually wanted).
	MaterializationClaimedTypes metric.Int64Gauge
	// MaterializationClaimedUnfollowable gauges how many claimed types are not currently
	// followable — the claim-vs-refused mismatch an operator should notice (L10).
	MaterializationClaimedUnfollowable metric.Int64Gauge
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
	uSpec struct {
		name string
		dest *metric.Int64UpDownCounter
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
	if err := registerGauges(); err != nil {
		return err
	}
	return registerUpDownCounters()
}

func registerCounters() error {
	counters := []cSpec{
		{"gitopsreverser_git_operations_total", &GitOperationsTotal},
		{"gitopsreverser_objects_scanned_total", &ObjectsScannedTotal},
		{"gitopsreverser_objects_written_total", &ObjectsWrittenTotal},
		{"gitopsreverser_files_deleted_total", &FilesDeletedTotal},
		{"gitopsreverser_commits_total", &CommitsTotal},
		{"gitopsreverser_commit_bytes_total", &CommitBytesTotal},
		{"gitopsreverser_rebase_retries_total", &RebaseRetriesTotal},
		{"gitopsreverser_ownership_conflicts_total", &OwnershipConflictsTotal},
		{"gitopsreverser_lease_acquire_failures_total", &LeaseAcquireFailuresTotal},
		{"gitopsreverser_marker_conflicts_total", &MarkerConflictsTotal},
		{"gitopsreverser_watch_duplicates_skipped_total", &WatchDuplicatesSkippedTotal},
		{"gitopsreverser_audit_events_total", &AuditEventsTotal},
		{"gitopsreverser_audit_eventlists_total", &AuditEventListsTotal},
		{"gitopsreverser_audit_eventlist_events_total", &AuditEventListEventsTotal},
		{"gitopsreverser_api_catalog_refresh_total", &APICatalogRefreshTotal},
		{"gitopsreverser_secret_encryption_attempts_total", &SecretEncryptionAttemptsTotal},
		{"gitopsreverser_secret_encryption_success_total", &SecretEncryptionSuccessTotal},
		{"gitopsreverser_secret_encryption_failures_total", &SecretEncryptionFailuresTotal},
		{"gitopsreverser_secret_encryption_cache_hits_total", &SecretEncryptionCacheHitsTotal},
		{"gitopsreverser_secret_encryption_marker_skips_total", &SecretEncryptionMarkerSkipsTotal},
		{"gitopsreverser_target_reconcile_completed_total", &TargetReconcileCompletedTotal},
		{"gitopsreverser_resync_background_failures_total", &ResyncBackgroundFailuresTotal},
		{"gitopsreverser_type_lifecycle_reconcile_total", &TypeLifecycleReconcileTotal},
		{"gitopsreverser_type_lifecycle_sweep_total", &TypeLifecycleSweepTotal},
		{"gitopsreverser_materialization_sync_events_total", &MaterializationSyncEventsTotal},
		{"gitopsreverser_materialization_checkpoint_fills_total", &MaterializationCheckpointFillsTotal},
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
	// auditJoinBuckets span the wait budget (sub-second) and the parked-body TTL margin
	// (seconds to minutes) so one set of boundaries fits both skew and gate-wait timings.
	auditJoinBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5, 30, 300}
	// catalogRefreshBuckets span discovery latency: two cached GETs on an aggregated
	// apiserver (sub-second) up to a slow per-group fallback (seconds).
	catalogRefreshBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	hists := []hSpec{
		{"gitopsreverser_git_push_duration_seconds", &GitPushDurationSeconds, nil},
		{"gitopsreverser_audit_join_skew_seconds", &AuditJoinSkewSeconds, auditJoinBuckets},
		{"gitopsreverser_audit_official_gate_wait_seconds", &AuditOfficialGateWaitSeconds, auditJoinBuckets},
		{"gitopsreverser_audit_eventlist_duration_seconds", &AuditEventListDurationSeconds, auditJoinBuckets},
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
		{"gitopsreverser_materialization_type_phase", &MaterializationTypePhase},
		{"gitopsreverser_materialization_claimed_types", &MaterializationClaimedTypes},
		{"gitopsreverser_materialization_claimed_unfollowable", &MaterializationClaimedUnfollowable},
		{"gitopsreverser_branch_worker_queue_depth", &BranchWorkerQueueDepth},
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

func registerUpDownCounters() error {
	upDowns := []uSpec{
		{"gitopsreverser_repo_branch_active_workers", &RepoBranchActiveWorkers},
		{"gitopsreverser_repo_branch_queue_depth", &RepoBranchQueueDepth},
	}
	for _, s := range upDowns {
		v, err := otelMeter.Int64UpDownCounter(s.name)
		if err != nil {
			return err
		}
		*s.dest = v
	}

	return nil
}
