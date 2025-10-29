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

package reconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestBaseFolderReconciler_FullReconciliationCycle tests the complete reconciliation cycle
// with both cluster and Git state events.
func TestBaseFolderReconciler_FullReconciliationCycle(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	reconciler := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter, log.Log)

	// Initial state - should not reconcile yet
	reconciler.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	})

	// Should not have reconciled yet (missing Git state)
	assert.False(t, reconciler.HasBothStates(), "Should not have both states")
	assert.Empty(t, mockEmitter.GetCreateEvents(), "Should not emit events without both states")

	// Provide Git state - should now reconcile
	reconciler.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			{Group: "", Version: "v1", Resource: "services", Name: "old-service"}, // Orphan
		},
	})

	// Should now have both states and should have reconciled
	assert.True(t, reconciler.HasBothStates(), "Should have both states")
	assert.Empty(t, mockEmitter.GetCreateEvents(), "No resources should be created (pod exists in both)")
	// The service is an orphan in Git (exists in Git but not in cluster), so it should be deleted
	assert.Len(t, mockEmitter.GetDeleteEvents(), 1, "Should delete orphaned service")
	assert.Equal(t, "old-service", mockEmitter.GetDeleteEvents()[0].Name, "Should delete orphaned service")
	assert.Len(t, mockEmitter.GetReconcileEvents(), 1, "Should emit reconcile event for existing resource")
}

// TestBaseFolderReconciler_MissingInGit tests reconciliation when cluster has resources not in Git.
func TestBaseFolderReconciler_MissingInGit(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	reconciler := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter, log.Log)

	// Cluster has resources, Git has subset
	reconciler.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			{Group: "", Version: "v1", Resource: "services", Name: "app-svc"}, // Missing in Git
		},
	})

	reconciler.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	})

	// Should emit create event for missing service
	createEvents := mockEmitter.GetCreateEvents()
	assert.Len(t, createEvents, 1, "Should emit one create event")
	assert.Equal(t, "app-svc", createEvents[0].Name, "Should create missing service")

	// Should emit reconcile event for existing resource
	reconcileEvents := mockEmitter.GetReconcileEvents()
	assert.Len(t, reconcileEvents, 1, "Should emit one reconcile event")
	assert.Equal(t, "app-pod", reconcileEvents[0].Name, "Should reconcile existing pod")
}

// TestBaseFolderReconciler_OrphansInGit tests reconciliation when Git has orphaned resources.
func TestBaseFolderReconciler_OrphansInGit(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	reconciler := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter, log.Log)

	// Git has resources not in cluster
	reconciler.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	})

	reconciler.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"}, // Orphan
		},
	})

	// Should emit delete event for orphan
	deleteEvents := mockEmitter.GetDeleteEvents()
	assert.Len(t, deleteEvents, 1, "Should emit one delete event")
	assert.Equal(t, "old-config", deleteEvents[0].Name, "Should delete orphan configmap")

	// Should emit reconcile event for existing resource
	reconcileEvents := mockEmitter.GetReconcileEvents()
	assert.Len(t, reconcileEvents, 1, "Should emit one reconcile event")
	assert.Equal(t, "app-pod", reconcileEvents[0].Name, "Should reconcile existing pod")
}

// TestBaseFolderReconciler_OrderIndependence tests that event order doesn't matter.
func TestBaseFolderReconciler_OrderIndependence(t *testing.T) {
	// Test 1: Git state first, then cluster state
	mockEmitter1 := &MockEventEmitter{}
	reconciler1 := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter1, log.Log)

	reconciler1.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	})

	reconciler1.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			{Group: "", Version: "v1", Resource: "services", Name: "app-svc"},
		},
	})

	// Test 2: Cluster state first, then Git state
	mockEmitter2 := &MockEventEmitter{}
	reconciler2 := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter2, log.Log)

	reconciler2.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			{Group: "", Version: "v1", Resource: "services", Name: "app-svc"},
		},
	})

	reconciler2.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	})

	// Both should produce the same results regardless of order
	createEvents1 := mockEmitter1.GetCreateEvents()
	createEvents2 := mockEmitter2.GetCreateEvents()

	assert.Len(t, createEvents2, len(createEvents1), "Both orders should produce same number of create events")
	assert.Len(t, createEvents1, 1, "Should have one create event")

	if len(createEvents1) > 0 && len(createEvents2) > 0 {
		assert.Equal(t, createEvents1[0], createEvents2[0], "Both orders should produce same create event")
	}
}

// TestBaseFolderReconciler_ScopeIsolation tests that different scopes don't interfere.
func TestBaseFolderReconciler_ScopeIsolation(t *testing.T) {
	mockEmitter1 := &MockEventEmitter{}
	reconciler1 := NewBaseFolderReconciler("test-repo", "main", "apps", mockEmitter1, log.Log)

	mockEmitter2 := &MockEventEmitter{}
	reconciler2 := NewBaseFolderReconciler("test-repo", "main", "infrastructure", mockEmitter2, log.Log)

	// Send events to first reconciler (apps)
	reconciler1.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
		},
	})

	reconciler1.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "apps",
		Resources: []types.ResourceIdentifier{
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
		},
	})

	// Send events to second reconciler (infrastructure)
	reconciler2.OnClusterState(events.ClusterStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "infrastructure",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "nodes", Name: "worker-node"},
		},
	})

	reconciler2.OnRepoState(events.RepoStateEvent{
		RepoName:   "test-repo",
		Branch:     "main",
		BaseFolder: "infrastructure",
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "nodes", Name: "worker-node"},
			{Group: "", Version: "v1", Resource: "configmaps", Name: "orphan-cm"}, // Orphan
		},
	})

	// Each reconciler should have its own state and events
	assert.True(t, reconciler1.HasBothStates(), "First reconciler should have both states")
	assert.True(t, reconciler2.HasBothStates(), "Second reconciler should have both states")

	// First reconciler should have reconcile event for deployment
	reconcileEvents1 := mockEmitter1.GetReconcileEvents()
	assert.Len(t, reconcileEvents1, 1, "First reconciler should have one reconcile event")
	assert.Equal(t, "app-deployment", reconcileEvents1[0].Name, "Should reconcile deployment")

	// Second reconciler should have delete event for orphan configmap
	deleteEvents2 := mockEmitter2.GetDeleteEvents()
	assert.Len(t, deleteEvents2, 1, "Second reconciler should have one delete event")
	assert.Equal(t, "orphan-cm", deleteEvents2[0].Name, "Should delete orphan configmap")

	// Cross-contamination check
	assert.Empty(t, mockEmitter1.GetDeleteEvents(), "First reconciler should not have delete events")
	// Second reconciler should have reconcile event for worker-node (exists in both places)
	reconcileEvents2 := mockEmitter2.GetReconcileEvents()
	assert.Len(t, reconcileEvents2, 1, "Second reconciler should have one reconcile event")
	assert.Equal(t, "worker-node", reconcileEvents2[0].Name, "Should reconcile worker-node")
}

// TestBaseFolderReconciler_ComplexScenario tests a complex real-world scenario.
func TestBaseFolderReconciler_ComplexScenario(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	reconciler := NewBaseFolderReconciler("my-app", "feature/new-ui", "k8s", mockEmitter, log.Log)

	// Initial cluster state
	reconciler.OnClusterState(events.ClusterStateEvent{
		RepoName:   "my-app",
		Branch:     "feature/new-ui",
		BaseFolder: "k8s",
		Resources: []types.ResourceIdentifier{
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "frontend"},
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "backend"},
			{Group: "", Version: "v1", Resource: "services", Name: "frontend-svc"},
			{Group: "", Version: "v1", Resource: "services", Name: "backend-svc"},
			{Group: "", Version: "v1", Resource: "configmaps", Name: "app-config"},
		},
	})

	// Current Git state (missing some resources, has some orphans)
	reconciler.OnRepoState(events.RepoStateEvent{
		RepoName:   "my-app",
		Branch:     "feature/new-ui",
		BaseFolder: "k8s",
		Resources: []types.ResourceIdentifier{
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "frontend"},
			{Group: "", Version: "v1", Resource: "services", Name: "frontend-svc"},
			{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"},      // Orphan
			{Group: "", Version: "v1", Resource: "secrets", Name: "legacy-secret"},      // Orphan
			{Group: "apps", Version: "v1", Resource: "deployments", Name: "deprecated"}, // Orphan
		},
	})

	// Verify complex reconciliation results
	createEvents := mockEmitter.GetCreateEvents()
	deleteEvents := mockEmitter.GetDeleteEvents()
	reconcileEvents := mockEmitter.GetReconcileEvents()

	// Should create missing resources (backend, backend-svc, app-config)
	assert.Len(t, createEvents, 3, "Should create 3 missing resources")

	createNames := make(map[string]bool)
	for _, event := range createEvents {
		createNames[event.Name] = true
	}
	assert.True(t, createNames["backend"], "Should create missing backend deployment")
	assert.True(t, createNames["backend-svc"], "Should create missing backend service")
	assert.True(t, createNames["app-config"], "Should create missing configmap")

	// Should delete orphaned resources (old-config, legacy-secret, deprecated)
	assert.Len(t, deleteEvents, 3, "Should delete 3 orphaned resources")

	deleteNames := make(map[string]bool)
	for _, event := range deleteEvents {
		deleteNames[event.Name] = true
	}
	assert.True(t, deleteNames["old-config"], "Should delete old configmap")
	assert.True(t, deleteNames["legacy-secret"], "Should delete legacy secret")
	assert.True(t, deleteNames["deprecated"], "Should delete deprecated deployment")

	// Should reconcile existing resources (frontend, frontend-svc)
	assert.Len(t, reconcileEvents, 2, "Should reconcile 2 existing resources")

	reconcileNames := make(map[string]bool)
	for _, event := range reconcileEvents {
		reconcileNames[event.Name] = true
	}
	assert.True(t, reconcileNames["frontend"], "Should reconcile frontend deployment")
	assert.True(t, reconcileNames["frontend-svc"], "Should reconcile frontend service")
}
