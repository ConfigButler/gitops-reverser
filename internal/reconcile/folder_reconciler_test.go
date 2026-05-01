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
	"github.com/ConfigButler/gitops-reverser/internal/git"
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
			mockEmitter := &MockReconcileEmitter{}
			mockControlEmitter := &MockControlEventEmitter{}

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
	mockEmitter := &MockReconcileEmitter{}
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
	mockEmitter := &MockReconcileEmitter{}
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

func TestFolderReconciler_NoDeleteForIdenticalCoreNamespacedResource(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	resource := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  "configmaps",
		Namespace: "ns1",
		Name:      "oeps3",
	}

	reconciler.OnClusterState(events.ClusterStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{resource},
	})
	reconciler.OnRepoState(events.RepoStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{resource},
	})

	assert.Empty(t, mockEmitter.GetEventsByOperation("CREATE"))
	assert.Empty(t, mockEmitter.GetEventsByOperation("DELETE"))
	assert.Equal(t,
		[]types.ResourceIdentifier{resource},
		mockEmitter.GetIdentifiersByOperation(string(events.ReconcileResource)),
	)

	stats := reconciler.GetLastSnapshotStats()
	assert.Equal(t, SnapshotStats{Created: 0, Updated: 1, Deleted: 0}, stats)
}

func TestFolderReconciler_HasBothStates(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	// Initially should have no states
	assert.False(t, reconciler.HasBothStates(), "Should not have both states initially")

	// Set cluster state
	reconciler.clusterResources = []types.ResourceIdentifier{
		{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
	}
	reconciler.clusterStateSeen = true

	// Should still not have both states
	assert.False(t, reconciler.HasBothStates(), "Should not have both states with only cluster state")

	// Set Git state
	reconciler.gitResources = []types.ResourceIdentifier{
		{Group: "", Version: "v1", Resource: "pods", Name: "app-pod"},
	}
	reconciler.gitStateSeen = true

	// Should now have both states
	assert.True(t, reconciler.HasBothStates(), "Should have both states when both are set")
}

func TestFolderReconciler_HasBothStates_WithEmptyStates(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	reconciler.OnClusterState(events.ClusterStateEvent{
		GitDest:   gitDest,
		Resources: nil,
	})
	assert.False(t, reconciler.HasBothStates(), "Should not have both states with only cluster state event")

	reconciler.OnRepoState(events.RepoStateEvent{
		GitDest:   gitDest,
		Resources: nil,
	})
	assert.True(t, reconciler.HasBothStates(), "Should have both states when both events are received, even if empty")
}

func TestFolderReconciler_GetGitDest(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
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

func TestFolderReconciler_EmitsSingleBatch(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	reconciler.OnClusterState(events.ClusterStateEvent{
		GitDest: gitDest,
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "new-pod"},
			{Group: "", Version: "v1", Resource: "pods", Name: "existing-pod"},
		},
	})
	reconciler.OnRepoState(events.RepoStateEvent{
		GitDest: gitDest,
		Resources: []types.ResourceIdentifier{
			{Group: "", Version: "v1", Resource: "pods", Name: "existing-pod"},
			{Group: "", Version: "v1", Resource: "pods", Name: "old-pod"},
		},
	})

	// Exactly one batch should be emitted
	assert.Len(t, mockEmitter.Batches, 1, "Should emit exactly one batch")
	batch := mockEmitter.Batches[0]

	// Batch should contain all events
	assert.Len(t, batch.Events, 3, "Batch should have 3 events (1 create, 1 delete, 1 reconcile)")
	assert.Equal(t, git.CommitModeAtomic, batch.CommitMode, "reconcile snapshots should be committed atomically")
	assert.NotEmpty(t, batch.CommitMessage, "reconcile snapshots should declare a commit message")
	assert.Equal(t,
		"Reconcile snapshot: 1 created, 1 deleted, 1 reconciled",
		batch.CommitMessage,
		"commit message should summarize the reconcile snapshot",
	)
	for _, event := range batch.Events {
		assert.Empty(t, event.UserInfo.Username, "reconcile events should not fabricate a user identity")
	}
}

func TestFolderReconciler_ResetStateRequiresFreshRepoAndClusterSnapshots(t *testing.T) {
	mockEmitter := &MockReconcileEmitter{}
	mockControlEmitter := &MockControlEventEmitter{}
	gitDest := types.NewResourceReference("test-gitdest", "default")
	reconciler := NewFolderReconciler(gitDest, mockEmitter, mockControlEmitter, log.Log)

	initialResource := types.ResourceIdentifier{Group: "", Version: "v1", Resource: "configmaps", Name: "old"}
	updatedResource := types.ResourceIdentifier{Group: "", Version: "v1", Resource: "configmaps", Name: "new"}

	reconciler.OnClusterState(events.ClusterStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{initialResource},
	})
	reconciler.OnRepoState(events.RepoStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{initialResource},
	})
	assert.Len(t, mockEmitter.Batches, 1, "Initial snapshot should emit one batch")

	reconciler.ResetState()
	assert.False(t, reconciler.HasBothStates(), "ResetState should clear observed snapshot flags")

	reconciler.OnRepoState(events.RepoStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{updatedResource},
	})
	assert.Len(t, mockEmitter.Batches, 1, "Fresh repo state alone should not reconcile against stale cluster state")

	reconciler.OnClusterState(events.ClusterStateEvent{
		GitDest:   gitDest,
		Resources: []types.ResourceIdentifier{updatedResource},
	})
	assert.Len(t, mockEmitter.Batches, 2, "Fresh cluster+repo snapshots should trigger the next batch")
}

// MockReconcileEmitter is a mock implementation of ReconcileEmitter for testing.
type MockReconcileEmitter struct {
	Batches []git.WriteRequest
}

func (m *MockReconcileEmitter) EmitWriteRequest(request git.WriteRequest) error {
	m.Batches = append(m.Batches, request)
	return nil
}

// GetEventsByOperation returns all events from all batches matching the given operation.
func (m *MockReconcileEmitter) GetEventsByOperation(op string) []git.Event {
	var result []git.Event
	for _, batch := range m.Batches {
		for _, ev := range batch.Events {
			if ev.Operation == op {
				result = append(result, ev)
			}
		}
	}
	return result
}

// GetIdentifiersByOperation returns resource identifiers from all batch events with the given operation.
func (m *MockReconcileEmitter) GetIdentifiersByOperation(op string) []types.ResourceIdentifier {
	var result []types.ResourceIdentifier
	for _, ev := range m.GetEventsByOperation(op) {
		result = append(result, ev.Identifier)
	}
	return result
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
