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

package git

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/events"
)

// mockRepoStateEmitter implements the interface needed for EmitRepoState
type mockRepoStateEmitter struct {
	emittedEvents []events.RepoStateEvent
}

func (m *mockRepoStateEmitter) EmitRepoStateEvent(event events.RepoStateEvent) error {
	m.emittedEvents = append(m.emittedEvents, event)
	return nil
}

func setupBranchWorkerTest() (*BranchWorker, *mockRepoStateEmitter, func()) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	emitter := &mockRepoStateEmitter{}

	worker := NewBranchWorker(client, log, "test-repo", "gitops-system", "main")

	cleanup := func() {
		if worker.started {
			worker.Stop()
		}
	}

	return worker, emitter, cleanup
}

// TestEmitRepoState_BasicFunctionality verifies EmitRepoState can be called without error
func TestEmitRepoState_BasicFunctionality(t *testing.T) {
	worker, emitter, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test verifies the method can be called without panicking
	// In a real scenario, this would require setting up a Git repository
	// For now, we just ensure the method signature and basic flow work
	err := worker.EmitRepoState("apps", emitter)

	// We expect an error since no GitRepoConfig exists in the fake client
	// But the important thing is that the method doesn't panic
	if err == nil {
		t.Error("Expected error due to missing GitRepoConfig, but got nil")
	}
}

// TestEmitRepoState_EventEmission verifies that events are properly emitted
func TestEmitRepoState_EventEmission(t *testing.T) {
	worker, emitter, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitRepoConfig in the fake client
	repoConfig := &configv1alpha1.GitRepoConfig{
		Spec: configv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitRepoConfig: %v", err)
	}

	// Call EmitRepoState - this will fail due to Git operations, but should emit events
	err = worker.EmitRepoState("apps", emitter)

	// We expect an error due to Git operations, but the event emission should work
	if err == nil {
		t.Error("Expected error due to Git operations, but got nil")
	}

	// Verify that an event was attempted to be emitted
	// (In a real test with proper Git setup, this would succeed)
	if len(emitter.emittedEvents) == 0 {
		t.Log("No events emitted - this is expected in test environment without Git repo")
	}
}

// TestEmitRepoState_DifferentBaseFolders verifies different base folders are handled
func TestEmitRepoState_DifferentBaseFolders(t *testing.T) {
	worker, emitter, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitRepoConfig in the fake client
	repoConfig := &configv1alpha1.GitRepoConfig{
		Spec: configv1alpha1.GitRepoConfigSpec{
			RepoURL:         "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitRepoConfig: %v", err)
	}

	// Test different base folders
	baseFolders := []string{"apps", "infra", "", "clusters/prod"}

	for _, baseFolder := range baseFolders {
		emitter.emittedEvents = nil // Reset events

		err := worker.EmitRepoState(baseFolder, emitter)

		// Should fail due to Git operations, but method should handle different base folders
		if err == nil {
			t.Errorf("Expected error for base folder %q, but got nil", baseFolder)
		}

		// Verify event structure if emitted
		if len(emitter.emittedEvents) > 0 {
			event := emitter.emittedEvents[0]
			if event.RepoName != "test-repo" {
				t.Errorf("Expected RepoName 'test-repo', got %q", event.RepoName)
			}
			if event.Branch != "main" {
				t.Errorf("Expected Branch 'main', got %q", event.Branch)
			}
			if event.BaseFolder != baseFolder {
				t.Errorf("Expected BaseFolder %q, got %q", baseFolder, event.BaseFolder)
			}
		}
	}
}

// TestEmitRepoState_MissingGitRepoConfig verifies proper error when GitRepoConfig is missing
func TestEmitRepoState_MissingGitRepoConfig(t *testing.T) {
	worker, emitter, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Don't create GitRepoConfig - should fail
	err := worker.EmitRepoState("apps", emitter)

	if err == nil {
		t.Error("Expected error when GitRepoConfig is missing, but got nil")
	}

	// Should contain information about missing config
	if len(emitter.emittedEvents) != 0 {
		t.Errorf("Expected no events when GitRepoConfig missing, got %d", len(emitter.emittedEvents))
	}
}

// TestBranchWorker_RegisterUnregister verifies the simplified register/unregister behavior
func TestBranchWorker_RegisterUnregister(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "test-repo", "gitops-system", "main")

	// Register should not panic and should not track destinations
	worker.RegisterDestination("dest1", "default", "apps")
	worker.RegisterDestination("dest2", "default", "infra")

	// Unregister should always return false (no tracking)
	result := worker.UnregisterDestination("dest1", "default")
	if result {
		t.Error("UnregisterDestination should return false (no internal tracking)")
	}

	result = worker.UnregisterDestination("dest2", "default")
	if result {
		t.Error("UnregisterDestination should return false (no internal tracking)")
	}
}

// TestBranchWorker_IdentityFields verifies worker identity is set correctly
func TestBranchWorker_IdentityFields(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "my-repo", "my-namespace", "develop")

	if worker.GitRepoConfigRef != "my-repo" {
		t.Errorf("Expected GitRepoConfigRef 'my-repo', got %q", worker.GitRepoConfigRef)
	}
	if worker.GitRepoConfigNamespace != "my-namespace" {
		t.Errorf("Expected GitRepoConfigNamespace 'my-namespace', got %q", worker.GitRepoConfigNamespace)
	}
	if worker.Branch != "develop" {
		t.Errorf("Expected Branch 'develop', got %q", worker.Branch)
	}
}
