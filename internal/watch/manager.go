// SPDX-License-Identifier: Apache-2.0

// Package watch drives the api-source-of-truth reconcile: it keeps the followability registry
// and the per-GitTarget watched-type tables fresh, and mirrors each watched type into Git from a
// long-lived Kubernetes watch per claimed (GitTarget, GVR, scope). The live watch is the only
// source of ongoing object state, and its events are the writes. The INITIAL desired set the
// mark-and-sweep folds over the Git folder comes from the same watch's sendInitialEvents replay,
// or, on an apiserver that does not serve it, from a LIST with the watch's events buffered behind
// it (targetWatchListAndStream). See docs/architecture.md.
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

	"github.com/ConfigButler/gitops-reverser/internal/queue"
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
// per-GitTarget watched-type tables fresh, and owns every target watch. It holds no object
// informers and no object cache: each stream is a raw watch, opened per claimed (GVR, scope)
// against the GitTarget's own source cluster. The only API touch on a schedule is the discovery
// refresh that keeps the catalogs current. See docs/architecture.md.
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
	// FactStreams is the process-wide subscription set the attribution fact follower reads. Each
	// running target watch holds one reference on the (audit route, group/resource) it covers, so
	// the process follows a type while at least one watch needs it and stops following it when the
	// last one goes away — which is the whole point of the per-type fan-out: facts for a type nobody
	// watches are appended and never received. Nil is configured-author mode: no follower runs, so
	// no subscription is taken.
	FactStreams *queue.FactStreamSet
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
	// resourceCatalogMu guards every clusterContext's catalog/registry edge-triggered
	// logging state (catalogDegradedLogged, typeRefusalsLogged).
	resourceCatalogMu sync.Mutex
	// resourceCatalog seeds the LOCAL cluster's API-resource catalog. Tests set it on a
	// zero-value Manager to drive resolution without an API server; production leaves it nil
	// and the local cluster context builds its own. Aliased to localCluster().catalog.
	resourceCatalog *APIResourceCatalog
	// discoveryClient overrides REST-config discovery construction for the LOCAL cluster in tests.
	discoveryClient func() (apiResourceDiscovery, error)
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
	// followable set onto each target's rules, read by scope resolution and the Declare
	// path instead of each re-resolving inline. watchedTypeInit guards its lazy
	// construction for zero-value Managers in tests.
	watchedTypeInit sync.Once
	watchedTypes    *watchedTypeStore

	// targetWatches is the data plane: one raw watch per (GitTarget, GVR, namespace scope), and
	// the only source of live object state. Its initial desired set comes from the replay, or from
	// the buffered LIST fallback when sendInitialEvents is unsupported.
	//
	// It carries no mutex, deliberately. It is owned by the watch-plane owner loop, which is its
	// only writer and its only reader, so cancelling a stream and starting one no longer happen
	// under a lock that the woken goroutine then has to contend for. See owner.go.
	targetWatches map[string]*targetWatchSet

	// watchLifetime is the parent of every target watch's context: the manager's own lifetime,
	// set once by Start.
	//
	// It is deliberately NOT the context of the pass that starts a stream. A pass runs under a
	// deadline and its context is cancelled the moment it returns, so parenting a stream to it
	// would kill that stream the instant the plan finished being applied — the plan would read as
	// applied while nothing was watching. A stream's lifetime is the manager's; the pass's
	// deadline bounds the pass. Cancelling one stream is the owner's decision, made per cell
	// through the plan diff, never a side effect of a context going out of scope.
	//
	// nil on a Manager that was never started, which is the shape tests drive; streamParent then
	// falls back to the caller's context, which in that setting IS the manager's lifetime.
	watchLifetime atomic.Pointer[context.Context]

	// watchTriggers is the coalescing intake the owner drains: the dirty set, the declare records,
	// and the pending deletions. triggerInit builds it lazily so a zero-value Manager works in
	// tests.
	triggerInit   sync.Once
	watchTriggers *watchPlaneTriggers

	// watchPlaneSnapshot is the immutable projection every reader outside the owner takes: stream
	// readiness, the two write-safety surfaces, the retention roll-up, and the four values
	// captured at declare. stateMu serializes the read-modify-publish and is never held across
	// anything that can block. See watch_plane_state.go.
	stateMu            sync.Mutex
	watchPlaneSnapshot atomic.Pointer[*watchPlaneState]

	// gitPathEventsCh carries a GenericEvent for a GitTarget whenever its GitPath acceptance
	// state TRANSITIONS, so the GitTarget controller re-projects GitPathAccepted promptly
	// instead of waiting up to RequeueSteadyInterval (5m) for its next periodic reconcile. The
	// data plane records acceptance asynchronously; without this edge the status lags. See
	// docs/spec/manifest-system.md. Lazily
	// created by GitPathEvents() and guarded by gitPathEventsMu.
	gitPathEventsMu sync.Mutex
	gitPathEventsCh chan event.GenericEvent

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

// Start begins the watch ingestion manager and blocks until context cancellation. Everything the
// watch plane does happens on the loop it enters: controllers submit intent, this owns the state.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (watch-first ingestion)")
	defer log.Info("watch ingestion manager stopping")

	// Every target watch is parented here, not to the pass that starts it.
	m.watchLifetime.Store(&ctx)

	// Arm the trigger informers before the first refresh: each successful catalog refresh
	// re-evaluates which triggers discovery actually serves, and starts the ones that
	// became available. They are stopped by this context, never by a reconcile's.
	m.setTriggerContext(ctx)

	if err := m.bootstrapRuleStore(ctx, log.WithName("bootstrap")); err != nil {
		log.Error(err, "RuleStore bootstrap failed, continuing with current in-memory state")
	}

	m.runOwnerLoop(ctx, log.WithName("owner"))
	return nil
}

// NeedLeaderElection ensures only the elected leader runs the watch manager.
func (m *Manager) NeedLeaderElection() bool {
	return true
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
