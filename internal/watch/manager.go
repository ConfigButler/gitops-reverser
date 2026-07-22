// SPDX-License-Identifier: Apache-2.0

// Package watch drives the api-source-of-truth reconcile: it keeps the followability
// registry and the demand-driven materialization axis fresh, fills per-type checkpoints,
// and reconciles each watched type into Git by SPLICING the per-type Redis materialization
// (checkpoint + audit log) into a desired set — no long-lived object watch is held (R3).
package watch

import (
	"context"
	"sync"
	"sync/atomic"
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

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The API-resource catalog reads the two resources that describe the API surface itself, so
// it can tell a type that vanished from one the operator was never allowed to see. These are
// the trigger informers' resources too; without them a least-privilege install gets a 403 on
// every reflector retry, which is why the trigger error handler stops on Forbidden.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;list;watch

// A WatchRule may name ANY type, so the watch manager's read access cannot be derived from
// code. It is deliberately NOT a marker: controller-gen would fold `groups=*,resources=*`
// into this ClusterRole, and RBAC is additive — a wildcard read grants cluster-wide Secret
// list/watch no matter how narrow the Secret rule beside it is. The wildcard therefore lives
// in a ClusterRole of its own (config/rbac/watch-any-role.yaml, `rbac.watchTypes.mode: any` in
// the chart), so an operator that mirrors two CRDs can be told the exact types it may read and
// be denied every other object in the cluster. See docs/rbac.md.

// Manager is a controller-runtime Runnable that keeps the followability registry and the
// demand-driven materialization axis fresh and drives the per-type splice reconcile. It
// holds NO long-lived object informers: the only always-on resource intake is the
// audit-webhook push (mirrored into the per-type :audit:stream); the only API touch on a
// schedule is the brief checkpoint fill (mirrorTypeObjects) the materialization driver runs
// for claimed types. See docs/architecture.md.
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
	// is configured-author mode (no audit/Redis): every event commits as the committer.
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

	// SourceClusters resolves a GitTarget's source cluster — a ClusterProvider NAME — into a
	// rest.Config, reading the kubeconfig Secret the provider names from the config plane. It is
	// required for any GitTarget to mirror, single-cluster installs included: a source cluster is
	// always a ClusterProvider, and only this resolver can say whether that provider is in-cluster
	// (kubeConfig omitted) or remote. Nil leaves every source cluster unresolvable; only the config
	// plane, which needs no provider, still works.
	SourceClusters SourceClusterResolver

	// clusters holds one clusterContext per distinct cluster — its API catalog, type registry,
	// and clients. configPlaneClusterID is the operator's own cluster (always present, never a
	// source); every other key is a ClusterProvider name. See cluster_context.go.
	clustersMu sync.Mutex
	clusters   map[string]*clusterContext
	// clusterOrder is the published, ordered snapshot of clusters (local first). The git
	// writer's cluster-scoped GVK lookup reads it once per document it scans out of a Git
	// folder, on the branch-worker goroutine, so it must not contend on clustersMu with the
	// reconcile loop.
	clusterOrder atomic.Pointer[[]*clusterContext]
	// gitTargetClusters maps a GitTarget key to the source-cluster id it mirrors from,
	// captured on Declare (the gitTargetUIDs pattern). Because spec.clusterProviderRef is immutable
	// this is learned once and never changes — no per-rule propagation, no disagreement
	// window. Guarded by gitTargetClustersMu.
	gitTargetClustersMu sync.Mutex
	gitTargetClusters   map[string]string

	// clusterAuditRoutes maps a source-cluster id to the audit route its attribution facts are
	// keyed under (ClusterProvider.AuditRoute()), also captured on Declare. It is keyed by CLUSTER
	// rather than by GitTarget because the route belongs to the provider, and unlike the cluster id
	// it is mutable. Guarded by gitTargetClustersMu, alongside the map it is captured with.
	clusterAuditRoutes map[string]string

	// resourceCatalogMu guards every clusterContext's catalog/registry edge-triggered
	// logging state (catalogDegradedLogged, typeRefusalsLogged).
	resourceCatalogMu sync.Mutex
	// resourceCatalog seeds the LOCAL cluster's API-resource catalog. Tests set it on a
	// zero-value Manager to drive resolution without an API server; production leaves it nil
	// and the local cluster context builds its own. Aliased to localCluster().catalog.
	resourceCatalog *APIResourceCatalog
	// discoveryClient overrides REST-config discovery construction for the LOCAL cluster in tests.
	discoveryClient func() (apiResourceDiscovery, error)
	// catalogRefreshCh coalesces API-surface trigger watch events into manager reconciliation.
	catalogRefreshCh chan struct{}
	// triggersMu guards the API-surface trigger informer set below. The informers are
	// (re-)evaluated after every catalog refresh — which controllers drive, not just the
	// manager's own loop — so this is not a startup-only structure.
	triggersMu sync.Mutex
	// triggerCtx is the manager's lifetime context, the parent of every trigger informer's
	// own context. Set once by Start; nil before then, which defers informer creation.
	triggerCtx context.Context
	// triggerClient is the dynamic client the trigger informers list and watch through.
	// Built once from the REST config, or injected by tests.
	triggerClient dynamic.Interface
	// triggersStarted is the set of trigger resources whose informer is already running.
	triggersStarted map[schema.GroupVersionResource]struct{}
	// triggerStops cancels one trigger informer without touching the others. Each informer
	// gets its own context so a single forbidden resource can be stopped and later re-armed.
	triggerStops map[schema.GroupVersionResource]context.CancelFunc
	// triggersSkipLogged records which unserved trigger resources have already been logged,
	// so a permanently absent aggregation layer produces one line, not one per refresh.
	triggersSkipLogged map[schema.GroupVersionResource]struct{}
	// triggersForbiddenLogged records which trigger resources RBAC has already denied, so a
	// permanently unauthorized resource produces one line per denial, not one per retry.
	triggersForbiddenLogged map[schema.GroupVersionResource]struct{}

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
	// targetRenderFidelity is the projected state of the shared worker gate. It keeps the
	// target-watch epoch observable without making a last successful scoped resync overwrite a
	// sibling scope's divergence.
	targetRenderFidelity map[string]git.RenderFidelityStatus

	// gitPathEventsCh carries a GenericEvent for a GitTarget whenever its GitPath acceptance
	// state TRANSITIONS, so the GitTarget controller re-projects GitPathAccepted promptly
	// instead of waiting up to RequeueSteadyInterval (5m) for its next periodic reconcile. The
	// data plane records acceptance asynchronously; without this edge the status lags. See
	// docs/spec/manifest-system.md. Lazily
	// created by GitPathEvents() and guarded by gitPathEventsMu.
	gitPathEventsMu sync.Mutex
	gitPathEventsCh chan event.GenericEvent

	// gitTargetUIDs maps a GitTarget's namespace/name key to its object UID, captured
	// from the controller on Declare. The watch data plane keys resume cursors by this
	// UID — the rule-derived watch tables don't carry it — so a GitTarget recreated
	// under the same name never inherits its predecessor's cursor.
	gitTargetUIDsMu sync.Mutex
	gitTargetUIDs   map[string]string

	// gitTargetPruneModes maps a GitTarget key to the effective spec.prune.mode of its LAST
	// successful Declare. Unlike the UID and the source cluster this value is mutable, and it is
	// remembered for exactly one reason: to detect the edge where an operator widens the policy to
	// one that sweeps, which must force a fresh replay or the newly authorized cleanup never runs.
	// See prune_declaration.go. Guarded by gitTargetPruneModesMu.
	gitTargetPruneModesMu sync.Mutex
	gitTargetPruneModes   map[string]v1alpha3.PruneMode

	// targetRetention holds each GitTarget's per-scope retained-document counts, epoch-keyed so a
	// scope that leaves the watch plan takes its count with it. Projected onto status.retention.
	// See retention_rollup.go. Guarded by targetRetentionMu.
	targetRetentionMu sync.Mutex
	targetRetention   map[string]targetRetentionState

	// declaredGVRsMu guards declaredGVRs: the type-set each GitTarget last Declared. The watch-first
	// data plane reads it to drive the per-(GitTarget, type) watch set; re-declaring is idempotent.
	declaredGVRsMu sync.Mutex
	declaredGVRs   map[string]map[schema.GroupVersionResource]struct{}

	// sourceNamespaceScope is the source-scope service: the per-source-cluster Namespace label
	// snapshot that GitTarget.allowedSourceNamespaces selectors are evaluated against, plus the
	// per-rule resolved scopes the establishing/maintaining contract turns on. See
	// source_namespace_scope.go. Lazily built so a zero-value Manager works in tests.
	sourceScopeInit      sync.Once
	sourceNamespaceScope *sourceNamespaceScope

	// sourceNamespaceEventsCh carries a GenericEvent for every GitTarget on a source cluster whose
	// Namespace labels changed, so a selector-driven grant or revocation reaches the WatchRule
	// controller on the change instead of waiting up to RequeueSteadyInterval (5m). Lazily created
	// by SourceNamespaceEvents() and guarded by sourceNamespaceEventsMu.
	sourceNamespaceEventsMu sync.Mutex
	sourceNamespaceEventsCh chan event.GenericEvent
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
	// Arm the trigger informers before the first refresh: each successful catalog refresh
	// re-evaluates which triggers discovery actually serves, and starts the ones that
	// became available. They are stopped by this context, never by a reconcile's.
	m.setTriggerContext(ctx)

	if err := m.bootstrapRuleStore(ctx, log.WithName("bootstrap")); err != nil {
		log.Error(err, "RuleStore bootstrap failed, continuing with current in-memory state")
	}

	// Perform initial reconciliation
	if err := m.ReconcileForRuleChange(ctx); err != nil {
		log.Error(err, "Initial reconciliation failed, will retry periodically")
	}

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

	// Re-list the source-cluster Namespace labels any selector policy has asked about, BEFORE the
	// tables are re-resolved: this is where a source-namespace grant or revocation is observed,
	// and a change enqueues the affected GitTargets so their WatchRules re-run the gate.
	m.refreshSourceNamespaceScopes(ctx)

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
