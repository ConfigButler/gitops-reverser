// SPDX-License-Identifier: Apache-2.0

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
	"sigs.k8s.io/controller-runtime/pkg/event"

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

	// liveContentDedup caches, per (gitDest, object), the hash of the last sanitized
	// content routed to a branch worker. A live UPDATE whose sanitized content is
	// unchanged (the classic /status-only churn, which carries no git-writable change)
	// is dropped before routing, so it cannot split an open commit window by arriving
	// unattributed against a named window author. Keyed by gitDest+gvr+namespace+uid;
	// entries are cleared on delete. Cross-session by design: a reconnect keeps deduping
	// against what git already holds. See routeLiveTargetWatchEvent.
	liveContentDedup sync.Map

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
	// targetStreamStates is the readiness surface for targetWatches. It is keyed
	// by GitTarget and watch key, and projected into status by controllers.
	targetStreamStates map[string]map[targetWatchKey]targetStreamStatus
	// targetGitPathAcceptance is the target-side acceptance surface. It is keyed by
	// GitTarget and projected into GitTarget status as GitPathAccepted.
	targetGitPathAcceptance map[string]GitPathAcceptanceStatus

	// gitPathEventsCh carries a GenericEvent for a GitTarget whenever its GitPath acceptance
	// state TRANSITIONS, so the GitTarget controller re-projects GitPathAccepted promptly
	// instead of waiting up to RequeueLongInterval (10m) for its next periodic reconcile. The
	// data plane records acceptance asynchronously; without this edge the status lags. See
	// docs/design/manifest/gitpathaccepted-projection-race-and-external-drift.md. Lazily
	// created by GitPathEvents() and guarded by gitPathEventsMu.
	gitPathEventsMu sync.Mutex
	gitPathEventsCh chan event.GenericEvent

	// gitTargetUIDs maps a GitTarget's namespace/name key to its object UID, captured
	// from the controller on Declare. The watch data plane keys resume cursors by this
	// UID — the rule-derived watch tables don't carry it — so a GitTarget recreated
	// under the same name never inherits its predecessor's cursor.
	gitTargetUIDsMu sync.Mutex
	gitTargetUIDs   map[string]string

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

	// declaredGVRsMu guards declaredGVRs: the type-set each GitTarget last Declared. The watch-first
	// data plane reads it to drive the per-(GitTarget, type) watch set; re-declaring is idempotent.
	declaredGVRsMu sync.Mutex
	declaredGVRs   map[string]map[schema.GroupVersionResource]struct{}
}

// GitPathAcceptanceStatus is the whole-target write-safety status for a GitTarget path.
type GitPathAcceptanceStatus struct {
	Accepted bool
	Reason   string
	Message  string
	At       metav1.Time
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
