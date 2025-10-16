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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Manager is a controller-runtime Runnable that will host dynamic informers
// and translate List+Watch deltas into gitops-reverser events.
// This is a minimal scaffold used to progressively implement the spec.
type Manager struct {
	// Client provides cluster access.
	Client client.Client
	// Log is the logger to use.
	Log logr.Logger
	// RuleStore gives access to compiled WatchRule/ClusterWatchRule.
	RuleStore *rulestore.RuleStore
	// EventQueue is where sanitized events will be enqueued for git workers.
	EventQueue *eventqueue.Queue
}

const (
	heartbeatInterval = 30 * time.Second
)

// Start begins the watch ingestion manager and blocks until context cancellation.
// Step 1 (MVP scaffold): emit a heartbeat and start a polling loop for ConfigMaps
// to validate the end-to-end pipeline. Further steps will replace polling with
// shared informers and add rule-driven GVR selection, orphan detection, batching, etc.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (scaffold)")
	defer log.Info("watch ingestion manager stopping")

	// Compute concrete GVRs from active rules (aggregation step).
	if m.RuleStore != nil {
		gvrList := m.ComputeRequestedGVRs()
		if len(gvrList) > 0 {
			discoverable := m.FilterDiscoverableGVRs(ctx, gvrList)
			log.Info(
				"aggregated requested GVRs from rules",
				"requested", len(gvrList),
				"discoverable", len(discoverable),
			)
			if len(discoverable) > 0 {
				// Start dynamic informers for discoverable GVRs (best-effort).
				m.maybeStartInformers(ctx)
				// Perform initial seed listing to enqueue upserts and compute initial live state.
				go m.seedSelectedResources(ctx)
			}
		} else {
			log.Info("no concrete GVRs from active rules yet")
		}
	}

	// Heartbeat ticker to make liveness observable in logs and tests.
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			log.V(1).Info("watch manager heartbeat")
		}
	}
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

// enqueueMatches pushes sanitized events to the shared event queue for both rule types.
func (m *Manager) enqueueMatches(
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	watchRules []rulestore.CompiledRule,
	clusterRules []rulestore.CompiledClusterRule,
) {
	// WatchRule matches.
	for _, rule := range watchRules {
		ev := eventqueue.Event{
			Object:                 sanitized.DeepCopy(),
			Identifier:             id,
			Operation:              "UPDATE",
			UserInfo:               eventqueue.UserInfo{}, // no admission user in watch-based ingestion
			GitRepoConfigRef:       rule.GitRepoConfigRef,
			GitRepoConfigNamespace: rule.Source.Namespace,
			Branch:                 rule.Branch,
			BaseFolder:             rule.BaseFolder,
		}
		m.EventQueue.Enqueue(ev)
	}

	// ClusterWatchRule matches.
	for _, cr := range clusterRules {
		ev := eventqueue.Event{
			Object:                 sanitized.DeepCopy(),
			Identifier:             id,
			Operation:              "UPDATE",
			UserInfo:               eventqueue.UserInfo{},
			GitRepoConfigRef:       cr.GitRepoConfigRef,
			GitRepoConfigNamespace: cr.GitRepoConfigNamespace,
			Branch:                 cr.Branch,
			BaseFolder:             cr.BaseFolder,
		}
		m.EventQueue.Enqueue(ev)
	}
}

// seedSelectedResources performs a one-time List across all discoverable GVRs derived from active rules,
// sanitizes objects, matches rules, and enqueues UPDATE events to bootstrap the repository state.
// Orphan detection will be added in a subsequent step.
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

	// Collect unique GitRepoConfig refs seen during seed to emit a SEED_SYNC control event per repo.
	repoKeys := make(map[k8stypes.NamespacedName]struct{})

	for _, g := range discoverable {
		m.seedListAndProcess(ctx, dc, g, repoKeys)
	}

	m.emitSeedSyncControls(repoKeys)

	log.Info("seed listing completed", "gvrCount", len(discoverable), "repoKeys", len(repoKeys))
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

// seedListAndProcess lists objects for a GVR and processes them into enqueue operations.
func (m *Manager) seedListAndProcess(
	ctx context.Context,
	dc dynamic.Interface,
	g GVR,
	repoKeys map[k8stypes.NamespacedName]struct{},
) {
	log := m.Log.WithName("seed").WithValues("group", g.Group, "version", g.Version, "resource", g.Resource)

	res := schema.GroupVersionResource{Group: g.Group, Version: g.Version, Resource: g.Resource}
	list, err := dc.Resource(res).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Error(err, "seed list failed")
		return
	}

	// Metrics: count objects scanned by seed (per GVR batch).
	metrics.ObjectsScannedTotal.Add(ctx, int64(len(list.Items)))

	for i := range list.Items {
		m.processListedObject(ctx, &list.Items[i], g, repoKeys)
	}
}

// processListedObject evaluates rules, tracks repo keys, sanitizes, and enqueues events for one item.
func (m *Manager) processListedObject(
	ctx context.Context,
	u *unstructured.Unstructured,
	g GVR,
	repoKeys map[k8stypes.NamespacedName]struct{},
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

	// Track GitRepoConfig keys for SEED_SYNC control emission (orphan detection) after seed.
	for _, r := range wrRules {
		repoKeys[k8stypes.NamespacedName{Name: r.GitRepoConfigRef, Namespace: r.Source.Namespace}] = struct{}{}
	}
	for _, cr := range cwrRules {
		repoKeys[k8stypes.NamespacedName{Name: cr.GitRepoConfigRef, Namespace: cr.GitRepoConfigNamespace}] = struct{}{}
	}

	sanitized := sanitize.Sanitize(u)
	m.enqueueMatches(sanitized, id, wrRules, cwrRules)

	enq := int64(len(wrRules) + len(cwrRules))
	if enq > 0 {
		metrics.EventsProcessedTotal.Add(ctx, enq)
		metrics.GitCommitQueueSize.Add(ctx, enq)
	}
}

// emitSeedSyncControls enqueues one SEED_SYNC control event per repository key.
func (m *Manager) emitSeedSyncControls(repoKeys map[k8stypes.NamespacedName]struct{}) {
	for key := range repoKeys {
		m.EventQueue.Enqueue(eventqueue.Event{
			Object:                 nil,
			Identifier:             itypes.ResourceIdentifier{},
			Operation:              "SEED_SYNC",
			UserInfo:               eventqueue.UserInfo{},
			GitRepoConfigRef:       key.Name,
			GitRepoConfigNamespace: key.Namespace,
		})
	}
}
