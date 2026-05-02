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
	"sync"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

var initWatchMetricsOnce sync.Once

type recordingEnqueuer struct {
	events []git.Event
}

func (r *recordingEnqueuer) Enqueue(event git.Event) {
	r.events = append(r.events, event)
}

func (r *recordingEnqueuer) EnqueueRequest(request *git.WriteRequest) {
	if request == nil {
		return
	}
	r.events = append(r.events, request.Events...)
}

func TestHandleEvent_SkipsLiveRoutingWhenAuditLiveEventsEnabled(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr", Namespace: "default"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"configmaps"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"target", "default", "provider", "default", "main", "state",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("target", "default", enqueuer, logr.Discard())
	stream.OnReconciliationComplete()
	gitDest := itypes.NewResourceReference("target", "default")
	router.RegisterGitTargetEventStream(gitDest, stream)

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
		}).
		Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: true,
	}

	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cm-test",
			Namespace: "default",
		},
		Data: map[string]string{"key": "value"},
	}

	manager.handleEvent(obj, GVR{Group: "", Version: "v1", Resource: "configmaps"}, configv1alpha1.OperationCreate)

	if len(enqueuer.events) != 0 {
		t.Fatalf(
			"expected no live events to be enqueued when audit live events are enabled, got %d",
			len(enqueuer.events),
		)
	}
}

func TestHandleEvent_SkipsClusterScopedLiveRoutingWhenAuditLiveEventsEnabled(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	store.AddOrUpdateClusterWatchRule(
		configv1alpha1.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "cwr"},
			Spec: configv1alpha1.ClusterWatchRuleSpec{
				TargetRef: configv1alpha1.NamespacedTargetReference{Name: "target", Namespace: "default"},
				Rules: []configv1alpha1.ClusterResourceRule{{
					Scope:       configv1alpha1.ResourceScopeCluster,
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"namespaces"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"target", "default", "provider", "default", "main", "state",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("target", "default", enqueuer, logr.Discard())
	stream.OnReconciliationComplete()
	gitDest := itypes.NewResourceReference("target", "default")
	router.RegisterGitTargetEventStream(gitDest, stream)

	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: true,
	}

	obj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-scope-test"},
	}

	manager.handleEvent(
		obj,
		GVR{Group: "", Version: "v1", Resource: "namespaces", Scope: configv1alpha1.ResourceScopeCluster},
		configv1alpha1.OperationCreate,
	)

	if len(enqueuer.events) != 0 {
		t.Fatalf("expected cluster-scoped live event to be skipped, got %d", len(enqueuer.events))
	}
}

func TestHandleEvent_SkipsSecretLiveRoutingWhenAuditLiveEventsEnabled(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr", Namespace: "default"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"secrets"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"target", "default", "provider", "default", "main", "state",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("target", "default", enqueuer, logr.Discard())
	stream.OnReconciliationComplete()
	gitDest := itypes.NewResourceReference("target", "default")
	router.RegisterGitTargetEventStream(gitDest, stream)

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
		}).
		Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: true,
	}

	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret-test",
			Namespace: "default",
		},
		StringData: map[string]string{"token": "value"},
	}

	manager.handleEvent(obj, GVR{Group: "", Version: "v1", Resource: "secrets"}, configv1alpha1.OperationCreate)

	if len(enqueuer.events) != 0 {
		t.Fatalf("expected secret live event to be skipped, got %d", len(enqueuer.events))
	}
}

func TestHandleEvent_NamespacedWatchRuleMustNotRouteForeignNamespaceObject(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "playground-watchrule", Namespace: "tilt-playground"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "playground-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"services"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"playground-target", "tilt-playground", "playground-provider", "tilt-playground", "main", "live",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("playground-target", "tilt-playground", enqueuer, logr.Discard())
	stream.OnReconciliationComplete()
	gitDest := itypes.NewResourceReference("playground-target", "tilt-playground")
	router.RegisterGitTargetEventStream(gitDest, stream)

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tilt-playground"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gitops-reverser"}},
		).
		Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: false,
	}

	// A Service from gitops-reverser namespace must NOT be routed by a WatchRule in tilt-playground.
	obj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitops-reverser",
			Namespace: "gitops-reverser",
		},
	}

	manager.handleEvent(obj, GVR{Group: "", Version: "v1", Resource: "services"}, configv1alpha1.OperationUpdate)

	if len(enqueuer.events) != 0 {
		t.Fatalf("expected foreign-namespace event to be rejected, got %d routed events", len(enqueuer.events))
	}
}

func TestHandleEvent_NamespacedWatchRuleRoutesOwnNamespaceObject(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "playground-watchrule", Namespace: "tilt-playground"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "playground-target"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"services"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"playground-target", "tilt-playground", "playground-provider", "tilt-playground", "main", "live",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream("playground-target", "tilt-playground", enqueuer, logr.Discard())
	stream.OnReconciliationComplete()
	gitDest := itypes.NewResourceReference("playground-target", "tilt-playground")
	router.RegisterGitTargetEventStream(gitDest, stream)

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tilt-playground"}},
		).
		Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: false,
	}

	// A Service from the same namespace as the WatchRule MUST be routed.
	obj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-service",
			Namespace: "tilt-playground",
		},
	}

	manager.handleEvent(obj, GVR{Group: "", Version: "v1", Resource: "services"}, configv1alpha1.OperationUpdate)

	if len(enqueuer.events) != 1 {
		t.Fatalf("expected same-namespace event to be routed, got %d events", len(enqueuer.events))
	}
	if enqueuer.events[0].Identifier.Namespace != "tilt-playground" {
		t.Fatalf("expected routed event namespace tilt-playground, got %s", enqueuer.events[0].Identifier.Namespace)
	}
}

func TestHandleEvent_MixedRules_OnlyClusterWatchRuleMatchesForeignNamespace(t *testing.T) {
	initWatchMetricsOnce.Do(func() {
		_, _ = telemetry.InitOTLPExporter(context.Background())
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add api scheme: %v", err)
	}

	store := rulestore.NewStore()
	// Namespaced WatchRule in ns-a — must NOT match events from ns-b.
	store.AddOrUpdateWatchRule(
		configv1alpha1.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "wr-ns-a", Namespace: "ns-a"},
			Spec: configv1alpha1.WatchRuleSpec{
				TargetRef: configv1alpha1.LocalTargetReference{Name: "target-a"},
				Rules: []configv1alpha1.ResourceRule{{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"services"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"target-a", "ns-a", "provider-a", "ns-a", "main", "live-a",
	)
	// ClusterWatchRule — MUST match events from any namespace.
	store.AddOrUpdateClusterWatchRule(
		configv1alpha1.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "cwr-all"},
			Spec: configv1alpha1.ClusterWatchRuleSpec{
				TargetRef: configv1alpha1.NamespacedTargetReference{Name: "target-cluster", Namespace: "ops"},
				Rules: []configv1alpha1.ClusterResourceRule{{
					Scope:       configv1alpha1.ResourceScopeNamespaced,
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"services"},
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
				}},
			},
		},
		"target-cluster", "ops", "provider-ops", "ops", "main", "live-cluster",
	)

	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: make(map[string]*reconcile.GitTargetEventStream),
	}
	enqueueA := &recordingEnqueuer{}
	streamA := reconcile.NewGitTargetEventStream("target-a", "ns-a", enqueueA, logr.Discard())
	streamA.OnReconciliationComplete()
	router.RegisterGitTargetEventStream(itypes.NewResourceReference("target-a", "ns-a"), streamA)

	enqueueCluster := &recordingEnqueuer{}
	streamCluster := reconcile.NewGitTargetEventStream("target-cluster", "ops", enqueueCluster, logr.Discard())
	streamCluster.OnReconciliationComplete()
	router.RegisterGitTargetEventStream(itypes.NewResourceReference("target-cluster", "ops"), streamCluster)

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-a"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-b"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ops"}},
		).
		Build()

	manager := &Manager{
		Client:                 fakeK8s,
		Log:                    logr.Discard(),
		RuleStore:              store,
		EventRouter:            router,
		AuditLiveEventsEnabled: false,
	}

	// Event from ns-b: WatchRule in ns-a must NOT match, ClusterWatchRule MUST match.
	obj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-in-b",
			Namespace: "ns-b",
		},
	}

	manager.handleEvent(obj, GVR{Group: "", Version: "v1", Resource: "services"}, configv1alpha1.OperationCreate)

	if len(enqueueA.events) != 0 {
		t.Fatalf("expected namespaced WatchRule in ns-a to reject ns-b event, got %d events", len(enqueueA.events))
	}
	if len(enqueueCluster.events) != 1 {
		t.Fatalf("expected ClusterWatchRule to route ns-b event, got %d events", len(enqueueCluster.events))
	}
}
