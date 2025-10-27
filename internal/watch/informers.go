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

package watch

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Note: startDynamicInformers removed - replaced by startInformersForGVRs in manager.go
// which provides better lifecycle management per GVR.

// addHandlers wires add/update/delete handlers for a single GVR to enqueue events.
func (m *Manager) addHandlers(inf cache.SharedIndexInformer, g GVR) {
	// Check the error returned by AddEventHandler to satisfy errcheck.
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			m.handleEvent(obj, g, configv1alpha1.OperationCreate)
		},
		UpdateFunc: func(_, newObj interface{}) {
			m.handleEvent(newObj, g, configv1alpha1.OperationUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			m.handleEvent(obj, g, configv1alpha1.OperationDelete)
		},
	}); err != nil {
		m.Log.WithName("informer").Error(
			err, "failed to add event handler",
			"group", g.Group, "version", g.Version, "resource", g.Resource,
		)
	}
}

// handleEvent converts an informer object to Unstructured, matches rules, sanitizes,
// enriches with correlation username, and enqueues commit events.
// Implements deduplication to skip status-only changes.
func (m *Manager) handleEvent(obj interface{}, g GVR, op configv1alpha1.OperationType) {
	u := toUnstructuredFromInformer(obj)
	if u == nil {
		return
	}

	ctx := context.Background()

	// Identifier from GVR + object metadata
	id := itypes.NewResourceIdentifier(g.Group, g.Version, g.Resource, u.GetNamespace(), u.GetName())

	// Namespace labels for namespaced scope
	var nsLabels map[string]string
	if id.Namespace != "" {
		nsLabels = m.getNamespaceLabels(ctx, id.Namespace)
	}

	isClusterScoped := id.IsClusterScoped()
	wrRules, cwrRules := m.matchRules(u, g.Resource, g.Group, g.Version, isClusterScoped, nsLabels)
	if len(wrRules) == 0 && len(cwrRules) == 0 {
		return
	}

	sanitized := sanitize.Sanitize(u)

	// Check for duplicate content (status-only changes) - use shared deduplication logic
	if m.isDuplicateContent(ctx, sanitized, id) {
		m.Log.V(1).Info("Skipping duplicate sanitized content (likely status-only change)",
			"identifier", id.String())
		metrics.WatchDuplicatesSkippedTotal.Add(ctx, 1)
		return
	}

	// Attempt correlation enrichment
	userInfo := m.tryEnrichFromCorrelation(ctx, sanitized, id, string(op))

	// Emit basic metrics for watcher path (mirrors webhook semantics).
	// Count each watched object processed by the informer path.
	metrics.ObjectsScannedTotal.Add(ctx, 1)
	enqueueCount := int64(len(wrRules) + len(cwrRules))
	if enqueueCount > 0 {
		metrics.EventsProcessedTotal.Add(ctx, enqueueCount)
		metrics.GitCommitQueueSize.Add(ctx, enqueueCount)
	}

	// WatchRule matches - route to workers
	for _, rule := range wrRules {
		ev := git.Event{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  string(op),
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
	for _, cr := range cwrRules {
		ev := git.Event{
			Object:     sanitized.DeepCopy(),
			Identifier: id,
			Operation:  string(op),
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

// toUnstructuredFromInformer safely unwraps a runtime object from informer callbacks.
func toUnstructuredFromInformer(obj interface{}) *unstructured.Unstructured {
	switch t := obj.(type) {
	case *unstructured.Unstructured:
		return t
	case cache.DeletedFinalStateUnknown:
		if u, ok := t.Obj.(*unstructured.Unstructured); ok {
			return u
		}
	case *cache.DeletedFinalStateUnknown:
		if u, ok := t.Obj.(*unstructured.Unstructured); ok {
			return u
		}
	default:
		// Try to convert typed objects (very rare with dynamic informer)
		if ro, ok := t.(runtime.Object); ok {
			u := &unstructured.Unstructured{}
			if m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ro); err == nil {
				u.Object = m
				return u
			}
		}
	}
	return nil
}

// Note: maybeStartInformers removed - replaced by ReconcileForRuleChange
// which is called explicitly by controllers when rules change.
