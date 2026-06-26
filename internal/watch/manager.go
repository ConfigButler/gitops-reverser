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

// Package watch drives the api-source-of-truth reconcile: it keeps the followability
// registry and the demand-driven materialization axis fresh, fills per-type checkpoints,
// and reconciles each watched type into Git by SPLICING the per-type Redis materialization
// (checkpoint + audit log) into a desired set — no long-lived object watch is held (R3).
package watch

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// RBAC permissions for dynamic watch manager - read-only access to watch all (also future ones!) resource types
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

// Manager is a controller-runtime Runnable that keeps the followability registry and the
// demand-driven materialization axis fresh and drives the per-type splice reconcile. It
// holds NO long-lived object informers: the only always-on resource intake is the
// audit-webhook push (mirrored into the per-type :audit:stream); the only API touch on a
// schedule is the brief checkpoint fill (mirrorTypeObjects) the materialization driver runs
// for claimed types. See docs/design/stream/api-source-of-truth-reconcile.md.
type Manager struct {
	// Client provides cluster access.
	Client client.Client
	// Log is the logger to use.
	Log logr.Logger
	// RuleStore gives access to compiled WatchRule/ClusterWatchRule.
	RuleStore *rulestore.RuleStore
	// EventRouter dispatches per-type reconciles/sweeps and field-patch events to branch workers.
	EventRouter *EventRouter
	// AuthorResolver optionally names the commit author for a live watch event by
	// joining the audit attribution index (RV/UID match, bounded grace window). Nil
	// is committer-only mode (no audit/Redis): every event commits as the committer.
	AuthorResolver AuthorResolver
	// WatchCursorStore optionally persists per-watch resourceVersion cursors so
	// reconnects can resume without replaying the full type snapshot.
	WatchCursorStore CursorStore
	// SensitiveResources is the startup-configured policy classifying which types must
	// use the encrypted Git write path. It is applied when the followability registry
	// builds its observations, so each TypeRecord carries the right Sensitive fact. The
	// zero value still treats core Secrets as sensitive.
	SensitiveResources types.SensitiveResourcePolicy

	// dynamicClient overrides the config-built dynamic client when non-nil.
	// Used in tests to inject a fake client without a real REST config.
	dynamicClient dynamic.Interface
	// targetWatchOpen overrides how per-GitTarget state watches are opened. nil
	// means build them from dynamicClient/rest config.
	targetWatchOpen func(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace string,
		opts metav1.ListOptions,
	) (watch.Interface, error)
	// targetWatchList overrides how per-GitTarget fallback snapshots are listed.
	// nil means build them from dynamicClient/rest config.
	targetWatchList func(
		ctx context.Context,
		gvr schema.GroupVersionResource,
		namespace string,
		opts metav1.ListOptions,
	) (*unstructured.UnstructuredList, error)

	// resourceCatalog is the shared discovery-backed API surface used by rule planning.
	resourceCatalogMu sync.Mutex
	resourceCatalog   *APIResourceCatalog
	// discoveryClient overrides REST-config discovery construction in tests.
	discoveryClient func() (apiResourceDiscovery, error)
	// catalogRefreshCh coalesces API-surface trigger watch events into manager reconciliation.
	catalogRefreshCh chan struct{}
	// catalogReadyOnce guards the one-time "catalog ready" log line, matching the
	// firstMessage/firstGroupReady sync.Once pattern used by the audit consumer.
	catalogReadyOnce sync.Once
	// catalogDegradedLogged is the degraded group/version set last reflected in
	// the log; logCatalogTransitions diffs against it to log appear/clear
	// transitions (degradation can recur, so this is not a one-shot). Guarded by
	// resourceCatalogMu.
	catalogDegradedLogged map[schema.GroupVersion]struct{}

	// watchedTypes is the resident, per-GitTarget watched-type table set: the single
	// source of "what each GitTarget watches", a projection of the type registry's
	// followable set onto each target's rules, read by the splice scope resolution and
	// the demand Declare instead of each re-resolving inline. watchedTypeInit guards its
	// lazy construction for zero-value Managers in tests.
	watchedTypeInit sync.Once
	watchedTypes    *watchedTypeStore

	// targetWatches is the watch-first data plane: one raw watch per
	// (GitTarget, GVR, namespace scope). It replaces the materialized Redis
	// checkpoint/audit-tail pipeline as the source of object state.
	targetWatchesMu sync.Mutex
	targetWatches   map[string]*targetWatchSet

	// typeRegistry is the followability decision surface (see
	// docs/design/manifest/version2/type-followability.md): one typeset.TypeRecord
	// per served type, refreshed from the catalog scan on every catalog refresh. It
	// is the inventory/status surface ("is this type followable, and if not, why?");
	// typeRegistryInit guards its lazy construction for zero-value Managers in tests.
	typeRegistryInit sync.Once
	typeRegistry     *typeset.Registry
	// typeRefusalsLogged is the GVK->summary of every type the registry currently
	// refuses, so the central "why is this not followable?" log is edge-triggered: a
	// stable refusal is logged once, not on every refresh. Guarded by resourceCatalogMu.
	typeRefusalsLogged map[string]string

	// materializer is the demand-met second axis beside typeRegistry (see
	// docs/design/stream/demand-driven-type-materialization-lifecycle.md): it owns the
	// per-(GitTarget, type) claim table and the materialization phase machine. The
	// registry answers "can we follow this type?"; the materializer answers "has any
	// GitTarget claimed it, and have we listed it?". materializerInit guards its lazy
	// construction for zero-value Managers in tests, mirroring typeRegistryInit.
	materializerInit sync.Once
	materializer     *typeset.Materializer
	// declaredGVRsMu guards declaredGVRs: the type-set each GitTarget last Declared, so the per-type
	// splice reconcile fires ONCE per (GitTarget, type) — when the type is newly claimed and already
	// Synced — for the initial backfill, after which the per-event audit tail owns live changes
	// (preserving their authorship). Re-folding the log on every Declare would re-commit live changes
	// with the bulk reconcile's default author and churn Git ("initial = checkpoint, then replay").
	declaredGVRsMu sync.Mutex
	declaredGVRs   map[string]map[schema.GroupVersionResource]struct{}

	// lateNudgeMu guards lateNudgeAt: the last time a divert nudged a type's
	// resync (NudgeTypeResyncForLateEvent), the per-type floor that keeps sustained
	// out-of-order arrivals from churning checkpoint LISTs.
	lateNudgeMu sync.Mutex
	lateNudgeAt map[schema.GroupVersionResource]time.Time
}

const (
	heartbeatInterval         = 30 * time.Second
	periodicReconcileInterval = 30 * time.Second
)

// Start begins the watch ingestion manager and blocks until context cancellation.
// Performs initial reconciliation then runs periodic discovery refresh.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (watch-first ingestion)")
	defer log.Info("watch ingestion manager stopping")

	m.initializeManagerState()

	if err := m.bootstrapRuleStore(ctx, log.WithName("bootstrap")); err != nil {
		log.Error(err, "RuleStore bootstrap failed, continuing with current in-memory state")
	}

	// Perform initial reconciliation
	if err := m.ReconcileForRuleChange(ctx); err != nil {
		log.Error(err, "Initial reconciliation failed, will retry periodically")
	}
	m.startAPISurfaceTriggerInformers(ctx, log.WithName("catalog-triggers"))

	// Periodic reconciliation for CRD detection and missed changes
	periodicTicker := time.NewTicker(periodicReconcileInterval)
	defer periodicTicker.Stop()

	// Heartbeat ticker to make liveness observable in logs and tests.
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-periodicTicker.C:
			log.V(1).Info("Periodic reconciliation triggered")
			if err := m.ReconcileForRuleChange(ctx); err != nil {
				log.Error(err, "Periodic reconciliation failed")
			}

		case <-m.catalogRefreshCh:
			log.V(1).Info("API surface trigger reconciliation")
			if err := m.ReconcileForRuleChange(ctx); err != nil {
				log.Error(err, "API surface trigger reconciliation failed")
			}

		case <-heartbeatTicker.C:
			log.V(1).Info("Watch manager heartbeat")
		}
	}
}

func (m *Manager) initializeManagerState() {
	if m.catalogRefreshCh == nil {
		m.catalogRefreshCh = make(chan struct{}, 1)
	}
}

// NeedLeaderElection ensures only the elected leader runs the watch manager.
func (m *Manager) NeedLeaderElection() bool {
	return true
}

// dynamicClientFromConfig builds a dynamic client from the controller's REST config.
// If m.dynamicClient is set (e.g. in tests) it is returned directly. It is used by the
// per-type checkpoint fill (mirrorTypeObjects) — the only API touch on a schedule.
func (m *Manager) dynamicClientFromConfig(log logr.Logger) dynamic.Interface {
	if m.dynamicClient != nil {
		return m.dynamicClient
	}
	cfg := m.restConfig()
	if cfg == nil {
		log.Info("skipping seed - no rest config available")
		return nil
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "failed to construct dynamic client for seed")
		return nil
	}
	return dc
}

// ReconcileForRuleChange refreshes the trusted API catalog and the resident watched-type
// tables when rules change or a CRD is installed/removed. It no longer starts object
// informers or gathers a whole-GitTarget snapshot (R3): the catalog refresh drives the
// followability registry, whose transitions gate the materialization axis (which types get
// a checkpoint) and fan per-type reconciles; the splice off that checkpoint is the only
// resource-mirror path. Called by the WatchRule/ClusterWatchRule controllers after rule
// modifications, by the periodic ticker, and by the API-surface trigger.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	log := m.Log.WithName("reconcile")
	log.V(1).Info("Reconciling watch manager for rule change")

	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return err
	}

	// Re-resolve the resident watched-type tables now that the catalog is fresh. This is
	// gated on a rule-set change or catalog generation bump, so a periodic reconcile with
	// neither reuses the resolved tables. The target-watch runner reads these tables.
	m.refreshWatchedTypeTables()
	m.refreshRunningTargetWatches(ctx)
	return nil
}

// recordTargetReconcileCompleted increments the per-GitTarget recovery counter once a
// per-type reconcile has been applied, or a cursor-backed watch resume has been established,
// tagged with the trigger that drove the pass. On a controller restart the new pod's counter
// starts at 0, so a per-pod `{pod="<new>"} > 0` reading shows the new pod completed its own
// recovery. No-op until the counter is registered.
func (m *Manager) recordTargetReconcileCompleted(gitDest types.ResourceReference, trigger string) {
	if telemetry.TargetReconcileCompletedTotal == nil {
		return
	}
	telemetry.TargetReconcileCompletedTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
		attribute.String("trigger", trigger),
	))
}

// SetupWithManager is a placeholder to enable kubebuilder RBAC marker scanning.
// The Manager is manually added to the controller-runtime manager in main.go as a Runnable,
// but this method allows kubebuilder's controller-gen to discover and process the RBAC markers.
func (m *Manager) SetupWithManager(mgr ctrl.Manager) error {
	// No actual setup needed - Manager is added manually in cmd/main.go
	// This method exists solely for kubebuilder RBAC marker scanning
	_ = mgr // Unused but required for signature
	return nil
}
