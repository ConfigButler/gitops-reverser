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

func TestFolderReconciler_FindDifferences(t *testing.T) {
	tests := []struct {
		name                   string
		clusterResources       []types.ResourceIdentifier
		gitResources           []types.ResourceIdentifier
		expectedToCreate       []types.ResourceIdentifier
		expectedToDelete       []types.ResourceIdentifier
		expectedExistingInBoth []types.ResourceIdentifier
	}{
		{
			name: "resources exist in both cluster and Git - no changes needed",
			clusterResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
			},
			gitResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
			},
			expectedToCreate: []types.ResourceIdentifier{},
			expectedToDelete: []types.ResourceIdentifier{},
			expectedExistingInBoth: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
			},
		},
		{
			name: "missing resource in Git - should create",
			clusterResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
				{Group: "", Version: "v1", Resource: "services", Name: "app-svc"}, // Missing in Git
			},
			gitResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			},
			expectedToCreate: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "services", Name: "app-svc"},
			},
			expectedToDelete: []types.ResourceIdentifier{},
			expectedExistingInBoth: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			},
		},
		{
			name: "orphaned resource in Git - should delete",
			clusterResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			},
			gitResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
				{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"}, // Orphan
			},
			expectedToCreate: []types.ResourceIdentifier{},
			expectedToDelete: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"},
			},
			expectedExistingInBoth: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
			},
		},
		{
			name: "both create and delete needed",
			clusterResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "new-pod"},
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
			},
			gitResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "old-pod"}, // Orphan
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
				{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"}, // Orphan
			},
			expectedToCreate: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "new-pod"},
			},
			expectedToDelete: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "old-pod"},
				{Group: "", Version: "v1", Resource: "configmaps", Name: "old-config"},
			},
			expectedExistingInBoth: []types.ResourceIdentifier{
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "app-deployment"},
			},
		},
		{
			name:             "no cluster resources - all Git resources are orphaned",
			clusterResources: []types.ResourceIdentifier{},
			gitResources: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "orphan-pod"},
			},
			expectedToCreate: []types.ResourceIdentifier{},
			expectedToDelete: []types.ResourceIdentifier{
				{Group: "", Version: "v1", Resource: "pods", Name: "orphan-pod"},
			},
			expectedExistingInBoth: []types.ResourceIdentifier{},
		},
		{
			name: "no Git resources - all cluster resources need creation",
			clusterResources: []types.ResourceIdentifier{
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "new-deployment"},
			},
			gitResources: []types.ResourceIdentifier{},
			expectedToCreate: []types.ResourceIdentifier{
				{Group: "apps", Version: "v1", Resource: "deployments", Name: "new-deployment"},
			},
			expectedToDelete:       []types.ResourceIdentifier{},
			expectedExistingInBoth: []types.ResourceIdentifier{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock emitters
			mockEmitter := &MockEventEmitter{}
			mockControlEmitter := &MockControlEventEmitter{}

			// Create reconciler with new API
			gitDest := types.NewResourceReference("test-gitdest", "default")
			reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

			// Call findDifferences
			toCreate, toDelete, existingInBoth := reconciler.findDifferences(tt.clusterResources, tt.gitResources)

			// Verify results
			assert.Len(t, toCreate, len(tt.expectedToCreate), "Number of resources to create should match")
			for _, expected := range tt.expectedToCreate {
				assert.Contains(t, toCreate, expected, "Should contain resource to create: %s", expected.String())
			}

			assert.Len(t, toDelete, len(tt.expectedToDelete), "Number of resources to delete should match")
			for _, expected := range tt.expectedToDelete {
				assert.Contains(t, toDelete, expected, "Should contain resource to delete: %s", expected.String())
			}

			assert.Len(t, existingInBoth, len(tt.expectedExistingInBoth), "Number of existing resources should match")
			for _, expected := range tt.expectedExistingInBoth {
				assert.Contains(t, existingInBoth, expected, "Should contain existing resource: %s", expected.String())
			}
		})
	}
}

func TestFolderReconciler_OnClusterState(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	clusterEvent := events.ClusterStateEvent{
		GitDest: gitDest,
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	}

	// Should process matching event
	reconciler.OnClusterState(clusterEvent)
	assert.NotNil(t, reconciler.clusterResources, "Cluster resources should be set")
	assert.Len(t, reconciler.clusterResources, 1, "Should have one cluster resource")

	// Should not process non-matching event
	otherGitDest := types.NewResourceReference("other-gitdest", "default")
	otherEvent := events.ClusterStateEvent{
		GitDest:   otherGitDest,
		Resources: []types.ResourceIdentifier{},
	}

	reconciler.OnClusterState(otherEvent)
	// Should still have original resources
	assert.Len(t, reconciler.clusterResources, 1, "Should not update cluster resources for non-matching event")
}

func TestFolderReconciler_OnRepoState(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	repoEvent := events.RepoStateEvent{
		GitDest: gitDest,
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
		},
	}

	// Should process matching event
	reconciler.OnRepoState(repoEvent)
	assert.NotNil(t, reconciler.gitResources, "Git resources should be set")
	assert.Len(t, reconciler.gitResources, 1, "Should have one Git resource")

	// Should not process non-matching event
	otherGitDest := types.NewResourceReference("other-gitdest", "default")
	otherEvent := events.RepoStateEvent{
		GitDest:   otherGitDest,
		Resources: []types.ResourceIdentifier{},
	}

	reconciler.OnRepoState(otherEvent)
	// Should still have original resources
	assert.Len(t, reconciler.gitResources, 1, "Should not update Git resources for non-matching event")
}

func TestFolderReconciler_HasBothStates(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	// Initially should have no states
	assert.False(t, reconciler.HasBothStates(), "Should not have both states initially")

	// Set cluster state
	reconciler.clusterResources = []types.ResourceIdentifier{
		{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
	}

	// Should still not have both states
	assert.False(t, reconciler.HasBothStates(), "Should not have both states with only cluster state")

	// Set Git state
	reconciler.gitResources = []types.ResourceIdentifier{
		{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
	}

	// Should now have both states
	assert.True(t, reconciler.HasBothStates(), "Should have both states when both are set")
}

func TestFolderReconciler_GetGitDest(t *testing.T) {
	mockEmitter := &MockEventEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	// Test getter method
	result := reconciler.GetGitDest()
	assert.Equal(t, gitDest, result, "GetGitDest should return the GitDest reference")
	assert.Equal(t, "test-gitdest", result.Name, "Name should match")
	assert.Equal(t, "default", result.Namespace, "Namespace should match")

	// Test String method
	assert.Contains(t, reconciler.String(), "default/test-gitdest", "String should contain gitDest reference")
}

// MockEventEmitter is a mock implementation of EventEmitter for testing.
type MockEventEmitter struct {
	createEvents    []types.ResourceIdentifier
	deleteEvents    []types.ResourceIdentifier
	reconcileEvents []types.ResourceIdentifier
}

func (m *MockEventEmitter) EmitCreateEvent(resource types.ResourceIdentifier) error {
	m.createEvents = append(m.createEvents, resource)
	return nil
}

func (m *MockEventEmitter) EmitDeleteEvent(resource types.ResourceIdentifier) error {
	m.deleteEvents = append(m.deleteEvents, resource)
	return nil
}

func (m *MockEventEmitter) EmitReconcileResourceEvent(resource types.ResourceIdentifier) error {
	m.reconcileEvents = append(m.reconcileEvents, resource)
	return nil
}

func (m *MockEventEmitter) GetCreateEvents() []types.ResourceIdentifier {
	return m.createEvents
}

func (m *MockEventEmitter) GetDeleteEvents() []types.ResourceIdentifier {
	return m.deleteEvents
}

func (m *MockEventEmitter) GetReconcileEvents() []types.ResourceIdentifier {
	return m.reconcileEvents
}

// MockControlEventEmitter is a mock implementation of ControlEventEmitter for testing.
type MockControlEventEmitter struct {
	controlEvents []events.ControlEvent
}

func (m *MockControlEventEmitter) EmitControlEvent(event events.ControlEvent) error {
	m.controlEvents = append(m.controlEvents, event)
	return nil
}

func (m *MockControlEventEmitter) GetControlEvents() []events.ControlEvent {
	return m.controlEvents
}
