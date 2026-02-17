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
Package metrics provides the OpenTelemetry-based metrics exporter for GitOps Reverser.
It configures Prometheus-compatible metrics collection for monitoring controller operations.
*/
package metrics

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
	EventsReceivedTotal    metric.Int64Counter
	EventsProcessedTotal   metric.Int64Counter
	GitOperationsTotal     metric.Int64Counter
	GitPushDurationSeconds metric.Float64Histogram
	GitCommitQueueSize     metric.Int64UpDownCounter

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

	// WebhookCorrelationsTotal counts correlation entries created by webhook.
	WebhookCorrelationsTotal metric.Int64Counter
	// EnrichHitsTotal counts successful webhook→watch correlation enrichments.
	EnrichHitsTotal metric.Int64Counter
	// EnrichMissesTotal counts failed webhook→watch correlations (no match found).
	EnrichMissesTotal metric.Int64Counter
	// KVEvictionsTotal counts correlation store evictions (TTL or LRU).
	KVEvictionsTotal metric.Int64Counter
	// WatchDuplicatesSkippedTotal counts watch events skipped due to duplicate sanitized content.
	WatchDuplicatesSkippedTotal metric.Int64Counter
	// AuditEventsReceivedTotal counts audit events received from Kubernetes API server.
	AuditEventsReceivedTotal metric.Int64Counter
	// Secret encryption pipeline counters.
	SecretEncryptionAttemptsTotal    metric.Int64Counter
	SecretEncryptionSuccessTotal     metric.Int64Counter
	SecretEncryptionFailuresTotal    metric.Int64Counter
	SecretEncryptionCacheHitsTotal   metric.Int64Counter
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

	// Register instruments in compact loops to keep complexity low.
	type cSpec struct {
		name string
		dest *metric.Int64Counter
	}
	type hSpec struct {
		name string
		dest *metric.Float64Histogram
	}
	type uSpec struct {
		name string
		dest *metric.Int64UpDownCounter
	}

	counters := []cSpec{
		{"gitopsreverser_events_received_total", &EventsReceivedTotal},
		{"gitopsreverser_events_processed_total", &EventsProcessedTotal},
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
		{"gitopsreverser_webhook_correlations_total", &WebhookCorrelationsTotal},
		{"gitopsreverser_enrich_hits_total", &EnrichHitsTotal},
		{"gitopsreverser_enrich_misses_total", &EnrichMissesTotal},
		{"gitopsreverser_kv_evictions_total", &KVEvictionsTotal},
		{"gitopsreverser_watch_duplicates_skipped_total", &WatchDuplicatesSkippedTotal},
		{"gitopsreverser_audit_events_received_total", &AuditEventsReceivedTotal},
		{"gitopsreverser_secret_encryption_attempts_total", &SecretEncryptionAttemptsTotal},
		{"gitopsreverser_secret_encryption_success_total", &SecretEncryptionSuccessTotal},
		{"gitopsreverser_secret_encryption_failures_total", &SecretEncryptionFailuresTotal},
		{"gitopsreverser_secret_encryption_cache_hits_total", &SecretEncryptionCacheHitsTotal},
		{"gitopsreverser_secret_encryption_marker_skips_total", &SecretEncryptionMarkerSkipsTotal},
	}
	for _, s := range counters {
		v, err := otelMeter.Int64Counter(s.name)
		if err != nil {
			return nil, err
		}
		*s.dest = v
	}

	hists := []hSpec{
		{"gitopsreverser_git_push_duration_seconds", &GitPushDurationSeconds},
	}
	for _, s := range hists {
		v, err := otelMeter.Float64Histogram(s.name)
		if err != nil {
			return nil, err
		}
		*s.dest = v
	}

	upDowns := []uSpec{
		{"gitopsreverser_git_commit_queue_size", &GitCommitQueueSize},
		{"gitopsreverser_repo_branch_active_workers", &RepoBranchActiveWorkers},
		{"gitopsreverser_repo_branch_queue_depth", &RepoBranchQueueDepth},
	}
	for _, s := range upDowns {
		v, err := otelMeter.Int64UpDownCounter(s.name)
		if err != nil {
			return nil, err
		}
		*s.dest = v
	}

	return func(_ context.Context) error {
		fmt.Println("Shutting down OTLP exporter")
		return nil
	}, nil
}
