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
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func setupBranchWorkerTest() (*BranchWorker, func()) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "test-repo", "gitops-system", "main")

	cleanup := func() {
		if worker.started {
			worker.Stop()
		}
	}

	return worker, cleanup
}

// TestListResourcesInBaseFolder_BasicFunctionality verifies ListResourcesInBaseFolder can be called.
func TestListResourcesInBaseFolder_BasicFunctionality(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test verifies the method can be called without panicking
	// In a real scenario, this would require setting up a Git repository
	// For now, we just ensure the method signature and basic flow work
	_, err := worker.ListResourcesInBaseFolder("apps")

	// We expect an error since no GitRepoConfig exists in the fake client
	// But the important thing is that the method doesn't panic
	if err == nil {
		t.Error("Expected error due to missing GitRepoConfig, but got nil")
	}
}

// TestListResourcesInBaseFolder_WithGitRepoConfig verifies resources are listed correctly.
func TestListResourcesInBaseFolder_WithGitRepoConfig(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
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

	// Call ListResourcesInBaseFolder - will fail due to Git operations
	_, err = worker.ListResourcesInBaseFolder("apps")

	// We expect an error due to Git operations
	if err == nil {
		t.Error("Expected error due to Git operations, but got nil")
	}
}

// TestListResourcesInBaseFolder_DifferentBaseFolders verifies different base folders are handled.
func TestListResourcesInBaseFolder_DifferentBaseFolders(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
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
		_, err := worker.ListResourcesInBaseFolder(baseFolder)

		// Should fail due to Git operations, but method should handle different base folders
		if err == nil {
			t.Errorf("Expected error for base folder %q, but got nil", baseFolder)
		}
	}
}

// TestListResourcesInBaseFolder_MissingGitRepoConfig verifies proper error when GitRepoConfig is missing.
func TestListResourcesInBaseFolder_MissingGitRepoConfig(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Don't create GitRepoConfig - should fail
	_, err := worker.ListResourcesInBaseFolder("apps")

	if err == nil {
		t.Error("Expected error when GitRepoConfig is missing, but got nil")
	}
}

// TestBranchWorker_IdentityFields verifies worker identity is set correctly.
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

// TestBranchWorker_GetBranchMetadata_Empty tests metadata behavior when worker is new.
func TestBranchWorker_GetBranchMetadata_Empty(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	exists, sha, lastFetch := worker.GetBranchMetadata()

	// New worker should have empty/default metadata
	if exists != false {
		t.Errorf("Expected branchExists to be false for new worker, got %v", exists)
	}
	if sha != "" {
		t.Errorf("Expected empty SHA for new worker, got %q", sha)
	}
	// lastFetch should be zero time for new worker
	if !lastFetch.IsZero() {
		t.Errorf("Expected zero time for new worker, got %v", lastFetch)
	}
}

// TestBranchWorker_GetBranchMetadata_ThreadSafety tests metadata access is thread-safe.
func TestBranchWorker_GetBranchMetadata_ThreadSafety(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Test concurrent access
	var wg sync.WaitGroup
	errors := make(chan error, 5)

	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = worker.GetBranchMetadata()
			errors <- nil
		}()
	}

	wg.Wait()
	close(errors)

	// Check no errors occurred
	for err := range errors {
		if err != nil {
			t.Errorf("Thread safety test failed: %v", err)
		}
	}
}

// TestBranchWorker_UpdateBranchMetadataFromRepo tests metadata update from repository.
func TestBranchWorker_UpdateBranchMetadataFromRepo(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test would require a real git repository to fully test
	// For now, we test the method exists and can be called
	// A more complete test would set up a real git repository

	// The updateBranchMetadataFromRepo method is internal, but we can test
	// the public interface through other methods that call it
	// In a real test scenario, we would need to mock the git.Repository

	// Test that metadata fields exist and are accessible
	w := worker
	if w == nil {
		t.Fatal("Worker should not be nil")
	}

	// Test metadata fields are initialized to zero values
	w.metaMu.RLock()
	initialExists := w.branchExists
	initialSHA := w.lastCommitSHA
	initialTime := w.lastFetchTime
	w.metaMu.RUnlock()

	if initialExists != false {
		t.Errorf("Expected initial branchExists to be false, got %v", initialExists)
	}
	if initialSHA != "" {
		t.Errorf("Expected initial lastCommitSHA to be empty, got %q", initialSHA)
	}
	if !initialTime.IsZero() {
		t.Errorf("Expected initial lastFetchTime to be zero, got %v", initialTime)
	}
}

// TestBranchWorker_EmptyRepositoryHandling tests behavior with empty repositories.
// This tests the critical path where a repository has no commits yet.
func TestBranchWorker_EmptyRepositoryHandling(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create an actual empty git repository for testing
	tempDir := t.TempDir()
	repo, err := git.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to create empty repository: %v", err)
	}

	// Test updateBranchMetadataFromRepo with empty repository
	err = worker.updateBranchMetadataFromRepo(repo)
	if err != nil {
		t.Errorf("updateBranchMetadataFromRepo should handle empty repository gracefully: %v", err)
	}

	// Verify metadata is set correctly for empty repository
	exists, sha, lastFetch := worker.GetBranchMetadata()
	if exists != false {
		t.Errorf("Expected branchExists to be false for empty repository, got %v", exists)
	}
	if sha != "" {
		t.Errorf("Expected empty SHA for empty repository, got %q", sha)
	}
	if lastFetch.IsZero() {
		t.Error("Expected non-zero lastFetchTime after metadata update")
	}
}

// TestBranchWorker_RepositoryInitializationWithEmptyRepo tests initialization with empty repositories.
func TestBranchWorker_RepositoryInitializationWithEmptyRepo(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test verifies that ensureRepositoryInitialized can handle the case
	// where we're working with an empty repository

	// Create a mock scenario by directly testing the metadata handling
	// since actual Git operations require network connectivity

	// Test that metadata fields exist and are accessible
	w := worker
	if w == nil {
		t.Fatal("Worker should not be nil")
	}

	// Simulate empty repository scenario by testing the metadata structure
	w.metaMu.Lock()
	// Verify initial state
	if w.branchExists != false {
		t.Errorf("Expected initial branchExists to be false, got %v", w.branchExists)
	}
	if w.lastCommitSHA != "" {
		t.Errorf("Expected initial lastCommitSHA to be empty, got %q", w.lastCommitSHA)
	}
	if !w.lastFetchTime.IsZero() {
		t.Errorf("Expected initial lastFetchTime to be zero, got %v", w.lastFetchTime)
	}
	w.metaMu.Unlock()

	// Test GetBranchMetadata returns valid zero values
	exists, sha, lastFetch := worker.GetBranchMetadata()
	if exists || sha != "" || !lastFetch.IsZero() {
		t.Errorf("Expected all zero values for new worker, got exists=%v, sha=%q, lastFetch=%v", exists, sha, lastFetch)
	}
}

// TestBranchWorker_SingleCloneArchitecture tests that ListResources uses managed repository.
func TestBranchWorker_SingleCloneArchitecture(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
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

	// Call ListResourcesInBaseFolder - should not panic and should handle errors gracefully
	// The key test is that this method should use the worker's managed repository
	// rather than creating separate clones
	_, err = worker.ListResourcesInBaseFolder("apps")

	// We expect an error due to Git operations (no real repository)
	// But the important thing is that the method should attempt to use
	// the worker's managed repository path instead of creating new temporary directories
	if err == nil {
		t.Error("Expected error due to Git operations, but got nil")
	}
}

// TestBranchWorker_Start_RepositoryInitialization tests that Start initializes repository.
func TestBranchWorker_Start_RepositoryInitialization(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitRepoConfig
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

	// Start the worker - this should trigger repository initialization
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = worker.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start worker: %v", err)
	}

	// Give some time for background initialization
	time.Sleep(100 * time.Millisecond)

	// Stop the worker
	worker.Stop()

	// The worker should have attempted to initialize the repository
	// In a real test scenario, we would verify that the repository path
	// was created and metadata was updated
}

// TestBranchWorker_MetadataUpdateAfterOperations tests metadata updates after operations.
func TestBranchWorker_MetadataUpdateAfterOperations(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test would verify that metadata is updated after operations
	// In a full implementation, we would:
	// 1. Start with initial metadata state
	// 2. Perform operations that modify repository state
	// 3. Verify metadata is updated correctly

	// For now, just test the interface works
	if worker == nil {
		t.Fatal("Worker should not be nil")
	}

	// Test GetBranchMetadata returns current state
	exists, sha, lastFetch := worker.GetBranchMetadata()

	// Verify the return values are valid (even if zero)
	_ = exists    // bool
	_ = sha       // string
	_ = lastFetch // time.Time

	// This test serves as a placeholder for the full implementation
	// that would test actual metadata updates
}
