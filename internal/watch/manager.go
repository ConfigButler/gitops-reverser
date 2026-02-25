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
	"sync"
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
	"github.com/ConfigButler/gitops-reverser/internal/correlation"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
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
	// CorrelationStore enables webhook→watch username enrichment.
	CorrelationStore *correlation.Store

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
	id types.ResourceIdentifier,
	watchRules []rulestore.CompiledRule,
	clusterRules []rulestore.CompiledClusterRule,
) {
	// Check for duplicate content (status-only changes)
	if m.isDuplicateContent(ctx, sanitized, id) {
		m.Log.V(1).Info("Skipping duplicate sanitized content (likely status-only change)",
			"identifier", id.String())
		telemetry.WatchDuplicatesSkippedTotal.Add(ctx, 1)
		return
	}

	// Attempt correlation enrichment
	userInfo := m.tryEnrichFromCorrelation(ctx, sanitized, id, "UPDATE")

	// WatchRule matches - route to GitDestinationEventStreams
	for _, rule := range watchRules {
		ev := git.Event{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  "UPDATE",
			UserInfo:   userInfo,
			Path:       rule.Path,
		}

		gitDest := types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)

		// Route to GitTargetEventStream for buffering and deduplication
		if err := m.EventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			m.Log.Error(err, "Failed to route event to GitTargetEventStream - dropping event",
				"gitDest", gitDest.String(),
				"resource", id.String())
		}
	}

	// ClusterWatchRule matches - route to GitTargetEventStreams
	for _, cr := range clusterRules {
		ev := git.Event{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  "UPDATE",
			UserInfo:   userInfo,
			Path:       cr.Path,
		}

		gitDest := types.NewResourceReference(cr.GitTargetRef, cr.GitTargetNamespace)

		// Route to GitTargetEventStream for buffering and deduplication
		if err := m.EventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			m.Log.Error(err, "Failed to route event to GitTargetEventStream - dropping event",
				"gitDest", gitDest.String(),
				"resource", id.String())
		}
	}
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

// tryEnrichFromCorrelation attempts to enrich an event with username from the correlation store.
func (m *Manager) tryEnrichFromCorrelation(
	ctx context.Context,
	sanitized *unstructured.Unstructured,
	id types.ResourceIdentifier,
	operation string,
) git.UserInfo {
	userInfo := git.UserInfo{} // default: no username

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
	entry, found := m.CorrelationStore.Get(key)
	if !found {
		telemetry.EnrichMissesTotal.Add(ctx, 1)
		log.V(1).Info("No correlation match", "identifier", id.String(), "key", key)
		return userInfo
	}

	userInfo.Username = entry.Username
	telemetry.EnrichHitsTotal.Add(ctx, 1)
	log.V(1).Info("Enriched with username",
		"identifier", id.String(),
		"username", entry.Username,
		"key", key)
	return userInfo
}

// seedSelectedResources performs a one-time List across all discoverable GVRs derived from active rules,
// sanitizes objects, matches rules, and enqueues UPDATE events to bootstrap the repository state.
// Orphan detection is now handled by FolderReconciler comparing cluster vs git state.
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

	for _, g := range discoverable {
		m.seedListAndProcess(ctx, dc, g)
	}

	log.Info("seed listing completed", "gvrCount", len(discoverable))
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

		telemetry.ObjectsScannedTotal.Add(ctx, int64(len(list.Items)))
		for i := range list.Items {
			m.processListedObject(ctx, &list.Items[i], g)
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
				m.processListedObject(ctx, &list.Items[i], g)
			}
		}
		telemetry.ObjectsScannedTotal.Add(ctx, int64(totalItems))
		log.V(1).Info("Seeded namespaced resources", "namespaces", len(namespacesToList), "totalItems", totalItems)
	}
}

// processListedObject evaluates rules, sanitizes, and enqueues events for one item.
func (m *Manager) processListedObject(
	ctx context.Context,
	u *unstructured.Unstructured,
	g GVR,
) {
	if shouldIgnoreResource(g.Group, g.Resource) {
		m.Log.V(1).Info("Skipping seeded resource due to safety filter",
			"group", g.Group, "version", g.Version, "resource", g.Resource)
		return
	}

	id := types.NewResourceIdentifier(g.Group, g.Version, g.Resource, u.GetNamespace(), u.GetName())

	var nsLabels map[string]string
	if ns := id.Namespace; ns != "" {
		nsLabels = m.getNamespaceLabels(ctx, ns)
	}
	isClusterScoped := id.IsClusterScoped()

	wrRules, cwrRules := m.matchRules(u, g.Resource, g.Group, g.Version, isClusterScoped, nsLabels)
	if len(wrRules) == 0 && len(cwrRules) == 0 {
		return
	}

	sanitized := sanitize.Sanitize(u)
	m.enqueueMatches(ctx, sanitized, id, wrRules, cwrRules)

	enq := int64(len(wrRules) + len(cwrRules))
	if enq > 0 {
		telemetry.EventsProcessedTotal.Add(ctx, enq)
		telemetry.GitCommitQueueSize.Add(ctx, enq)
	}
}

// GetClusterStateForGitDest returns cluster resources for a GitTarget.
// This is a synchronous service method called by EventRouter.
func (m *Manager) GetClusterStateForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) ([]types.ResourceIdentifier, error) {
	log := m.Log.WithValues("gitDest", gitDest.String())

	// Look up GitTarget to get path
	var gitTargetObj configv1alpha1.GitTarget
	if err := m.Client.Get(ctx, client.ObjectKey{
		Name:      gitDest.Name,
		Namespace: gitDest.Namespace,
	}, &gitTargetObj); err != nil {
		return nil, fmt.Errorf("failed to get GitTarget: %w", err)
	}

	path := gitTargetObj.Spec.Path
	log = log.WithValues("path", path)

	// Get matching rules
	wrRules := m.RuleStore.SnapshotWatchRules()
	cwrRules := m.RuleStore.SnapshotClusterWatchRules()

	// Build GVR set from rules that reference this GitTarget
	gvrSet := make(map[schema.GroupVersionResource]struct{})

	for _, rule := range wrRules {
		if rule.GitTargetRef == gitTargetObj.Name &&
			rule.GitTargetNamespace == gitTargetObj.Namespace {
			for _, rr := range rule.ResourceRules {
				m.addGVRsFromResourceRule(rr, gvrSet)
			}
		}
	}

	for _, cwrRule := range cwrRules {
		if cwrRule.GitTargetRef == gitTargetObj.Name &&
			cwrRule.GitTargetNamespace == gitTargetObj.Namespace {
			for _, rr := range cwrRule.Rules {
				m.addGVRsFromClusterResourceRule(rr, gvrSet)
			}
		}
	}

	// Query cluster for these GVRs
	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		return nil, errors.New("no dynamic client available")
	}

	var resources []types.ResourceIdentifier
	for gvr := range gvrSet {
		gvrResources, err := m.listResourcesForGVR(ctx, dc, gvr, &gitTargetObj)
		if err != nil {
			log.Error(err, "Failed to list GVR", "gvr", gvr)
			continue
		}
		resources = append(resources, gvrResources...)
	}

	log.Info("Retrieved cluster state", "resourceCount", len(resources))
	return resources, nil
}

// addGVRsFromResourceRule adds GVRs from a CompiledResourceRule to the set.
func (m *Manager) addGVRsFromResourceRule(
	rr rulestore.CompiledResourceRule,
	gvrSet map[schema.GroupVersionResource]struct{},
) {
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
			continue // Skip wildcards for now
		}
		for _, version := range versions {
			if version == "*" {
				continue // Skip wildcards
			}
			for _, resource := range rr.Resources {
				normalized := normalizeResource(resource)
				if normalized == "*" || shouldIgnoreResource(group, normalized) {
					continue // Skip wildcards
				}
				gvr := schema.GroupVersionResource{
					Group:    group,
					Version:  version,
					Resource: normalized,
				}
				gvrSet[gvr] = struct{}{}
			}
		}
	}
}

// addGVRsFromClusterResourceRule adds GVRs from a CompiledClusterResourceRule to the set.
func (m *Manager) addGVRsFromClusterResourceRule(
	rr rulestore.CompiledClusterResourceRule,
	gvrSet map[schema.GroupVersionResource]struct{},
) {
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
			continue // Skip wildcards for now
		}
		for _, version := range versions {
			if version == "*" {
				continue // Skip wildcards
			}
			for _, resource := range rr.Resources {
				normalized := normalizeResource(resource)
				if normalized == "*" || shouldIgnoreResource(group, normalized) {
					continue // Skip wildcards
				}
				gvr := schema.GroupVersionResource{
					Group:    group,
					Version:  version,
					Resource: normalized,
				}
				gvrSet[gvr] = struct{}{}
			}
		}
	}
}

// listResourcesForGVR lists all resources for a specific GVR that match the GitTarget criteria.
func (m *Manager) listResourcesForGVR(
	ctx context.Context,
	dc dynamic.Interface,
	gvr schema.GroupVersionResource,
	gitTarget *configv1alpha1.GitTarget,
) ([]types.ResourceIdentifier, error) {
	if shouldIgnoreResource(gvr.Group, gvr.Resource) {
		return nil, nil
	}

	var resources []types.ResourceIdentifier

	// List resources (cluster-wide for now, namespace filtering would go here)
	list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list %v: %w", gvr, err)
	}

	// Convert to ResourceIdentifiers, filtering by labels/selectors if needed
	for i := range list.Items {
		obj := &list.Items[i]

		// Check if object matches GitTarget criteria
		if !m.objectMatchesGitTarget(obj, gitTarget) {
			continue
		}

		id := types.NewResourceIdentifier(
			gvr.Group,
			gvr.Version,
			gvr.Resource,
			obj.GetNamespace(),
			obj.GetName(),
		)
		resources = append(resources, id)
	}

	return resources, nil
}

// objectMatchesGitTarget checks if an object should be included for a GitTarget.
func (m *Manager) objectMatchesGitTarget(
	_ *unstructured.Unstructured,
	_ *configv1alpha1.GitTarget,
) bool {
	// For now, simple match - in the future could filter by namespace, labels, etc.
	// based on the rules that reference this GitTarget
	return true
}

// ReconcileForRuleChange reconciles the watch manager when rules change.
// Called by WatchRule/ClusterWatchRule controllers after rule modifications.
// Single-pod MVP: No debouncing needed since we control pod lifecycle.
// Implements immediate CRD discovery with retry logic for newly installed CRDs.
func (m *Manager) ReconcileForRuleChange(ctx context.Context) error {
	log := m.Log.WithName("reconcile")
	log.Info("Reconciling watch manager for rule change")

	// Compute desired GVRs from current rules
	requestedGVRs := m.ComputeRequestedGVRs()
	discoverableGVRs := m.FilterDiscoverableGVRs(ctx, requestedGVRs)

	log.Info("Computed GVRs for reconciliation",
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
	factory, factoryExists := m.informerFactories[ns]
	if !factoryExists {
		if ns == "" {
			factory = dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
			log.Info("Created cluster-wide informer factory")
		} else {
			factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
				client, 0, ns, nil)
			log.Info("Created namespace-scoped informer factory", "namespace", ns)
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

	log.Info("Registered new informer")

	// Start the factory (idempotent - starts new informers if factory already running)
	if !factoryExists {
		log.Info("Starting informer factory")
	} else {
		log.Info("Factory already running, starting new informer")
	}
	factory.Start(ctx.Done())

	// ALWAYS wait for this specific informer to sync
	log.Info("Waiting for informer cache sync")
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("failed to sync cache for %v in namespace %s", resource, ns)
	}
	log.Info("Informer cache synced")

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

// SetupWithManager is a placeholder to enable kubebuilder RBAC marker scanning.
// The Manager is manually added to the controller-runtime manager in main.go as a Runnable,
// but this method allows kubebuilder's controller-gen to discover and process the RBAC markers.
func (m *Manager) SetupWithManager(mgr ctrl.Manager) error {
	// No actual setup needed - Manager is added manually in cmd/main.go
	// This method exists solely for kubebuilder RBAC marker scanning
	_ = mgr // Unused but required for signature
	return nil
}
