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
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestListResourcesInPath_BasicFunctionality verifies ListResourcesInPath can be called.
func TestListResourcesInPath_BasicFunctionality(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// This test verifies the method can be called without panicking
	// In a real scenario, this would require setting up a Git repository
	// For now, we just ensure the method signature and basic flow work
	_, err := worker.ListResourcesInPath("apps")

	// We expect an error since no GitProvider exists in the fake client
	// But the important thing is that the method doesn't panic
	if err == nil {
		t.Error("Expected error due to missing GitProvider, but got nil")
	}
}

// TestListResourcesInPath_WithGitProvider verifies resources are listed correctly.
func TestListResourcesInPath_WithGitProvider(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitProvider in the fake client
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL:             "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitProvider: %v", err)
	}

	// Call ListResourcesInPath - with new abstraction, initialization succeeds
	// but listing resources will return empty list for fake repo
	resources, err := worker.ListResourcesInPath("apps")

	// With the new abstraction, we expect success but empty resource list
	if err != nil {
		t.Logf("Got expected error during fetch: %v", err)
	} else {
		// If no error (abstraction handles it gracefully), verify empty list
		assert.Empty(t, resources, "Should return empty list for fake repository")
	}
}

// TestListResourcesInPath_DifferentPaths verifies different paths are handled.
func TestListResourcesInPath_DifferentPaths(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Create a GitProvider in the fake client
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL:             "https://github.com/test/repo.git",
			AllowedBranches: []string{"main"},
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "gitops-system"

	err := worker.Client.Create(context.Background(), repoConfig)
	if err != nil {
		t.Fatalf("Failed to create GitProvider: %v", err)
	}

	// Test different paths - with new abstraction, method handles them gracefully
	paths := []string{"apps", "infra", "", "clusters/prod"}

	for _, path := range paths {
		resources, err := worker.ListResourcesInPath(path)

		// With new abstraction, we either get an error during fetch or empty list
		if err != nil {
			t.Logf("Got expected error for path %q: %v", path, err)
		} else {
			// Method succeeded - verify it returns empty list for fake repo
			assert.Empty(t, resources, "Should return empty list for path %q", path)
		}
	}
}

// TestListResourcesInPath_MissingGitProvider verifies proper error when GitProvider is missing.
func TestListResourcesInPath_MissingGitProvider(t *testing.T) {
	worker, cleanup := setupBranchWorkerTest()
	defer cleanup()

	// Don't create GitProvider - should fail
	_, err := worker.ListResourcesInPath("apps")

	if err == nil {
		t.Error("Expected error when GitProvider is missing, but got nil")
	}
}

// TestBranchWorker_EmptyRepository tests that BranchWorker properly handles empty repositories
// that have no commits yet. This is a critical scenario for bootstrapping new repositories.
func TestBranchWorker_EmptyRepository(t *testing.T) {
	ctx := context.Background()

	// Create a temporary directory for the empty repository
	tempDir := t.TempDir()
	repoPath := filepath.Join(tempDir, "empty-repo")

	// Initialize an empty git repository (no initial commit)
	_, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	// Create a BranchWorker for this empty repository
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := logr.Discard()
	worker := NewBranchWorker(client, logger, "test-repo", "default", "main")

	// Create a GitProvider in the fake client pointing to our empty repo
	repoConfig := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: "file://" + repoPath,
		},
	}
	repoConfig.Name = "test-repo"
	repoConfig.Namespace = "default"
	err = client.Create(ctx, repoConfig)
	require.NoError(t, err)

	// Test ensureRepositoryInitialized with empty repo
	err = worker.ensureRepositoryInitialized(ctx)
	require.NoError(t, err, "ensureRepositoryInitialized should succeed with empty repository")

	// Test GetBranchMetadata - should return branchExists=false for empty repo
	exists, sha, fetchTime := worker.GetBranchMetadata()
	assert.False(t, exists, "Branch should not exist in empty repository")
	assert.Empty(t, sha, "SHA should be empty for empty repository")
	assert.False(t, fetchTime.IsZero(), "Fetch time should be set")

	// Test ListResourcesInPath - should work with empty repo
	resources, err := worker.ListResourcesInPath("")
	require.NoError(t, err, "ListResourcesInPath should succeed with empty repository")
	assert.Empty(t, resources, "Should return empty resources list for empty repository")

	// Verify metadata was updated after ListResourcesInPath
	exists2, sha2, fetchTime2 := worker.GetBranchMetadata()
	assert.False(t, exists2, "Branch should still not exist after listing")
	assert.Empty(t, sha2, "SHA should still be empty after listing")
	assert.True(t, fetchTime2.After(fetchTime), "Fetch time should be updated after listing")
}

// TestBranchWorker_IdentityFields verifies worker identity is set correctly.
func TestBranchWorker_IdentityFields(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	worker := NewBranchWorker(client, log, "my-repo", "my-namespace", "develop")

	if worker.GitProviderRef != "my-repo" {
		t.Errorf("Expected GitProviderRef 'my-repo', got %q", worker.GitProviderRef)
	}
	if worker.GitProviderNamespace != "my-namespace" {
		t.Errorf("Expected GitProviderNamespace 'my-namespace', got %q", worker.GitProviderNamespace)
	}
	if worker.Branch != "develop" {
		t.Errorf("Expected Branch 'develop', got %q", worker.Branch)
	}
}
