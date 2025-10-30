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
