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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cespare/xxhash/v2"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
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
	// Deduplication: tracks last seen content hash per resource to skip status-only changes
	lastSeenMu   sync.RWMutex
	lastSeenHash map[string]uint64 // resourceKey → content hash (key uses types.ResourceIdentifier.Key)

	// Dynamic informer lifecycle management
	informersMu       sync.Mutex
	activeInformers   map[GVR]map[string]context.CancelFunc                   // GVR -> namespace -> cancel (empty string = cluster-wide)
	informerFactories map[string]dynamicinformer.DynamicSharedInformerFactory // namespace -> factory (empty string = cluster-wide)

	// CRD discovery retry tracking
	unavailableGVRsMu      sync.Mutex
	unavailableGVRs        map[GVR]int       // GVR -> retry count
	unavailableGVRsLastTry map[GVR]time.Time // GVR -> last retry time

	// dynamicClient overrides the config-built dynamic client when non-nil.
	// Used in tests to inject a fake client without a real REST config.
	dynamicClient dynamic.Interface

	// discoveryFilter, when non-nil, replaces FilterDiscoverableGVRs in
	// ReconcileForRuleChange. Used in tests to bypass the real discovery client
	// (which is unavailable without a real REST config). Production code leaves
	// it nil and uses the default discovery-backed implementation.
	discoveryFilter func(context.Context, []GVR) []GVR

	// snapshotEmitCount tracks how many times emitSnapshotForRuleChange has
	// actually emitted snapshots for at least one affected GitTarget. Useful for
	// tests to observe the snapshot-trigger contract and will be exposed as a
	// Prometheus metric later.
	snapshotEmitCount atomic.Int64

	// ruleSetSnapshotMu protects per-GitTarget snapshot delivery state.
	ruleSetSnapshotMu        sync.Mutex
	lastDeliveredRuleSetHash map[string]uint64
	pendingRuleSetHash       map[string]uint64
}

// SnapshotEmitCount returns the number of times the manager has emitted a
// snapshot for rule changes since process start.
func (m *Manager) SnapshotEmitCount() int64 {
	return m.snapshotEmitCount.Load()
}

type ruleSetSnapshotTarget struct {
	gitDest types.ResourceReference
	hash    uint64
}

const (
	heartbeatInterval         = 30 * time.Second
	periodicReconcileInterval = 30 * time.Second
	crdDiscoveryRetryInterval = 2 * time.Second
	crdDiscoveryMaxRetries    = 3
	maxRetryShift             = 8 // Max bit shift for exponential backoff to prevent overflow
	minResourceKeyParts       = 3
	resourceKeyCapacity       = 5
)

// Start begins the watch ingestion manager and blocks until context cancellation.
// Performs initial reconciliation then runs periodic discovery refresh.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (reconciliation-based)")
	defer log.Info("watch ingestion manager stopping")

	// Initialize active informers maps
	m.informersMu.Lock()
	if m.activeInformers == nil {
		m.activeInformers = make(map[GVR]map[string]context.CancelFunc)
	}
	if m.informerFactories == nil {
		m.informerFactories = make(map[string]dynamicinformer.DynamicSharedInformerFactory)
	}
	m.informersMu.Unlock()

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
			m.shutdown()
			return nil

		case <-periodicTicker.C:
			log.V(1).Info("Periodic reconciliation triggered")
			if err := m.ReconcileForRuleChange(ctx); err != nil {
				log.Error(err, "Periodic reconciliation failed")
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

// getNamespacesForGVR returns the list of namespaces to list for a given GVR.
// Returns empty slice for cluster-scoped resources or ClusterWatchRules (meaning cluster-wide list).
// Returns specific namespace(s) for namespaced resources from WatchRules.
func (m *Manager) getNamespacesForGVR(g GVR) []string {
	// Cluster-scoped resources always list cluster-wide
	if g.Scope == configv1alpha1.ResourceScopeCluster {
		return nil
	}

	// Collect namespaces from WatchRules
	namespacesSet := m.collectWatchRuleNamespaces(g)

	// Convert set to slice
	namespaces := make([]string, 0, len(namespacesSet))
	for ns := range namespacesSet {
		namespaces = append(namespaces, ns)
	}

	// Check ClusterWatchRules if no WatchRules matched
	if len(namespaces) == 0 && m.hasMatchingClusterWatchRule(g) {
		return nil // ClusterWatchRule with Namespaced scope - list cluster-wide
	}

	return namespaces
}

// collectWatchRuleNamespaces collects namespaces from WatchRules that match the given GVR.
func (m *Manager) collectWatchRuleNamespaces(g GVR) map[string]struct{} {
	wrRules := m.RuleStore.SnapshotWatchRules()
	namespacesSet := make(map[string]struct{})

	for _, rule := range wrRules {
		if m.compiledRuleMatchesGVR(rule.ResourceRules, g) {
			namespacesSet[rule.Source.Namespace] = struct{}{}
		}
	}

	return namespacesSet
}

// hasMatchingClusterWatchRule checks if any ClusterWatchRule with Namespaced scope matches the GVR.
func (m *Manager) hasMatchingClusterWatchRule(g GVR) bool {
	cwrRules := m.RuleStore.SnapshotClusterWatchRules()

	for _, cwrRule := range cwrRules {
		for _, rr := range cwrRule.Rules {
			if rr.Scope != configv1alpha1.ResourceScopeNamespaced {
				continue
			}
			if m.clusterResourceRuleMatchesGVR(rr, g) {
				return true
			}
		}
	}

	return false
}

// compiledRuleMatchesGVR checks if any CompiledResourceRule in the slice matches the given GVR.
func (m *Manager) compiledRuleMatchesGVR(resourceRules []rulestore.CompiledResourceRule, g GVR) bool {
	for _, rr := range resourceRules {
		if m.compiledResourceRuleMatchesGVR(rr, g) {
			return true
		}
	}
	return false
}

// compiledResourceRuleMatchesGVR checks if a CompiledResourceRule matches the given GVR.
func (m *Manager) compiledResourceRuleMatchesGVR(rr rulestore.CompiledResourceRule, g GVR) bool {
	if !m.matchesAPIGroups(rr.APIGroups, g.Group) {
		return false
	}
	if !m.matchesAPIVersions(rr.APIVersions, g.Version) {
		return false
	}
	return m.matchesResources(rr.Resources, g.Resource)
}

// clusterResourceRuleMatchesGVR checks if a CompiledClusterResourceRule matches the given GVR.
func (m *Manager) clusterResourceRuleMatchesGVR(rr rulestore.CompiledClusterResourceRule, g GVR) bool {
	if !m.matchesAPIGroups(rr.APIGroups, g.Group) {
		return false
	}
	if !m.matchesAPIVersions(rr.APIVersions, g.Version) {
		return false
	}
	return m.matchesResources(rr.Resources, g.Resource)
}

// matchesAPIGroups checks if the rule's API groups match the target group.
func (m *Manager) matchesAPIGroups(groups []string, targetGroup string) bool {
	if len(groups) == 0 {
		groups = []string{""}
	}
	for _, grp := range groups {
		if grp == "*" || grp == targetGroup {
			return true
		}
	}
	return false
}

// matchesAPIVersions checks if the rule's API versions match the target version.
func (m *Manager) matchesAPIVersions(versions []string, targetVersion string) bool {
	if len(versions) == 0 {
		versions = []string{"v1"}
	}
	for _, ver := range versions {
		if ver == "*" || ver == targetVersion {
			return true
		}
	}
	return false
}

// matchesResources checks if the rule's resources match the target resource.
func (m *Manager) matchesResources(resources []string, targetResource string) bool {
	for _, res := range resources {
		normalized := normalizeResource(res)
		if normalized == "*" || normalized == targetResource {
			return true
		}
	}
	return false
}

// GetClusterStateForGitDest returns cluster resources for a GitTarget.
// This is a synchronous service method called by EventRouter.
// It returns both resource identifiers (for diff logic) and sanitized full objects
// (keyed by ResourceIdentifier.Key()) for hydrating initial snapshot write events.
//
//nolint:gocognit,cyclop
func (m *Manager) GetClusterStateForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) ([]types.ResourceIdentifier, map[string]unstructured.Unstructured, error) {
	log := m.Log.WithValues("gitDest", gitDest.String())

	// Look up GitTarget to get path
	var gitTargetObj configv1alpha1.GitTarget
	if err := m.Client.Get(ctx, client.ObjectKey{
		Name:      gitDest.Name,
		Namespace: gitDest.Namespace,
	}, &gitTargetObj); err != nil {
		return nil, nil, fmt.Errorf("failed to get GitTarget: %w", err)
	}

	path := gitTargetObj.Spec.Path
	log = log.WithValues("path", path)

	// Get matching rules
	wrRules := m.RuleStore.SnapshotWatchRules()
	cwrRules := m.RuleStore.SnapshotClusterWatchRules()

	// Build a map from GVR to the namespaces that should be listed for it.
	// WatchRules are namespace-scoped: only list within rule.Source.Namespace.
	// ClusterWatchRules are cluster-wide: clusterWide=true overrides any namespace set.
	type gvrEntry struct {
		namespaces  map[string]struct{}
		clusterWide bool
	}
	gvrMap := make(map[schema.GroupVersionResource]*gvrEntry)

	for _, rule := range wrRules {
		if rule.GitTargetRef == gitTargetObj.Name &&
			rule.GitTargetNamespace == gitTargetObj.Namespace {
			ns := rule.Source.Namespace
			for _, rr := range rule.ResourceRules {
				for _, gvr := range m.gvrsFromResourceRule(rr) {
					entry := gvrMap[gvr]
					if entry == nil {
						entry = &gvrEntry{namespaces: make(map[string]struct{})}
						gvrMap[gvr] = entry
					}
					if !entry.clusterWide {
						entry.namespaces[ns] = struct{}{}
					}
				}
			}
		}
	}

	for _, cwrRule := range cwrRules {
		if cwrRule.GitTargetRef == gitTargetObj.Name &&
			cwrRule.GitTargetNamespace == gitTargetObj.Namespace {
			for _, gvr := range m.gvrsFromClusterRule(cwrRule) {
				entry := gvrMap[gvr]
				if entry == nil {
					entry = &gvrEntry{namespaces: make(map[string]struct{})}
					gvrMap[gvr] = entry
				}
				entry.clusterWide = true
			}
		}
	}

	// Query cluster for these GVRs
	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		return nil, nil, errors.New("no dynamic client available")
	}

	var resources []types.ResourceIdentifier
	objects := make(map[string]unstructured.Unstructured)
	for gvr, entry := range gvrMap {
		var namespaces []string
		if !entry.clusterWide {
			for ns := range entry.namespaces {
				namespaces = append(namespaces, ns)
			}
		}
		gvrResources, err := m.listResourcesForGVR(ctx, dc, gvr, namespaces, objects)
		if err != nil {
			log.Error(err, "Failed to list GVR", "gvr", gvr)
			continue
		}
		resources = append(resources, gvrResources...)
	}

	log.Info("Retrieved cluster state", "resourceCount", len(resources))
	return resources, objects, nil
}

// gvrsFromResourceRule returns the GVRs implied by a CompiledResourceRule.
func (m *Manager) gvrsFromResourceRule(rr rulestore.CompiledResourceRule) []schema.GroupVersionResource {
	groups := rr.APIGroups
	if len(groups) == 0 {
		groups = []string{""}
	}
	versions := rr.APIVersions
	if len(versions) == 0 {
		versions = []string{"v1"}
	}

	var out []schema.GroupVersionResource
	for _, group := range groups {
		if group == "*" {
			continue
		}
		for _, version := range versions {
			if version == "*" {
				continue
			}
			for _, resource := range rr.Resources {
				normalized := normalizeResource(resource)
				if normalized == "*" || shouldIgnoreResource(group, normalized) {
					continue
				}
				out = append(out, schema.GroupVersionResource{
					Group:    group,
					Version:  version,
					Resource: normalized,
				})
			}
		}
	}
	return out
}

// gvrsFromClusterRule returns the GVRs implied by a CompiledClusterRule.
//
//nolint:gocognit
func (m *Manager) gvrsFromClusterRule(cwrRule rulestore.CompiledClusterRule) []schema.GroupVersionResource {
	var out []schema.GroupVersionResource
	for _, rr := range cwrRule.Rules {
		groups := rr.APIGroups
		if len(groups) == 0 {
			groups = []string{""}
		}
		versions := rr.APIVersions
		if len(versions) == 0 {
			versions = []string{"v1"}
		}
		for _, group := range groups {
			if group == "*" {
				continue
			}
			for _, version := range versions {
				if version == "*" {
					continue
				}
				for _, resource := range rr.Resources {
					normalized := normalizeResource(resource)
					if normalized == "*" || shouldIgnoreResource(group, normalized) {
						continue
					}
					out = append(out, schema.GroupVersionResource{
						Group:    group,
						Version:  version,
						Resource: normalized,
					})
				}
			}
		}
	}
	return out
}

// listResourcesForGVR lists resources for a GVR, scoped to the given namespaces.
// If namespaces is empty, a cluster-wide list is performed (for ClusterWatchRules).
// Identifiers are returned; sanitized full objects are written into the provided objects map
// (keyed by ResourceIdentifier.Key()) for hydrating initial snapshot write events.
func (m *Manager) listResourcesForGVR(
	ctx context.Context,
	dc dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespaces []string,
	objects map[string]unstructured.Unstructured,
) ([]types.ResourceIdentifier, error) {
	if shouldIgnoreResource(gvr.Group, gvr.Resource) {
		return nil, nil
	}

	var allItems []unstructured.Unstructured

	if len(namespaces) == 0 {
		// ClusterWatchRule or cluster-scoped resource: list cluster-wide
		list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list %v: %w", gvr, err)
		}
		allItems = list.Items
	} else {
		// WatchRule: list only in the namespaces that have a matching rule
		for _, ns := range namespaces {
			list, err := dc.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list %v in namespace %s: %w", gvr, ns, err)
			}
			allItems = append(allItems, list.Items...)
		}
	}

	var resources []types.ResourceIdentifier
	for i := range allItems {
		obj := &allItems[i]
		id := types.NewResourceIdentifier(
			gvr.Group,
			gvr.Version,
			gvr.Resource,
			obj.GetNamespace(),
			obj.GetName(),
		)
		resources = append(resources, id)
		objects[id.Key()] = *sanitize.Sanitize(obj)
	}

	return resources, nil
}

// ReconcileForRuleChange reconciles the watch manager when rules change.
// Called by WatchRule/ClusterWatchRule controllers after rule modifications.
// Single-pod MVP: No debouncing needed since we control pod lifecycle.
// Implements immediate CRD discovery with retry logic for newly installed CRDs.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	log := m.Log.WithName("reconcile")
	log.V(1).Info("Reconciling watch manager for rule change")

	// Compute desired GVRs from current rules
	requestedGVRs := m.ComputeRequestedGVRs()
	filter := m.FilterDiscoverableGVRs
	if m.discoveryFilter != nil {
		filter = m.discoveryFilter
	}
	discoverableGVRs := filter(ctx, requestedGVRs)

	log.V(1).Info("Computed GVRs for reconciliation",
		"requested", len(requestedGVRs),
		"discoverable", len(discoverableGVRs))

	// Track newly unavailable GVRs and retry previously unavailable ones
	m.updateUnavailableGVRTracking(requestedGVRs, discoverableGVRs, log)

	// Determine what changed
	added, removed := m.compareGVRs(discoverableGVRs)

	// Log current active count for debugging
	m.informersMu.Lock()
	activeCount := len(m.activeInformers)
	m.informersMu.Unlock()

	targets := m.snapshotTargetsNeedingDelivery(len(added) > 0 || len(removed) > 0)
	if len(added) == 0 && len(removed) == 0 && len(targets) == 0 {
		log.V(1).Info("No GVR changes detected, skipping reconciliation",
			"activeGVRs", activeCount)
		return nil
	}

	log.Info("GVR changes detected",
		"added", len(added),
		"removed", len(removed),
		"activeGVRs", activeCount)

	// Stop informers for removed GVRs
	for _, gvr := range removed {
		m.stopInformer(gvr)
	}

	// Put affected GitTarget event streams into RECONCILING state BEFORE starting new
	// informers.  This ensures informer ADDED events fired during cache sync are buffered
	// rather than processed as N individual [CREATE] commits.
	m.beginReconciliationForTargets(targets, log)

	// Start informers for added GVRs
	if len(added) > 0 {
		if err := m.startInformersForGVRs(ctx, added); err != nil {
			log.Error(err, "Failed to start informers for new GVRs")
			return err
		}
	}

	// Clear deduplication cache for changed GVRs to prevent false duplicates
	m.clearDeduplicationCacheForGVRs(append(added, removed...))

	// Emit RequestClusterState for each affected GitTarget so that a single
	// "reconcile: sync N resources" commit is produced instead of N individual
	// [CREATE] commits from the informer ADDED events buffered above.
	m.emitSnapshotForRuleChange(ctx, log, targets)

	// Transition streams back to LIVE_PROCESSING and flush buffered events.
	// startInformersForGVRs already waited for cache sync (WaitForCacheSync),
	// so all initial ADDED events are guaranteed to be buffered before this point.
	// The flushed events are no-ops at the git level because the snapshot batch
	// just wrote those files.
	m.completeReconciliationForTargets(targets, log)

	log.V(1).Info("Watch manager reconciliation completed",
		"addedGVRs", len(added),
		"removedGVRs", len(removed))

	return nil
}

// updateUnavailableGVRTracking tracks GVRs that are requested but not discoverable.
// Schedules immediate retry for newly unavailable GVRs.
func (m *Manager) updateUnavailableGVRTracking(
	requested []GVR,
	discoverable []GVR,
	log logr.Logger,
) {
	m.unavailableGVRsMu.Lock()
	defer m.unavailableGVRsMu.Unlock()

	m.initializeUnavailableMaps()
	discoverableSet := m.buildDiscoverableSet(discoverable)
	newlyUnavailable := m.processRequestedGVRs(requested, discoverableSet, log)
	m.cleanupUnrequestedGVRs(requested)
	m.scheduleRetries(newlyUnavailable, log)
}

// initializeUnavailableMaps ensures unavailable tracking maps are initialized.
func (m *Manager) initializeUnavailableMaps() {
	if m.unavailableGVRs == nil {
		m.unavailableGVRs = make(map[GVR]int)
	}
	if m.unavailableGVRsLastTry == nil {
		m.unavailableGVRsLastTry = make(map[GVR]time.Time)
	}
}

// buildDiscoverableSet creates a set of discoverable GVRs for fast lookup.
func (m *Manager) buildDiscoverableSet(discoverable []GVR) map[GVR]bool {
	discoverableSet := make(map[GVR]bool)
	for _, gvr := range discoverable {
		discoverableSet[gvr] = true
	}
	return discoverableSet
}

// processRequestedGVRs checks each requested GVR and returns newly unavailable ones.
func (m *Manager) processRequestedGVRs(requested []GVR, discoverableSet map[GVR]bool, log logr.Logger) []GVR {
	var newlyUnavailable []GVR

	for _, gvr := range requested {
		if discoverableSet[gvr] {
			m.handleAvailableGVR(gvr, log)
		} else {
			if shouldRetry := m.handleUnavailableGVR(gvr, log); shouldRetry {
				newlyUnavailable = append(newlyUnavailable, gvr)
			}
		}
	}

	return newlyUnavailable
}

// handleAvailableGVR processes a GVR that became available.
func (m *Manager) handleAvailableGVR(gvr GVR, log logr.Logger) {
	if _, wasUnavailable := m.unavailableGVRs[gvr]; wasUnavailable {
		delete(m.unavailableGVRs, gvr)
		delete(m.unavailableGVRsLastTry, gvr)
		log.Info("GVR became available",
			"group", gvr.Group,
			"version", gvr.Version,
			"resource", gvr.Resource)
	}
}

// handleUnavailableGVR processes an unavailable GVR and returns whether to retry.
func (m *Manager) handleUnavailableGVR(gvr GVR, log logr.Logger) bool {
	retryCount, exists := m.unavailableGVRs[gvr]

	if !exists {
		return m.handleNewlyUnavailableGVR(gvr, log)
	}

	return m.handlePreviouslyUnavailableGVR(gvr, retryCount, log)
}

// handleNewlyUnavailableGVR processes a newly unavailable GVR.
func (m *Manager) handleNewlyUnavailableGVR(gvr GVR, log logr.Logger) bool {
	m.unavailableGVRs[gvr] = 0
	m.unavailableGVRsLastTry[gvr] = time.Now()
	log.Info("New unavailable GVR detected - scheduling retry",
		"group", gvr.Group,
		"version", gvr.Version,
		"resource", gvr.Resource)
	return true
}

// handlePreviouslyUnavailableGVR processes a previously unavailable GVR.
func (m *Manager) handlePreviouslyUnavailableGVR(gvr GVR, retryCount int, log logr.Logger) bool {
	if retryCount >= crdDiscoveryMaxRetries {
		return false
	}

	lastTry := m.unavailableGVRsLastTry[gvr]
	delay := m.calculateRetryDelay(retryCount)

	if time.Since(lastTry) < delay {
		return false
	}

	m.unavailableGVRs[gvr] = retryCount + 1
	m.unavailableGVRsLastTry[gvr] = time.Now()
	log.Info("Retrying unavailable GVR",
		"group", gvr.Group,
		"version", gvr.Version,
		"resource", gvr.Resource,
		"attempt", retryCount+1)
	return true
}

// calculateRetryDelay calculates exponential backoff delay with overflow protection.
func (m *Manager) calculateRetryDelay(retryCount int) time.Duration {
	// Cap shift to prevent overflow
	shiftAmount := retryCount
	if shiftAmount > maxRetryShift {
		shiftAmount = maxRetryShift
	}
	return crdDiscoveryRetryInterval * time.Duration(1<<shiftAmount)
}

// cleanupUnrequestedGVRs removes GVRs that are no longer requested.
func (m *Manager) cleanupUnrequestedGVRs(requested []GVR) {
	for gvr := range m.unavailableGVRs {
		if !m.isGVRRequested(gvr, requested) {
			delete(m.unavailableGVRs, gvr)
			delete(m.unavailableGVRsLastTry, gvr)
		}
	}
}

// isGVRRequested checks if a GVR is in the requested list.
func (m *Manager) isGVRRequested(gvr GVR, requested []GVR) bool {
	for _, req := range requested {
		if req == gvr {
			return true
		}
	}
	return false
}

// scheduleRetries schedules retry reconciliation for newly unavailable GVRs.
func (m *Manager) scheduleRetries(newlyUnavailable []GVR, log logr.Logger) {
	if len(newlyUnavailable) > 0 {
		ctxCopy := context.Background()
		go m.retryDiscoveryAfterDelay(ctxCopy, newlyUnavailable, log)
	}
}

// retryDiscoveryAfterDelay waits briefly then retries reconciliation for CRD discovery.
func (m *Manager) retryDiscoveryAfterDelay(ctx context.Context, gvrs []GVR, log logr.Logger) {
	// Small delay to allow API server to update discovery after CRD installation
	time.Sleep(crdDiscoveryRetryInterval)

	log.Info("Retrying reconciliation for unavailable GVRs",
		"count", len(gvrs),
		"delay", crdDiscoveryRetryInterval)

	if err := m.ReconcileForRuleChange(ctx); err != nil {
		log.Error(err, "Retry reconciliation failed")
	}
}

// compareGVRs returns (added, removed) GVRs compared to current active set.
// Now handles GVR+namespace combinations properly.
func (m *Manager) compareGVRs(desired []GVR) ([]GVR, []GVR) {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()

	// Initialize if needed
	if m.activeInformers == nil {
		m.activeInformers = make(map[GVR]map[string]context.CancelFunc)
	}

	// Build map of desired GVR -> namespaces
	desiredGVRNamespaces := m.buildDesiredGVRNamespaces(desired)

	// Find added and removed GVRs
	added := m.findAddedGVRs(desiredGVRNamespaces)
	removed := m.findRemovedGVRs(desiredGVRNamespaces)

	return added, removed
}

// buildDesiredGVRNamespaces constructs a map of desired GVR to their namespaces.
func (m *Manager) buildDesiredGVRNamespaces(desired []GVR) map[GVR]map[string]bool {
	desiredGVRNamespaces := make(map[GVR]map[string]bool)

	for _, gvr := range desired {
		namespaces := m.getNamespacesForGVRUnlocked(gvr)

		if desiredGVRNamespaces[gvr] == nil {
			desiredGVRNamespaces[gvr] = make(map[string]bool)
		}

		if len(namespaces) == 0 {
			// Cluster-wide
			desiredGVRNamespaces[gvr][""] = true
		} else {
			for _, ns := range namespaces {
				desiredGVRNamespaces[gvr][ns] = true
			}
		}
	}

	return desiredGVRNamespaces
}

// findAddedGVRs identifies GVRs that need informers added.
func (m *Manager) findAddedGVRs(desiredGVRNamespaces map[GVR]map[string]bool) []GVR {
	var added []GVR
	seenGVRs := make(map[GVR]bool)

	for gvr, desiredNS := range desiredGVRNamespaces {
		if m.hasNewNamespaces(gvr, desiredNS) && !seenGVRs[gvr] {
			added = append(added, gvr)
			seenGVRs[gvr] = true
		}
	}

	return added
}

// hasNewNamespaces checks if a GVR has new namespaces compared to active informers.
func (m *Manager) hasNewNamespaces(gvr GVR, desiredNS map[string]bool) bool {
	activeNS, gvrExists := m.activeInformers[gvr]

	if !gvrExists {
		return true
	}

	for ns := range desiredNS {
		if _, nsExists := activeNS[ns]; !nsExists {
			return true
		}
	}

	return false
}

// findRemovedGVRs identifies GVRs that should be removed.
func (m *Manager) findRemovedGVRs(desiredGVRNamespaces map[GVR]map[string]bool) []GVR {
	var removed []GVR

	for gvr := range m.activeInformers {
		if _, exists := desiredGVRNamespaces[gvr]; !exists {
			removed = append(removed, gvr)
		}
	}

	return removed
}

// getNamespacesForGVRUnlocked is like getNamespacesForGVR but assumes informersMu is already held.
func (m *Manager) getNamespacesForGVRUnlocked(g GVR) []string {
	// Temporarily unlock to call getNamespacesForGVR which doesn't need the lock
	m.informersMu.Unlock()
	defer m.informersMu.Lock()
	return m.getNamespacesForGVR(g)
}

// stopInformer cancels and removes all informers for a specific GVR (across all namespaces).
func (m *Manager) stopInformer(gvr GVR) {
	m.informersMu.Lock()
	defer m.informersMu.Unlock()

	if nsMap, exists := m.activeInformers[gvr]; exists {
		for ns, cancel := range nsMap {
			cancel() // Stop the informer
			m.Log.V(1).Info("Stopped informer",
				"group", gvr.Group,
				"version", gvr.Version,
				"resource", gvr.Resource,
				"namespace", ns)
		}
		delete(m.activeInformers, gvr)
	}
}

// startInformersForGVRs starts watching specific GVRs.
// Creates namespace-scoped factories for WatchRule GVRs and cluster-wide factory for ClusterWatchRule GVRs.
func (m *Manager) startInformersForGVRs(ctx context.Context, gvrs []GVR) error {
	log := m.Log.WithName("reconcile")
	log.V(1).Info("startInformersForGVRs called", "gvrCount", len(gvrs))

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

	toStart := m.collectInformersToStart(gvrs)

	if len(toStart) == 0 {
		log.V(1).Info("All informers already running")
		return nil
	}

	log.Info("Starting new informers", "count", len(toStart))
	return m.startCollectedInformers(ctx, client, toStart)
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

// collectInformersToStart identifies which informers need to be started.
func (m *Manager) collectInformersToStart(gvrs []GVR) []gvrNamespace {
	var toStart []gvrNamespace

	for _, gvr := range gvrs {
		namespaces := m.getNamespacesForGVR(gvr)

		if m.activeInformers[gvr] == nil {
			m.activeInformers[gvr] = make(map[string]context.CancelFunc)
		}

		if len(namespaces) == 0 {
			// Cluster-wide informer
			if _, exists := m.activeInformers[gvr][""]; !exists {
				toStart = append(toStart, gvrNamespace{gvr: gvr, ns: ""})
			}
		} else {
			// Namespace-scoped informers
			for _, ns := range namespaces {
				if _, exists := m.activeInformers[gvr][ns]; !exists {
					toStart = append(toStart, gvrNamespace{gvr: gvr, ns: ns})
				}
			}
		}
	}

	return toStart
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

func (m *Manager) snapshotTargetsNeedingDelivery(force bool) []ruleSetSnapshotTarget {
	current := m.currentRuleSetSnapshots()
	currentKeys := make(map[string]struct{}, len(current))

	m.ruleSetSnapshotMu.Lock()
	defer m.ruleSetSnapshotMu.Unlock()
	m.ensureRuleSetSnapshotMapsLocked()

	var targets []ruleSetSnapshotTarget
	for _, target := range current {
		key := target.gitDest.Key()
		currentKeys[key] = struct{}{}
		if !force {
			if lastDelivered, ok := m.lastDeliveredRuleSetHash[key]; ok && lastDelivered == target.hash {
				continue
			}
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

func (m *Manager) currentRuleSetSnapshots() []ruleSetSnapshotTarget {
	entriesByTarget := make(map[string][]string)
	refsByTarget := make(map[string]types.ResourceReference)

	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		ref := types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		key := ref.Key()
		refsByTarget[key] = ref
		entriesByTarget[key] = append(entriesByTarget[key], fmt.Sprintf(
			"watch|source=%s/%s|target=%s/%s|provider=%s/%s|branch=%q|path=%q|rules=%#v",
			rule.Source.Namespace,
			rule.Source.Name,
			rule.GitTargetNamespace,
			rule.GitTargetRef,
			rule.GitProviderNamespace,
			rule.GitProviderRef,
			rule.Branch,
			rule.Path,
			rule.ResourceRules,
		))
	}

	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		ref := types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		key := ref.Key()
		refsByTarget[key] = ref
		entriesByTarget[key] = append(entriesByTarget[key], fmt.Sprintf(
			"cluster|source=%s|target=%s/%s|provider=%s/%s|branch=%q|path=%q|rules=%#v",
			rule.Source.Name,
			rule.GitTargetNamespace,
			rule.GitTargetRef,
			rule.GitProviderNamespace,
			rule.GitProviderRef,
			rule.Branch,
			rule.Path,
			rule.Rules,
		))
	}

	keys := make([]string, 0, len(entriesByTarget))
	for key := range entriesByTarget {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	targets := make([]ruleSetSnapshotTarget, 0, len(keys))
	for _, key := range keys {
		entries := entriesByTarget[key]
		sort.Strings(entries)
		targets = append(targets, ruleSetSnapshotTarget{
			gitDest: refsByTarget[key],
			hash:    xxhash.Sum64String(strings.Join(entries, "\x00")),
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

// MaybeReplaySnapshot emits a pending rule-change snapshot once a FolderReconciler
// exists for gitDest. It is called by ReconcilerManager when a reconciler is
// created; ctx is the originating reconcile context so the replay is cancellable.
func (m *Manager) MaybeReplaySnapshot(ctx context.Context, gitDest types.ResourceReference) {
	if m == nil {
		return
	}

	key := gitDest.Key()
	m.ruleSetSnapshotMu.Lock()
	m.ensureRuleSetSnapshotMapsLocked()
	hash, pending := m.pendingRuleSetHash[key]
	lastDelivered := m.lastDeliveredRuleSetHash[key]
	m.ruleSetSnapshotMu.Unlock()

	if !pending || lastDelivered == hash || m.EventRouter == nil {
		return
	}

	target := ruleSetSnapshotTarget{gitDest: gitDest, hash: hash}
	log := m.Log.WithName("reconcile")
	m.EventRouter.BeginReconciliationForStream(gitDest)
	m.emitSnapshotForRuleChange(ctx, log, []ruleSetSnapshotTarget{target})
	m.EventRouter.CompleteReconciliationForStream(gitDest)
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

// emitSnapshotForRuleChange emits fresh repo and cluster state requests for every affected
// GitTarget so FolderReconciler diffs against current repository contents rather than a
// stale cached repo snapshot from an earlier reconcile.
func (m *Manager) emitSnapshotForRuleChange(
	ctx context.Context,
	log logr.Logger,
	targets []ruleSetSnapshotTarget,
) {
	if m.EventRouter == nil {
		log.Info("EventRouter not set, skipping snapshot emission")
		if len(targets) > 0 {
			m.snapshotEmitCount.Add(1)
		}
		return
	}
	log.Info("Emitting fresh repo and cluster state for affected GitTargets after rule change", "count", len(targets))
	emitted := false
	for _, target := range targets {
		gitDest := target.gitDest
		if m.EventRouter.ReconcilerManager == nil {
			log.V(1).Info("ReconcilerManager not set, leaving snapshot pending", "gitDest", gitDest.String())
			continue
		}
		reconciler, exists := m.EventRouter.ReconcilerManager.GetReconciler(gitDest)
		if !exists {
			log.V(1).Info("No reconciler registered, leaving snapshot pending", "gitDest", gitDest.String())
			continue
		}
		reconciler.ResetState()
		if err := m.EventRouter.ProcessControlEvent(ctx, events.ControlEvent{
			Type:    events.RequestRepoState,
			GitDest: gitDest,
		}); err != nil {
			log.Error(err, "failed to emit RequestRepoState for rule change", "gitDest", gitDest)
			continue
		}
		if err := m.EventRouter.ProcessControlEvent(ctx, events.ControlEvent{
			Type:    events.RequestClusterState,
			GitDest: gitDest,
		}); err != nil {
			log.Error(err, "failed to emit RequestClusterState for rule change", "gitDest", gitDest)
			continue
		}
		m.markRuleSetSnapshotDelivered(target)
		emitted = true
	}
	if emitted {
		m.snapshotEmitCount.Add(1)
	}
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
