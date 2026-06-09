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

// Package watch provides List+Watch ingestion for cluster-as-source-of-truth.
package watch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cespare/xxhash/v2"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// RBAC permissions for dynamic watch manager - read-only access to watch all (also future ones!) resource types
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

// Manager is a controller-runtime Runnable that will host dynamic informers
// and translate List+Watch deltas into gitops-reverser events.
// Implements deduplication to prevent status-only changes from creating redundant commits.
type Manager struct {
	// Client provides cluster access.
	Client client.Client
	// Log is the logger to use.
	Log logr.Logger
	// RuleStore gives access to compiled WatchRule/ClusterWatchRule.
	RuleStore *rulestore.RuleStore
	// EventRouter dispatches events to branch workers (replaces EventQueue).
	EventRouter *EventRouter
	// AuditLiveEventsEnabled makes the audit pipeline authoritative for live mutating events.
	// Watchers still support discovery and snapshot/reconcile flows.
	AuditLiveEventsEnabled bool
	// SensitiveResources is the startup-configured policy classifying which types must
	// use the encrypted Git write path. It is applied when the followability registry
	// builds its observations, so each TypeRecord carries the right Sensitive fact. The
	// zero value still treats core Secrets as sensitive.
	SensitiveResources types.SensitiveResourcePolicy
	// Deduplication: tracks last seen content hash per resource to skip status-only changes
	lastSeenMu   sync.RWMutex
	lastSeenHash map[string]uint64 // resourceKey → content hash (key uses types.ResourceIdentifier.Key)

	// Dynamic informer lifecycle management
	informersMu       sync.Mutex
	activeInformers   map[GVR]map[string]context.CancelFunc                   // GVR -> namespace -> cancel (empty string = cluster-wide)
	informerFactories map[string]dynamicinformer.DynamicSharedInformerFactory // namespace -> factory (empty string = cluster-wide)

	// dynamicClient overrides the config-built dynamic client when non-nil.
	// Used in tests to inject a fake client without a real REST config.
	dynamicClient dynamic.Interface

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

	// snapshotEmitCount tracks how many times emitSnapshotForRuleChange has
	// actually emitted snapshots for at least one affected GitTarget. Useful for
	// tests to observe the snapshot-trigger contract and will be exposed as a
	// Prometheus metric later.
	snapshotEmitCount atomic.Int64

	// ruleSetSnapshotMu protects per-GitTarget snapshot delivery state.
	ruleSetSnapshotMu        sync.Mutex
	lastDeliveredRuleSetHash map[string]uint64
	pendingRuleSetHash       map[string]uint64

	// watchedTypes is the resident, per-GitTarget watched-type table set: the single
	// source of "what each GitTarget watches", a projection of the type registry's
	// followable set onto each target's rules, read by the snapshot, informer, and
	// plan-hash paths instead of each re-resolving inline. watchedTypeInit guards its
	// lazy construction for zero-value Managers in tests.
	watchedTypeInit sync.Once
	watchedTypes    *watchedTypeStore

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

	// lifecycleEvents carries per-type registry transitions (TypeActivated / TypeRemoved /
	// …) from the registry's updater to the drain goroutine that drives the M12 per-type
	// reconcile/sweep. lifecycleConsumerOnce guards the one-time subscribe + goroutine start.
	lifecycleEvents       chan typeset.LifecycleEvent
	lifecycleConsumerOnce sync.Once
}

// SnapshotEmitCount returns the number of times the manager has emitted a
// snapshot for rule changes since process start.
func (m *Manager) SnapshotEmitCount() int64 {
	return m.snapshotEmitCount.Load()
}

type ruleSetSnapshotTarget struct {
	gitDest    types.ResourceReference
	hash       uint64
	hasEntries bool
}

// targetWatchPlan accumulates a single GitTarget's effective watch surface while
// currentRuleSetSnapshots walks the rule set. entries maps an entry key
// ("group/version/resource|scope|ns") to the union of operation tokens watched
// for it; dest holds the write destination. hash() folds both into a stable
// per-target hash. See currentRuleSetSnapshots for the rationale on what is and
// is not included.
type targetWatchPlan struct {
	gitDest types.ResourceReference
	entries map[string]map[string]struct{}
	dest    string
}

// addEntry records that the target watches gvr in the given namespace ("" means
// all namespaces / cluster-scoped) for the given operations. An empty or
// wildcard operation set is canonicalised to "*" (all operations), and once "*"
// is present it subsumes any explicit operations during hashing.
func (p *targetWatchPlan) addEntry(gvr GVR, namespace string, ops []configv1alpha1.OperationType) {
	entryKey := fmt.Sprintf("%s/%s/%s|scope=%s|ns=%s",
		gvr.Group, gvr.Version, gvr.Resource, gvr.Scope, namespace)
	opsSet := p.entries[entryKey]
	if opsSet == nil {
		opsSet = make(map[string]struct{})
		p.entries[entryKey] = opsSet
	}
	if len(ops) == 0 {
		opsSet["*"] = struct{}{}
		return
	}
	for _, op := range ops {
		if op == configv1alpha1.OperationAll {
			opsSet["*"] = struct{}{}
			continue
		}
		opsSet[string(op)] = struct{}{}
	}
}

// hash returns a stable hash of the plan: destination plus each entry with its
// resolved operation set, sorted for determinism.
func (p *targetWatchPlan) hash() uint64 {
	entryStrings := make([]string, 0, len(p.entries))
	for entryKey, opsSet := range p.entries {
		var ops []string
		if _, all := opsSet["*"]; all {
			ops = []string{"*"}
		} else {
			ops = make([]string, 0, len(opsSet))
			for op := range opsSet {
				ops = append(ops, op)
			}
			sort.Strings(ops)
		}
		entryStrings = append(entryStrings, entryKey+"|ops="+strings.Join(ops, ","))
	}
	sort.Strings(entryStrings)
	return xxhash.Sum64String(p.dest + "\x00" + strings.Join(entryStrings, "\x00"))
}

const (
	heartbeatInterval         = 30 * time.Second
	periodicReconcileInterval = 30 * time.Second
	minResourceKeyParts       = 3
	resourceKeyCapacity       = 5
)

// Start begins the watch ingestion manager and blocks until context cancellation.
// Performs initial reconciliation then runs periodic discovery refresh.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (reconciliation-based)")
	defer log.Info("watch ingestion manager stopping")

	m.initializeManagerState()

	// Subscribe to the registry's per-type transitions before the first reconcile drives a
	// registry Update, so cold-start activations drive the M12 per-type reconcile path.
	m.startTypeLifecycleConsumer(ctx, log.WithName("type-lifecycle"))

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
			m.shutdown()
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
			m.informersMu.Lock()
			totalInformers := 0
			for _, nsMap := range m.activeInformers {
				totalInformers += len(nsMap)
			}
			m.informersMu.Unlock()
			log.V(1).Info("Watch manager heartbeat", "activeInformers", totalInformers)
		}
	}
}

func (m *Manager) initializeManagerState() {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()
	if m.activeInformers == nil {
		m.activeInformers = make(map[GVR]map[string]context.CancelFunc)
	}
	if m.informerFactories == nil {
		m.informerFactories = make(map[string]dynamicinformer.DynamicSharedInformerFactory)
	}
	if m.catalogRefreshCh == nil {
		m.catalogRefreshCh = make(chan struct{}, 1)
	}
}

// shutdown gracefully stops all active informers.
func (m *Manager) shutdown() {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()

	for gvr, nsMap := range m.activeInformers {
		for ns, cancel := range nsMap {
			cancel()
			m.Log.V(1).Info("Shutdown: stopped informer",
				"group", gvr.Group,
				"version", gvr.Version,
				"resource", gvr.Resource,
				"namespace", ns)
		}
	}

	m.activeInformers = make(map[GVR]map[string]context.CancelFunc)
	m.informerFactories = make(map[string]dynamicinformer.DynamicSharedInformerFactory)
}

// NeedLeaderElection ensures only the elected leader runs the watchers.
func (m *Manager) NeedLeaderElection() bool {
	return true
}

// getNamespaceLabels fetches the labels of a namespace, returning nil if unavailable.
func (m *Manager) getNamespaceLabels(ctx context.Context, namespace string) map[string]string {
	if namespace == "" {
		return nil
	}
	ns := &corev1.Namespace{}
	if err := m.Client.Get(ctx, k8stypes.NamespacedName{Name: namespace}, ns); err == nil {
		return ns.Labels
	}
	return nil
}

// matchRules returns matching WatchRule and ClusterWatchRule entries for the given object.
func (m *Manager) matchRules(
	u *unstructured.Unstructured,
	resourcePlural, apiGroup, apiVersion string,
	isClusterScoped bool,
	nsLabels map[string]string,
) ([]rulestore.CompiledRule, []rulestore.CompiledClusterRule) {
	wrRules := m.RuleStore.GetMatchingRules(
		u,
		resourcePlural,
		configv1alpha1.OperationUpdate,
		apiGroup,
		apiVersion,
		isClusterScoped,
	)

	cwrRules := m.RuleStore.GetMatchingClusterRules(
		resourcePlural,
		configv1alpha1.OperationUpdate,
		apiGroup,
		apiVersion,
		isClusterScoped,
		nsLabels,
	)

	return wrRules, cwrRules
}

// isDuplicateContent checks if the sanitized content is identical to the last seen version.
// Returns true if content is duplicate (should skip), false if new content (should process).
func (m *Manager) isDuplicateContent(
	_ context.Context,
	sanitized *unstructured.Unstructured,
	id types.ResourceIdentifier,
) bool {
	// Initialize dedup map if needed
	m.lastSeenMu.Lock()
	if m.lastSeenHash == nil {
		m.lastSeenHash = make(map[string]uint64)
	}
	m.lastSeenMu.Unlock()

	// Compute content hash
	yaml, err := sanitize.MarshalToOrderedYAML(sanitized)
	if err != nil {
		// Can't compute hash - assume not duplicate to be safe
		return false
	}
	currentHash := xxhash.Sum64(yaml)

	// Resource key: fully-qualified identifier (types.ResourceIdentifier.Key).
	resourceKey := id.Key()

	// Check against last seen
	m.lastSeenMu.RLock()
	lastHash, exists := m.lastSeenHash[resourceKey]
	m.lastSeenMu.RUnlock()

	if exists && lastHash == currentHash {
		// Duplicate content
		return true
	}

	// New content - update tracking
	m.lastSeenMu.Lock()
	m.lastSeenHash[resourceKey] = currentHash
	m.lastSeenMu.Unlock()

	return false
}

// dynamicClientFromConfig builds a dynamic client from the controller's REST config.
// If m.dynamicClient is set (e.g. in tests) it is returned directly.
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

// desiredInformerScope computes, in a single pass over the resident watched-type tables,
// the informer surface every GitTarget wants: each GVR mapped to the namespaces to watch,
// where the empty string is a cluster-wide stream. A cluster-wide selection wins over any
// named namespace for the same GVR (the snapshot's collapse), so the informer scope and
// the snapshot agree. It is a pure read — the caller refreshes the tables once per
// reconcile — which keeps re-resolution off the per-type path the old per-GVR
// getNamespacesForGVR walked once for every requested GVR.
func (m *Manager) desiredInformerScope() map[GVR]map[string]struct{} {
	desired := map[GVR]map[string]struct{}{}
	clusterWide := map[GVR]struct{}{}
	for _, table := range m.residentWatchedTypeTables() {
		for _, wt := range table.Types {
			gvr := GVR{Group: wt.GVR.Group, Version: wt.GVR.Version, Resource: wt.GVR.Resource, Scope: wt.Scope}
			if desired[gvr] == nil {
				desired[gvr] = map[string]struct{}{}
			}
			if wt.ClusterWide() {
				clusterWide[gvr] = struct{}{}
				continue
			}
			for _, ns := range wt.SnapshotNamespaces() {
				desired[gvr][ns] = struct{}{}
			}
		}
	}
	// A cluster-wide selection for a GVR subsumes every named namespace for it.
	for gvr := range clusterWide {
		desired[gvr] = map[string]struct{}{"": {}}
	}
	return desired
}

// informersToStart returns the (GVR, namespace) informers in the desired scope that are
// not yet active.
func informersToStart(
	active map[GVR]map[string]context.CancelFunc,
	desired map[GVR]map[string]struct{},
) []gvrNamespace {
	var toStart []gvrNamespace
	for gvr, namespaces := range desired {
		for ns := range namespaces {
			if _, ok := active[gvr][ns]; !ok {
				toStart = append(toStart, gvrNamespace{gvr: gvr, ns: ns})
			}
		}
	}
	return toStart
}

// informersObsolete returns the active (GVR, namespace) informers no longer in the
// desired scope — a whole GVR removed, or just a namespace scope narrowed.
func informersObsolete(
	active map[GVR]map[string]context.CancelFunc,
	desired map[GVR]map[string]struct{},
) []gvrNamespace {
	var obsolete []gvrNamespace
	for gvr, activeNS := range active {
		want := desired[gvr]
		for ns := range activeNS {
			if _, ok := want[ns]; !ok {
				obsolete = append(obsolete, gvrNamespace{gvr: gvr, ns: ns})
			}
		}
	}
	return obsolete
}

// compareInformerScope diffs the desired informer scope against the active informers
// under informersMu, returning the (GVR, namespace) informers to start, then those to
// retire.
func (m *Manager) compareInformerScope(desired map[GVR]map[string]struct{}) ([]gvrNamespace, []gvrNamespace) {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()
	m.initializeInformerMaps()
	return informersToStart(m.activeInformers, desired), informersObsolete(m.activeInformers, desired)
}

// ReconcileForRuleChange reconciles the watch manager when rules change.
// Called by WatchRule/ClusterWatchRule controllers after rule modifications.
// Single-pod MVP: No debouncing needed since we control pod lifecycle.
// A newly installed CRD is picked up via the API-surface trigger informers and
// the periodic reconcile, both of which re-run this function.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	log := m.Log.WithName("reconcile")
	log.V(1).Info("Reconciling watch manager for rule change")

	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return err
	}

	// Re-resolve the resident watched-type tables (M10) now that the catalog is
	// fresh. This is gated on a rule-set change or catalog generation bump, so a
	// periodic reconcile with neither reuses the resolved tables. Every consumer
	// below (informer set, snapshot gather, plan hash) reads these tables.
	m.refreshWatchedTypeTables()

	// The informer surface is read from the resident watched-type tables in a single
	// pass (refreshed once above), then diffed against the active informers at
	// (GVR, namespace) granularity: toStart needs starting, obsolete needs retiring so
	// a narrowed scope (a namespace dropped, or a switch between namespaced and
	// cluster-wide) does not leave the old informer running alongside the new one.
	desired := m.desiredInformerScope()
	toStart, obsolete := m.compareInformerScope(desired)

	// Log current active count for debugging
	m.informersMu.Lock()
	activeCount := len(m.activeInformers)
	m.informersMu.Unlock()

	targets := m.snapshotTargetsNeedingDelivery()
	if len(toStart) == 0 && len(obsolete) == 0 && len(targets) == 0 {
		log.V(1).Info("No GVR changes detected, skipping reconciliation",
			"activeGVRs", activeCount)
		return nil
	}

	log.Info("GVR changes detected",
		"toStart", len(toStart),
		"obsolete", len(obsolete),
		"activeGVRs", activeCount)

	// Stop obsolete (GVR, namespace) informers.
	for _, gn := range obsolete {
		m.stopInformerNamespace(gn.gvr, gn.ns)
	}

	// Put affected GitTarget event streams into RECONCILING state BEFORE starting new
	// informers.  This ensures informer ADDED events fired during cache sync are buffered
	// rather than processed as N individual [CREATE] commits.
	m.beginReconciliationForTargets(targets, log)

	// Start the new (GVR, namespace) informers.
	if err := m.startInformerScope(ctx, toStart); err != nil {
		log.Error(err, "Failed to start informers for new GVRs")
		return err
	}

	// Clear deduplication cache for changed GVRs to prevent false duplicates
	m.clearDeduplicationCacheForGVRs(changedInformerGVRs(toStart, obsolete))

	// Run one streaming-snapshot resync per affected GitTarget so a single
	// "reconcile: sync N resources" commit is produced instead of N individual
	// [CREATE] commits from the informer ADDED events buffered above.
	deliveryErr := m.emitSnapshotForRuleChange(ctx, log, targets, "rule_change")

	// Transition streams back to LIVE_PROCESSING and flush buffered events.
	// startInformersForGVRs already waited for cache sync (WaitForCacheSync),
	// so all initial ADDED events are guaranteed to be buffered before this point.
	// The flushed events are no-ops at the git level because the snapshot batch
	// just wrote those files. Run this even when a target failed delivery so the
	// streams that did snapshot leave the buffering state.
	m.completeReconciliationForTargets(targets, log)

	if deliveryErr != nil {
		// A transient emit failure left at least one target pending. Surface it so
		// the controller requeues with backoff and retries promptly instead of
		// waiting for the next periodic reconcile.
		return deliveryErr
	}

	log.V(1).Info("Watch manager reconciliation completed",
		"startedInformers", len(toStart),
		"obsoleteInformers", len(obsolete))

	return nil
}

// changedInformerGVRs is the deduplicated set of GVRs touched by a reconcile (started or
// torn down), used to clear the content-dedup cache for exactly those types.
func changedInformerGVRs(toStart, obsolete []gvrNamespace) []GVR {
	seen := make(map[GVR]struct{}, len(toStart)+len(obsolete))
	out := make([]GVR, 0, len(toStart)+len(obsolete))
	for _, gn := range append(append([]gvrNamespace{}, toStart...), obsolete...) {
		if _, ok := seen[gn.gvr]; !ok {
			seen[gn.gvr] = struct{}{}
			out = append(out, gn.gvr)
		}
	}
	return out
}

// stopInformerNamespace cancels one (GVR, namespace) informer and drops it from the
// active set, removing the GVR entry once its last namespace stops. It is idempotent: a
// concurrent reconcile may have already stopped the same informer.
func (m *Manager) stopInformerNamespace(gvr GVR, ns string) {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()

	nsMap, exists := m.activeInformers[gvr]
	if !exists {
		return
	}
	if cancel, ok := nsMap[ns]; ok {
		cancel() // Stop the informer
		delete(nsMap, ns)
		m.Log.V(1).Info("Stopped informer",
			"group", gvr.Group,
			"version", gvr.Version,
			"resource", gvr.Resource,
			"namespace", ns)
	}
	if len(nsMap) == 0 {
		delete(m.activeInformers, gvr)
	}
}

// startInformerScope starts the given (GVR, namespace) informers that are not already
// running. It is the namespace-granular replacement for the old GVR-list start path:
// the caller passes the exact (GVR, namespace) pairs computed once from the desired
// scope, so there is no per-GVR namespace re-resolution here.
func (m *Manager) startInformerScope(ctx context.Context, toStart []gvrNamespace) error {
	if len(toStart) == 0 {
		return nil
	}
	log := m.Log.WithName("reconcile")

	cfg := m.restConfig()
	if cfg == nil {
		log.V(1).Info("No REST config available, skipping informer start")
		return nil // No config available (e.g., in unit tests)
	}

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return err
	}

	m.informersMu.Lock()
	defer m.informersMu.Unlock()

	m.initializeInformerMaps()

	// Re-check under the lock: a concurrent reconcile may have started some already.
	actual := make([]gvrNamespace, 0, len(toStart))
	for _, gn := range toStart {
		if m.activeInformers[gn.gvr] == nil {
			m.activeInformers[gn.gvr] = make(map[string]context.CancelFunc)
		}
		if _, exists := m.activeInformers[gn.gvr][gn.ns]; !exists {
			actual = append(actual, gn)
		}
	}
	if len(actual) == 0 {
		log.V(1).Info("All informers already running")
		return nil
	}

	log.Info("Starting new informers", "count", len(actual))
	return m.startCollectedInformers(ctx, client, actual)
}

// initializeInformerMaps ensures informer tracking maps are initialized.
func (m *Manager) initializeInformerMaps() {
	if m.activeInformers == nil {
		m.activeInformers = make(map[GVR]map[string]context.CancelFunc)
	}
	if m.informerFactories == nil {
		m.informerFactories = make(map[string]dynamicinformer.DynamicSharedInformerFactory)
	}
}

// gvrNamespace represents a GVR and its target namespace.
type gvrNamespace struct {
	gvr GVR
	ns  string
}

// startCollectedInformers starts all the collected informers.
func (m *Manager) startCollectedInformers(ctx context.Context, client dynamic.Interface, toStart []gvrNamespace) error {
	for _, item := range toStart {
		if err := m.startSingleInformer(ctx, client, item.gvr, item.ns); err != nil {
			return err
		}
	}

	m.Log.WithName("reconcile").V(1).Info("All informers started and synced")
	return nil
}

// startSingleInformer starts a single informer for a GVR in a specific namespace (or cluster-wide if ns is empty).
// Must be called with informersMu held.
func (m *Manager) startSingleInformer(ctx context.Context, client dynamic.Interface, gvr GVR, ns string) error {
	log := m.Log.WithName("reconcile").WithValues(
		"group", gvr.Group,
		"version", gvr.Version,
		"resource", gvr.Resource,
		"namespace", ns)

	// Get or create factory for this namespace
	factory, factoryExists := m.informerFactories[ns]
	if !factoryExists {
		if ns == "" {
			factory = dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
			log.V(1).Info("Created cluster-wide informer factory")
		} else {
			factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
				client, 0, ns, nil)
			log.V(1).Info("Created namespace-scoped informer factory", "namespace", ns)
		}
		m.informerFactories[ns] = factory
	}

	// Create informer for this GVR
	resource := schema.GroupVersionResource{
		Group:    gvr.Group,
		Version:  gvr.Version,
		Resource: gvr.Resource,
	}
	informer := factory.ForResource(resource).Informer()

	// Add event handlers BEFORE starting the factory
	m.addHandlers(informer, gvr)

	// Track the informer
	if m.activeInformers[gvr] == nil {
		m.activeInformers[gvr] = make(map[string]context.CancelFunc)
	}
	m.activeInformers[gvr][ns] = func() {
		// Cancel function would stop this specific informer
		// For now, we stop the entire factory when all informers for a namespace are removed
	}

	log.V(1).Info("Registered new informer")

	// Start the factory (idempotent - starts new informers if factory already running)
	factory.Start(ctx.Done())

	// ALWAYS wait for this specific informer to sync
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("failed to sync cache for %v in namespace %s", resource, ns)
	}
	log.V(1).Info("Informer cache synced")

	return nil
}

// clearDeduplicationCacheForGVRs removes hash entries for resources of the specified GVRs.
// This prevents false duplicate detection after informer changes.
func (m *Manager) clearDeduplicationCacheForGVRs(gvrs []GVR) {
	if len(gvrs) == 0 {
		return
	}

	m.lastSeenMu.Lock()
	defer m.lastSeenMu.Unlock()

	if m.lastSeenHash == nil {
		return
	}

	// Build set of GVRs being cleared
	gvrSet := make(map[GVR]struct{})
	for _, gvr := range gvrs {
		gvrSet[gvr] = struct{}{}
	}

	// Remove hash entries for resources of these GVRs
	// Key format: "group/version/resource/namespace/name" (group may be empty for core resources)
	for key := range m.lastSeenHash {
		if resourceMatchesGVRs(key, gvrSet) {
			delete(m.lastSeenHash, key)
		}
	}

	m.Log.V(1).Info("Cleared deduplication cache for GVR changes",
		"gvrCount", len(gvrs))
}

// resourceMatchesGVRs checks if a resource key matches any GVR in the set.
// Key format: "group/version/resource/namespace/name" or "group/version/resource/name" (cluster-scoped).
// Group may be empty for core resources, which yields a key like "/v1/secrets/ns/name".
func resourceMatchesGVRs(resourceKey string, gvrSet map[GVR]struct{}) bool {
	// Parse key components
	parts := splitResourceKey(resourceKey)
	if len(parts) < minResourceKeyParts {
		return false
	}

	resourceGVR := GVR{
		Group:    parts[0],
		Version:  parts[1],
		Resource: parts[2],
	}

	// Check if this resource's GVR is in the set (scope doesn't matter for dedup)
	for gvr := range gvrSet {
		if gvr.Group == resourceGVR.Group &&
			gvr.Version == resourceGVR.Version &&
			gvr.Resource == resourceGVR.Resource {
			return true
		}
	}

	return false
}

// splitResourceKey splits a resource key into components.
// Format: "group/version/resource/namespace/name" or "group/version/resource/name" (cluster-scoped).
func splitResourceKey(key string) []string {
	// types.ResourceIdentifier.Key() produces: "group/version/resource/namespace/name" (or ".../name" cluster-scoped).
	// We just need the first 3 parts for GVR matching, including an empty group for core resources.
	parts := make([]string, 0, resourceKeyCapacity)
	current := ""
	for _, ch := range key {
		if ch == '/' {
			parts = append(parts, current)
			current = ""
			if len(parts) >= minResourceKeyParts {
				// We have group/version/resource, that's enough
				break
			}
		} else {
			current += string(ch)
		}
	}
	return parts
}

// snapshotTargetsNeedingDelivery returns the GitTargets whose effective watch
// plan hash differs from the one last delivered — i.e. targets whose watched
// resource surface actually changed. Selection is purely per-target: global GVR
// churn for an unrelated target no longer drags every target into a snapshot.
// Targets whose hash is unchanged keep processing live events. See
// docs/design/gittarget-isolation-on-rule-change.md.
func (m *Manager) snapshotTargetsNeedingDelivery() []ruleSetSnapshotTarget {
	current := m.currentRuleSetSnapshots()
	currentKeys := make(map[string]struct{}, len(current))

	m.ruleSetSnapshotMu.Lock()
	defer m.ruleSetSnapshotMu.Unlock()
	m.ensureRuleSetSnapshotMapsLocked()

	var targets []ruleSetSnapshotTarget
	for _, target := range current {
		key := target.gitDest.Key()
		currentKeys[key] = struct{}{}
		if !target.hasEntries {
			continue
		}
		if lastDelivered, ok := m.lastDeliveredRuleSetHash[key]; ok && lastDelivered == target.hash {
			continue
		}
		m.pendingRuleSetHash[key] = target.hash
		targets = append(targets, target)
	}

	for key := range m.lastDeliveredRuleSetHash {
		if _, ok := currentKeys[key]; !ok {
			delete(m.lastDeliveredRuleSetHash, key)
			delete(m.pendingRuleSetHash, key)
		}
	}
	for key := range m.pendingRuleSetHash {
		if _, ok := currentKeys[key]; !ok {
			delete(m.pendingRuleSetHash, key)
		}
	}

	return targets
}

// currentRuleSetSnapshots returns, per GitTarget, a hash of that target's
// *effective watch plan* — what it actually watches after rule resolution and
// API discovery, not the literal rule text. The hash drives snapshot selection
// in snapshotTargetsNeedingDelivery: a target is re-snapshotted only when its
// plan hash changes.
//
// A plan is the set of (resolved GVR, namespace scope, union of operations)
// entries the target watches, plus the write destination (provider/branch/path).
// Deliberately excluded: source rule identity (namespace/name) and the raw
// apiGroups/apiVersions/resources patterns. Those are inputs to resolution, not
// the resolved surface — keeping them caused unrelated churn (a redundant
// duplicate rule) and missed real churn (a wildcard newly matching a CRD). See
// docs/design/gittarget-isolation-on-rule-change.md.
//
// Operations add up across rules: when two rules for the same target resolve to
// the same GVR, the entry's operation set is their union (see
// rulestore TestGetMatchingRules_OverlappingRulesUnionOperations). A target with
// rules that currently resolve to nothing is kept as an empty plan so transient
// discovery gaps do not look like rule removal.
//
// As of M10 the resolved surface is read from the resident watched-type tables
// rather than re-resolved here; watchPlanFromTable reconstructs the identical
// effective-plan entries (and hash) from a table, so snapshot selection is
// unchanged.
func (m *Manager) currentRuleSetSnapshots() []ruleSetSnapshotTarget {
	tables := m.allWatchedTypeTables()
	targets := make([]ruleSetSnapshotTarget, 0, len(tables))
	for _, table := range tables {
		p := watchPlanFromTable(table)
		targets = append(targets, ruleSetSnapshotTarget{
			gitDest:    table.GitDest,
			hash:       p.hash(),
			hasEntries: len(p.entries) > 0,
		})
	}
	return targets
}

func (m *Manager) ensureRuleSetSnapshotMapsLocked() {
	if m.lastDeliveredRuleSetHash == nil {
		m.lastDeliveredRuleSetHash = make(map[string]uint64)
	}
	if m.pendingRuleSetHash == nil {
		m.pendingRuleSetHash = make(map[string]uint64)
	}
}

func (m *Manager) markRuleSetSnapshotDelivered(target ruleSetSnapshotTarget) {
	key := target.gitDest.Key()
	m.ruleSetSnapshotMu.Lock()
	defer m.ruleSetSnapshotMu.Unlock()
	m.ensureRuleSetSnapshotMapsLocked()
	m.lastDeliveredRuleSetHash[key] = target.hash
	if pending, ok := m.pendingRuleSetHash[key]; ok && pending == target.hash {
		delete(m.pendingRuleSetHash, key)
	}
}

func (m *Manager) beginReconciliationForTargets(targets []ruleSetSnapshotTarget, log logr.Logger) {
	if m.EventRouter == nil {
		return
	}
	for _, target := range targets {
		m.EventRouter.BeginReconciliationForStream(target.gitDest)
		log.Info("Buffering live events for snapshot", "gitDest", target.gitDest.String())
	}
}

// emitSnapshotForRuleChange runs one streaming-snapshot resync for every affected
// GitTarget (M8), replacing the old repo+cluster two-snapshot handshake. Each target's
// complete watched set is gathered via the streaming-list watch and applied at the
// worker as a content-derived mark-and-sweep.
//
// A target is marked delivered and counted once its resync has been ENQUEUED at the
// worker (the rule-change resync is fire-and-forget). Delivery is deliberately NOT gated
// on the apply committing: doing so turned a slow or failed apply into an unbounded
// re-resync loop that re-gathered the whole snapshot every reconcile and starved the
// reconcile goroutine (see TriggerResyncForGitDest). A gather failure is returned
// synchronously so the caller requeues promptly — which matters for the per-pod restart
// gate that blocks on the reconcile counter reaching the new pod — and leaves the target
// pending for retry. Other targets in the batch are still attempted so one bad target
// cannot starve the rest.
func (m *Manager) emitSnapshotForRuleChange(
	ctx context.Context,
	log logr.Logger,
	targets []ruleSetSnapshotTarget,
	trigger string,
) error {
	if m.EventRouter == nil {
		log.Info("EventRouter not set, skipping snapshot emission")
		if len(targets) > 0 {
			m.snapshotEmitCount.Add(1)
		}
		return nil
	}
	log.Info("Resyncing affected GitTargets after rule change", "count", len(targets))
	emitted := false
	var errs []error
	for _, target := range targets {
		gitDest := target.gitDest
		// One content-derived, mark-and-sweep resync per target: gather the streaming
		// snapshot and enqueue it at the worker without blocking on the commit, so many
		// targets' commits proceed in parallel. A target whose GitTarget no longer exists
		// is skipped benignly (a rule may briefly outlive its GitTarget during deletion);
		// it must not poison the batch into a requeue storm. A target whose worker is not
		// yet live, or whose snapshot could not be gathered, is left pending and retried
		// by the next reconcile, exactly as the old two-snapshot path left it pending.
		if err := m.EventRouter.TriggerResyncForGitDest(ctx, gitDest); err != nil {
			if apierrors.IsNotFound(err) {
				log.V(1).Info("GitTarget no longer exists; skipping resync", "gitDest", gitDest.String())
				continue
			}
			log.Error(err, "failed to resync GitTarget for rule change", "gitDest", gitDest)
			errs = append(errs, fmt.Errorf("resync %s: %w", gitDest, err))
			continue
		}
		// Mark delivered + count the reconcile once the resync is ENQUEUED. Gating this
		// on the apply completing caused an unbounded re-resync loop (see
		// TriggerResyncForGitDest); a failed apply is recovered by steady-state events and
		// the next rule-set change, not by re-running the whole snapshot every reconcile.
		m.markRuleSetSnapshotDelivered(target)
		m.recordTargetReconcileCompleted(gitDest, trigger)
		emitted = true
	}
	if emitted {
		m.snapshotEmitCount.Add(1)
	}
	return errors.Join(errs...)
}

// recordTargetReconcileCompleted increments the per-GitTarget reconcile counter
// once its snapshot decision has been made and the resync ENQUEUED on the branch
// worker, tagged with the trigger that drove the pass. It deliberately does NOT wait
// for the commit (see emitSnapshotForRuleChange / TriggerResyncForGitDest), so the
// counter measures "the new pod gathered the snapshot and submitted it to the worker
// queue", not "the commit landed in git". On a controller restart the new pod's counter
// starts at 0, so a per-pod `{pod="<new>"} > 0` reading shows the new pod reached the
// submit step; paired with a drained BranchWorkerQueueDepth it shows the worker then
// processed everything it was handed. The apply itself can still fail (the worker logs
// it and the steady-state path / next rule change recovers), so a rollout gate built on
// this must accept "gathered + enqueued + queue drained", not "every snapshot commit
// succeeded". No-op until the counter is registered.
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

func (m *Manager) completeReconciliationForTargets(targets []ruleSetSnapshotTarget, log logr.Logger) {
	if m.EventRouter == nil {
		return
	}
	for _, target := range targets {
		m.EventRouter.CompleteReconciliationForStream(target.gitDest)
		log.Info("Flushing buffered events after snapshot", "gitDest", target.gitDest.String())
	}
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
