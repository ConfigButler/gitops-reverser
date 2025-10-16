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
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// Lint-friendly constants.
const (
	defaultResync = 0 * time.Second
)

// startDynamicInformers computes requested GVRs from rules, filters to those discoverable on the
// current API server (list+watch), and starts dynamic informers for them. Deltas are translated
// into enqueue operations using the existing sanitization and RuleStore matching logic.
// This is an MVP implementation; batching/orphan detection is handled elsewhere.
func (m *Manager) startDynamicInformers(ctx context.Context) error {
	cfg := m.restConfig()
	if cfg == nil {
		// In tests without a running control plane this may be nil; no-op.
		return nil
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	// Aggregate and filter GVRs
	requested := m.ComputeRequestedGVRs()
	discoverable := m.FilterDiscoverableGVRs(ctx, requested)
	if len(discoverable) == 0 {
		return nil
	}

	// Shared factory with no resync; we rely on watch events
	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, defaultResync)

	// Register informers per GVR
	for _, g := range discoverable {
		resource := schema.GroupVersionResource{Group: g.Group, Version: g.Version, Resource: g.Resource}
		informer := factory.ForResource(resource).Informer()
		m.addHandlers(informer, g)
	}

	// Start informers and wait for cache sync
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	return nil
}

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
// and enqueues commit events.
func (m *Manager) handleEvent(obj interface{}, g GVR, op configv1alpha1.OperationType) {
	u := toUnstructuredFromInformer(obj)
	if u == nil {
		return
	}

	// Identifier from GVR + object metadata
	id := itypes.NewResourceIdentifier(g.Group, g.Version, g.Resource, u.GetNamespace(), u.GetName())

	// Namespace labels for namespaced scope
	var nsLabels map[string]string
	if id.Namespace != "" {
		nsLabels = m.getNamespaceLabels(context.Background(), id.Namespace)
	}

	isClusterScoped := id.IsClusterScoped()
	wrRules, cwrRules := m.matchRules(u, g.Resource, g.Group, g.Version, isClusterScoped, nsLabels)
	if len(wrRules) == 0 && len(cwrRules) == 0 {
		return
	}

	sanitized := sanitize.Sanitize(u)

	// Emit basic metrics for watcher path (mirrors webhook semantics).
	ctx := context.Background()
	// Count each watched object processed by the informer path.
	metrics.ObjectsScannedTotal.Add(ctx, 1)
	enqueueCount := int64(len(wrRules) + len(cwrRules))
	if enqueueCount > 0 {
		metrics.EventsProcessedTotal.Add(ctx, enqueueCount)
		metrics.GitCommitQueueSize.Add(ctx, enqueueCount)
	}

	// WatchRule matches.
	for _, rule := range wrRules {
		ev := eventqueue.Event{
			Object:                 sanitized.DeepCopy(),
			Identifier:             id,
			Operation:              string(op),
			UserInfo:               eventqueue.UserInfo{},
			GitRepoConfigRef:       rule.GitRepoConfigRef,
			GitRepoConfigNamespace: rule.Source.Namespace,
			Branch:                 rule.Branch,
			BaseFolder:             rule.BaseFolder,
		}
		m.EventQueue.Enqueue(ev)
	}

	// ClusterWatchRule matches.
	for _, cr := range cwrRules {
		ev := eventqueue.Event{
			Object:                 sanitized.DeepCopy(),
			Identifier:             id,
			Operation:              string(op),
			UserInfo:               eventqueue.UserInfo{},
			GitRepoConfigRef:       cr.GitRepoConfigRef,
			GitRepoConfigNamespace: cr.GitRepoConfigNamespace,
			Branch:                 cr.Branch,
			BaseFolder:             cr.BaseFolder,
		}
		m.EventQueue.Enqueue(ev)
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

// Replace the existing configmap polling with dynamic informers when available.
// This keeps the MVP polling in place while we roll out informers progressively.
func (m *Manager) maybeStartInformers(ctx context.Context) {
	// Best-effort; errors are logged at the call site if needed.
	_ = m.startDynamicInformers(ctx)
}
