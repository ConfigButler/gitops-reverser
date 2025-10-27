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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/cespare/xxhash/v2"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/correlation"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
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
	EventRouter *git.EventRouter
	// CorrelationStore enables webhook→watch username enrichment.
	CorrelationStore *correlation.Store

	// Deduplication: tracks last seen content hash per resource to skip status-only changes
	lastSeenMu   sync.RWMutex
	lastSeenHash map[string]uint64 // resourceKey → content hash

	// Dynamic informer lifecycle management
	informersMu       sync.Mutex
	activeInformers   map[GVR]map[string]context.CancelFunc                   // GVR -> namespace -> cancel (empty string = cluster-wide)
	informerFactories map[string]dynamicinformer.DynamicSharedInformerFactory // namespace -> factory (empty string = cluster-wide)
}

const (
	heartbeatInterval         = 30 * time.Second
	periodicReconcileInterval = 30 * time.Second
	minResourceKeyParts       = 3
	resourceKeyCapacity       = 5
	cacheWarmupDelay          = 500 * time.Millisecond
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
			m.Log.Info("Shutdown: stopped informer",
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

// enqueueMatches pushes sanitized events to branch workers via event router.
// It attempts to enrich events with webhook-captured username via correlation store.
// Implements deduplication: skips enqueuing if sanitized content is identical to last seen.
func (m *Manager) enqueueMatches(
	ctx context.Context,
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	watchRules []rulestore.CompiledRule,
	clusterRules []rulestore.CompiledClusterRule,
) {
	// Check for duplicate content (status-only changes)
	if m.isDuplicateContent(ctx, sanitized, id) {
		m.Log.V(1).Info("Skipping duplicate sanitized content (likely status-only change)",
			"identifier", id.String())
		metrics.WatchDuplicatesSkippedTotal.Add(ctx, 1)
		return
	}

	// Attempt correlation enrichment
	userInfo := m.tryEnrichFromCorrelation(ctx, sanitized, id, "UPDATE")

	// WatchRule matches - route to workers
	for _, rule := range watchRules {
		ev := git.SimplifiedEvent{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  "UPDATE",
			UserInfo:   userInfo,
			BaseFolder: rule.BaseFolder,
		}

		if err := m.EventRouter.RouteEvent(
			rule.GitRepoConfigRef,
			rule.GitRepoConfigNamespace,
			rule.Branch,
			ev,
		); err != nil {
			m.Log.V(1).Info("Failed to route event", "error", err)
		}
	}

	// ClusterWatchRule matches - route to workers
	for _, cr := range clusterRules {
		ev := git.SimplifiedEvent{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  "UPDATE",
			UserInfo:   userInfo,
			BaseFolder: cr.BaseFolder,
		}

		if err := m.EventRouter.RouteEvent(
			cr.GitRepoConfigRef,
			cr.GitRepoConfigNamespace,
			cr.Branch,
			ev,
		); err != nil {
			m.Log.V(1).Info("Failed to route event", "error", err)
		}
	}
}

// isDuplicateContent checks if the sanitized content is identical to the last seen version.
// Returns true if content is duplicate (should skip), false if new content (should process).
func (m *Manager) isDuplicateContent(
	_ context.Context,
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
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

	// Resource key: namespace/name (or just name for cluster-scoped)
	resourceKey := id.String()

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

// tryEnrichFromCorrelation attempts to enrich an event with username from the correlation store.
func (m *Manager) tryEnrichFromCorrelation(
	ctx context.Context,
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	operation string,
) eventqueue.UserInfo {
	userInfo := eventqueue.UserInfo{} // default: no username

	if m.CorrelationStore == nil {
		return userInfo
	}

	log := m.Log.WithName("correlation")
	sanitizedYAML, err := sanitize.MarshalToOrderedYAML(sanitized)
	if err != nil {
		log.Error(err, "Failed to marshal for correlation", "identifier", id.String())
		return userInfo
	}

	key := correlation.GenerateKey(id, operation, sanitizedYAML)
	entry, found := m.CorrelationStore.GetAndDelete(key)
	if !found {
		metrics.EnrichMissesTotal.Add(ctx, 1)
		log.V(1).Info("No correlation match", "identifier", id.String(), "key", key)
		return userInfo
	}

	userInfo.Username = entry.Username
	metrics.EnrichHitsTotal.Add(ctx, 1)
	log.V(1).Info("Enriched with username",
		"identifier", id.String(),
		"username", entry.Username,
		"key", key)
	return userInfo
}

// seedSelectedResources performs a one-time List across all discoverable GVRs derived from active rules,
// sanitizes objects, matches rules, and enqueues UPDATE events to bootstrap the repository state.
// Orphan detection happens via SEED_SYNC control events per branch.
func (m *Manager) seedSelectedResources(ctx context.Context) {
	log := m.Log.WithName("seed")
	log.Info("starting initial seed listing")

	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		// Reason already logged.
		return
	}

	discoverable := m.discoverableGVRs(ctx)
	if len(discoverable) == 0 {
		log.Info("no discoverable GVRs to seed")
		return
	}

	// Track unique (repo, branch) combinations seen during seed to emit SEED_SYNC per branch
	branchKeys := make(map[git.BranchKey]struct{})

	for _, g := range discoverable {
		m.seedListAndProcess(ctx, dc, g, branchKeys)
	}

	m.emitSeedSyncControls(branchKeys)

	log.Info("seed listing completed", "gvrCount", len(discoverable), "branchKeys", len(branchKeys))
}

// dynamicClientFromConfig builds a dynamic client from the controller's REST config.
func (m *Manager) dynamicClientFromConfig(log logr.Logger) dynamic.Interface {
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

// discoverableGVRs returns the filtered list of GVRs to seed.
func (m *Manager) discoverableGVRs(ctx context.Context) []GVR {
	requested := m.ComputeRequestedGVRs()
	return m.FilterDiscoverableGVRs(ctx, requested)
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

// seedListAndProcess lists objects for a GVR and processes them into enqueue operations.
// For namespaced GVRs from WatchRules, only lists resources in the rule's namespace.
// For cluster-scoped GVRs or ClusterWatchRule GVRs, lists cluster-wide.
func (m *Manager) seedListAndProcess(
	ctx context.Context,
	dc dynamic.Interface,
	g GVR,
	branchKeys map[git.BranchKey]struct{},
) {
	log := m.Log.WithName("seed").
		WithValues("group", g.Group, "version", g.Version, "resource", g.Resource, "scope", g.Scope)

	res := schema.GroupVersionResource{Group: g.Group, Version: g.Version, Resource: g.Resource}

	// Determine which namespaces to list based on scope and source
	namespacesToList := m.getNamespacesForGVR(g)

	if len(namespacesToList) == 0 {
		// Cluster-scoped resource or ClusterWatchRule with namespaced resources (all namespaces)
		list, err := dc.Resource(res).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Error(err, "seed list failed (cluster-wide)")
			return
		}

		metrics.ObjectsScannedTotal.Add(ctx, int64(len(list.Items)))
		for i := range list.Items {
			m.processListedObject(ctx, &list.Items[i], g, branchKeys)
		}
	} else {
		// Namespaced resource from WatchRule(s) - list per namespace
		totalItems := 0
		for _, ns := range namespacesToList {
			list, err := dc.Resource(res).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				log.Error(err, "seed list failed", "namespace", ns)
				continue
			}

			totalItems += len(list.Items)
			for i := range list.Items {
				m.processListedObject(ctx, &list.Items[i], g, branchKeys)
			}
		}
		metrics.ObjectsScannedTotal.Add(ctx, int64(totalItems))
		log.V(1).Info("Seeded namespaced resources", "namespaces", len(namespacesToList), "totalItems", totalItems)
	}
}

// processListedObject evaluates rules, tracks branch keys, sanitizes, and enqueues events for one item.
func (m *Manager) processListedObject(
	ctx context.Context,
	u *unstructured.Unstructured,
	g GVR,
	branchKeys map[git.BranchKey]struct{},
) {
	id := itypes.NewResourceIdentifier(g.Group, g.Version, g.Resource, u.GetNamespace(), u.GetName())

	var nsLabels map[string]string
	if ns := id.Namespace; ns != "" {
		nsLabels = m.getNamespaceLabels(ctx, ns)
	}
	isClusterScoped := id.IsClusterScoped()

	wrRules, cwrRules := m.matchRules(u, g.Resource, g.Group, g.Version, isClusterScoped, nsLabels)
	if len(wrRules) == 0 && len(cwrRules) == 0 {
		return
	}

	// Track (repo, branch) keys for SEED_SYNC control emission (orphan detection) after seed
	for _, r := range wrRules {
		branchKeys[git.BranchKey{
			RepoNamespace: r.GitRepoConfigNamespace,
			RepoName:      r.GitRepoConfigRef,
			Branch:        r.Branch,
		}] = struct{}{}
	}
	for _, cr := range cwrRules {
		branchKeys[git.BranchKey{
			RepoNamespace: cr.GitRepoConfigNamespace,
			RepoName:      cr.GitRepoConfigRef,
			Branch:        cr.Branch,
		}] = struct{}{}
	}

	sanitized := sanitize.Sanitize(u)
	m.enqueueMatches(ctx, sanitized, id, wrRules, cwrRules)

	enq := int64(len(wrRules) + len(cwrRules))
	if enq > 0 {
		metrics.EventsProcessedTotal.Add(ctx, enq)
		metrics.GitCommitQueueSize.Add(ctx, enq)
	}
}

// emitSeedSyncControls enqueues one SEED_SYNC control event per (repo, branch) combination.
// Each worker handles orphan detection for all baseFolders it manages.
func (m *Manager) emitSeedSyncControls(branchKeys map[git.BranchKey]struct{}) {
	for key := range branchKeys {
		ev := git.SimplifiedEvent{
			Operation:  "SEED_SYNC",
			BaseFolder: "", // Empty = all baseFolders for this branch
		}

		if err := m.EventRouter.RouteEvent(
			key.RepoName,
			key.RepoNamespace,
			key.Branch,
			ev,
		); err != nil {
			m.Log.Error(err, "Failed to route SEED_SYNC", "key", key.String())
		}
	}
}

// ReconcileForRuleChange reconciles the watch manager when rules change.
// Called by WatchRule/ClusterWatchRule controllers after rule modifications.
// Single-pod MVP: No debouncing needed since we control pod lifecycle.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	log := m.Log.WithName("reconcile")
	log.Info("Reconciling watch manager for rule change")

	// Compute desired GVRs from current rules
	requestedGVRs := m.ComputeRequestedGVRs()
	discoverableGVRs := m.FilterDiscoverableGVRs(ctx, requestedGVRs)

	log.Info("Computed GVRs for reconciliation",
		"requested", len(requestedGVRs),
		"discoverable", len(discoverableGVRs))

	// Determine what changed
	added, removed := m.compareGVRs(discoverableGVRs)

	// Log current active count for debugging
	m.informersMu.Lock()
	activeCount := len(m.activeInformers)
	m.informersMu.Unlock()

	if len(added) == 0 && len(removed) == 0 {
		log.Info("No GVR changes detected, skipping reconciliation",
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

	// Start informers for added GVRs
	if len(added) > 0 {
		if err := m.startInformersForGVRs(ctx, added); err != nil {
			log.Error(err, "Failed to start informers for new GVRs")
			return err
		}
	}

	// Clear deduplication cache for changed GVRs to prevent false duplicates
	m.clearDeduplicationCacheForGVRs(append(added, removed...))

	// Trigger re-seed to sync Git with new state
	// Run in background to avoid blocking controller
	go m.seedSelectedResources(ctx)

	log.Info("Watch manager reconciliation completed",
		"addedGVRs", len(added),
		"removedGVRs", len(removed))

	return nil
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
			m.Log.Info("Stopped informer",
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
	log.Info("startInformersForGVRs called", "gvrCount", len(gvrs))

	cfg := m.restConfig()
	if cfg == nil {
		log.Info("No REST config available, skipping informer start")
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
		log.Info("All informers already running")
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

	m.Log.WithName("reconcile").Info("All informers started and synced")
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
	factory, exists := m.informerFactories[ns]
	if !exists {
		if ns == "" {
			factory = dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
			log.Info("Created cluster-wide informer factory")
		} else {
			factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
				client, 0, ns, nil)
			log.Info("Created namespace-scoped informer factory", "namespace", ns)
		}
		m.informerFactories[ns] = factory

		// Start the factory
		factory.Start(ctx.Done())
	}

	// Create informer for this GVR
	resource := schema.GroupVersionResource{
		Group:    gvr.Group,
		Version:  gvr.Version,
		Resource: gvr.Resource,
	}
	informer := factory.ForResource(resource).Informer()

	// Add event handlers
	m.addHandlers(informer, gvr)

	// Track the informer
	if m.activeInformers[gvr] == nil {
		m.activeInformers[gvr] = make(map[string]context.CancelFunc)
	}
	m.activeInformers[gvr][ns] = func() {
		// Cancel function would stop this specific informer
		// For now, we stop the entire factory when all informers for a namespace are removed
	}

	log.Info("Registered new informer")

	// Wait for cache sync for this specific informer
	if !exists {
		// New factory - wait for all informers in it to sync
		log.Info("Waiting for factory cache sync")
		synced := factory.WaitForCacheSync(ctx.Done())
		syncCount := 0
		for _, isSynced := range synced {
			if isSynced {
				syncCount++
			}
		}
		log.Info("Factory cache synced", "syncedCount", syncCount)
	}

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
	// Key format: "group/version/resource/namespace/name"
	for key := range m.lastSeenHash {
		if resourceMatchesGVRs(key, gvrSet) {
			delete(m.lastSeenHash, key)
		}
	}

	m.Log.V(1).Info("Cleared deduplication cache for GVR changes",
		"gvrCount", len(gvrs))
}

// resourceMatchesGVRs checks if a resource key matches any GVR in the set.
// Key format: "group/version/resource/namespace/name".
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
	// ResourceIdentifier.String() produces: "group/version/resource/namespace/name"
	// We just need the first 3 parts for GVR matching.
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

// SetupWithManager is a placeholder to enable kubebuilder RBAC marker scanning.
// The Manager is manually added to the controller-runtime manager in main.go as a Runnable,
// but this method allows kubebuilder's controller-gen to discover and process the RBAC markers.
func (m *Manager) SetupWithManager(mgr ctrl.Manager) error {
	// No actual setup needed - Manager is added manually in cmd/main.go
	// This method exists solely for kubebuilder RBAC marker scanning
	_ = mgr // Unused but required for signature
	return nil
}
