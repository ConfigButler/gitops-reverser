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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
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

// Lint-friendly timing constants (avoid magic numbers).
const (
	heartbeatInterval = 30 * time.Second
	// poll interval for the initial seed/trailing implementation for ConfigMaps.
	configMapPollInterval = 20 * time.Second
)

// Start begins the watch ingestion manager and blocks until context cancellation.
// Step 1 (MVP scaffold): emit a heartbeat and start a polling loop for ConfigMaps
// to validate the end-to-end pipeline. Further steps will replace polling with
// shared informers and add rule-driven GVR selection, orphan detection, batching, etc.
func (m *Manager) Start(ctx context.Context) error {
	log := m.Log.WithName("watch")
	log.Info("watch ingestion manager starting (scaffold)")
	defer log.Info("watch ingestion manager stopping")

	// Start minimal polling loop for ConfigMaps (disabled effects unless rules match).
	go m.pollConfigMaps(ctx)

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

// pollConfigMaps periodically lists ConfigMaps cluster-wide and enqueues UPDATE events
// for rules that match. This validates the ingestion path without requiring admission webhooks.
// NOTE: This is intentionally simple (no RV tracking or deletes) and will be replaced
// by discovery-driven informers and proper trailing logic in subsequent steps.
func (m *Manager) pollConfigMaps(ctx context.Context) {
	log := m.Log.WithName("configmaps")
	ticker := time.NewTicker(configMapPollInterval)
	defer ticker.Stop()

	const (
		apiGroup       = ""   // core
		apiVersion     = "v1" // core/v1
		resourcePlural = "configmaps"
	)

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping configmap poller")
			return
		case <-ticker.C:
			items, err := m.listConfigMaps(ctx)
			if err != nil {
				log.Error(err, "failed to list ConfigMaps")
				continue
			}

			for i := range items {
				cm := &items[i]

				u, err := toUnstructured(cm)
				if err != nil {
					log.Error(
						err,
						"failed to convert configmap to unstructured",
						"name", cm.Name,
						"namespace", cm.Namespace,
					)
					continue
				}

				id := buildIdentifierFromCM(apiGroup, apiVersion, resourcePlural, cm)

				nsLabels := m.getNamespaceLabels(ctx, cm.Namespace)
				isClusterScoped := false

				wrRules, cwrRules := m.matchRules(u, resourcePlural, apiGroup, apiVersion, isClusterScoped, nsLabels)
				if len(wrRules) == 0 && len(cwrRules) == 0 {
					continue
				}

				sanitized := sanitize.Sanitize(u)
				m.enqueueMatches(sanitized, id, wrRules, cwrRules)
			}
		}
	}
}

// listConfigMaps retrieves all ConfigMaps in the cluster.
func (m *Manager) listConfigMaps(ctx context.Context) ([]corev1.ConfigMap, error) {
	var list corev1.ConfigMapList
	if err := m.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// toUnstructured converts a typed ConfigMap to an Unstructured object.
func toUnstructured(cm *corev1.ConfigMap) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cm)
	if err != nil {
		return nil, err
	}
	obj.Object = objMap
	return obj, nil
}

// buildIdentifierFromCM constructs a ResourceIdentifier for a ConfigMap.
func buildIdentifierFromCM(
	apiGroup, apiVersion, resourcePlural string,
	cm *corev1.ConfigMap,
) itypes.ResourceIdentifier {
	return itypes.NewResourceIdentifier(
		apiGroup,
		apiVersion,
		resourcePlural,
		cm.Namespace,
		cm.Name,
	)
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
		}
		m.EventQueue.Enqueue(ev)
	}
}
