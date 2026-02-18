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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	return scheme
}

func createProviderWithLocalRepo(
	t *testing.T,
	ctx context.Context,
	k8sClient client.Client,
	namespace, name string,
) {
	t.Helper()

	remotePath := filepath.Join(t.TempDir(), name+".git")
	createBareRepo(t, remotePath)

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{
			URL: "file://" + remotePath,
		},
	}
	provider.Name = name
	provider.Namespace = namespace
	require.NoError(t, k8sClient.Create(ctx, provider))
}

func createTargetForRegister(
	t *testing.T,
	ctx context.Context,
	k8sClient client.Client,
	namespace, name, providerName, branch, path string,
) {
	t.Helper()
	target := &configv1alpha1.GitTarget{}
	target.Name = name
	target.Namespace = namespace
	target.Spec.ProviderRef = configv1alpha1.GitProviderReference{
		Kind: "GitProvider",
		Name: providerName,
	}
	target.Spec.Branch = branch
	target.Spec.Path = path
	require.NoError(t, k8sClient.Create(ctx, target))
}

// TestWorkerManagerRegisterTarget verifies worker registration.
func TestWorkerManagerRegisterTarget(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start manager
	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // Allow manager to start
	createProviderWithLocalRepo(t, ctx, client, "gitops-system", "repo1")
	createTargetForRegister(t, ctx, client, "default", "target1", "repo1", "main", "clusters/prod")

	// Register first target
	err := manager.RegisterTarget(ctx,
		"target1", "default",
		"repo1", "gitops-system",
		"main", "clusters/prod")
	if err != nil {
		t.Fatalf("Failed to register target: %v", err)
	}

	// Verify worker was created
	worker, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist after registration")
	}
	if worker == nil {
		t.Fatal("Worker should not be nil")
	}

	// Verify worker has correct identity
	if worker.GitProviderRef != "repo1" {
		t.Errorf("Worker RepoRef = %q, want 'repo1'", worker.GitProviderRef)
	}
	if worker.GitProviderNamespace != "gitops-system" {
		t.Errorf("Worker Namespace = %q, want 'gitops-system'", worker.GitProviderNamespace)
	}
	if worker.Branch != "main" {
		t.Errorf("Worker Branch = %q, want 'main'", worker.Branch)
	}

	// Verify target registration succeeded (no longer tracks internally)
	// The worker exists and registration completed without error

	// Cleanup
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerMultipleTargetsSameBranch verifies multiple targets can share a worker.
func TestWorkerManagerMultipleTargetsSameBranch(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(t, ctx, client, "gitops-system", "shared-repo")
	createTargetForRegister(t, ctx, client, "default", "target-apps", "shared-repo", "main", "apps/")
	createTargetForRegister(t, ctx, client, "default", "target-infra", "shared-repo", "main", "infra/")

	// Register two targets for same repo+branch, different paths
	err := manager.RegisterTarget(ctx,
		"target-apps", "default",
		"shared-repo", "gitops-system",
		"main", "apps/")
	if err != nil {
		t.Fatalf("Failed to register target-apps: %v", err)
	}

	err = manager.RegisterTarget(ctx,
		"target-infra", "default",
		"shared-repo", "gitops-system",
		"main", "infra/")
	if err != nil {
		t.Fatalf("Failed to register target-infra: %v", err)
	}

	// Verify only one worker exists
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker for shared repo+branch, got %d", workerCount)
	}

	// Verify worker exists for both targets
	_, exists := manager.GetWorkerForTarget("shared-repo", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerDifferentBranches verifies different branches get different workers.
func TestWorkerManagerDifferentBranches(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(t, ctx, client, "gitops-system", "repo1")
	createTargetForRegister(t, ctx, client, "default", "target-main", "repo1", "main", "base/")
	createTargetForRegister(t, ctx, client, "default", "target-dev", "repo1", "develop", "base/")

	// Register targets for same repo, different branches
	err := manager.RegisterTarget(ctx,
		"target-main", "default",
		"repo1", "gitops-system",
		"main", "base/")
	if err != nil {
		t.Fatalf("Failed to register target-main: %v", err)
	}

	err = manager.RegisterTarget(ctx,
		"target-dev", "default",
		"repo1", "gitops-system",
		"develop", "base/")
	if err != nil {
		t.Fatalf("Failed to register target-dev: %v", err)
	}

	// Verify two workers exist
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 2 {
		t.Errorf("Should have 2 workers for different branches, got %d", workerCount)
	}

	// Verify each worker exists and has correct branch
	workerMain, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if !exists || workerMain.Branch != "main" {
		t.Error("Main branch worker not found or has wrong branch")
	}

	workerDev, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "develop")
	if !exists || workerDev.Branch != "develop" {
		t.Error("Develop branch worker not found or has wrong branch")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerUnregisterTarget verifies target unregistration.
func TestWorkerManagerUnregisterTarget(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(t, ctx, client, "gitops-system", "repo1")
	createTargetForRegister(t, ctx, client, "default", "target1", "repo1", "main", "apps/")
	createTargetForRegister(t, ctx, client, "default", "target2", "repo1", "main", "infra/")

	// Register two targets
	_ = manager.RegisterTarget(ctx,
		"target1", "default",
		"repo1", "gitops-system",
		"main", "apps/")
	_ = manager.RegisterTarget(ctx,
		"target2", "default",
		"repo1", "gitops-system",
		"main", "infra/")

	// Verify worker exists
	_, exists := manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist")
	}

	// Unregister first target
	err := manager.UnregisterTarget("target1", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to unregister target1: %v", err)
	}

	// Verify worker was destroyed (WorkerManager now destroys on any unregister)
	_, exists = manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if exists {
		t.Error("Worker should be destroyed when target unregistered")
	}

	// Unregister last target
	err = manager.UnregisterTarget("target2", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to unregister target2: %v", err)
	}

	// Verify worker was destroyed
	_, exists = manager.GetWorkerForTarget("repo1", "gitops-system", "main")
	if exists {
		t.Error("Worker should be destroyed when last target unregistered")
	}

	manager.mu.RLock()
	finalWorkerCount := len(manager.workers)
	manager.mu.RUnlock()

	if finalWorkerCount != 0 {
		t.Errorf("Manager should have 0 workers, got %d", finalWorkerCount)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerConcurrentRegistration verifies thread safety.
func TestWorkerManagerConcurrentRegistration(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	createProviderWithLocalRepo(t, ctx, client, "gitops-system", "repo1")
	createTargetForRegister(t, ctx, client, "default", "target", "repo1", "main", "base/")

	// Concurrently register multiple targets
	done := make(chan bool, 10)
	for i := range 10 {
		go func(index int) {
			targetName := "target"
			err := manager.RegisterTarget(ctx,
				targetName, "default",
				"repo1", "gitops-system",
				"main", "base/")
			if err != nil {
				t.Errorf("Failed to register target %d: %v", index, err)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}

	// Verify only one worker was created (same repo+branch)
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker despite concurrent registration, got %d", workerCount)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerGetNonexistentWorker verifies getting nonexistent worker returns false.
func TestWorkerManagerGetNonexistentWorker(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)

	worker, exists := manager.GetWorkerForTarget("nonexistent", "default", "main")
	if exists {
		t.Error("Should return exists=false for nonexistent worker")
	}
	if worker != nil {
		t.Error("Worker should be nil for nonexistent key")
	}
}

// TestWorkerManagerUnregisterNonexistent verifies unregistering nonexistent target is safe.
func TestWorkerManagerUnregisterNonexistent(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)

	// Unregister should be idempotent and not error
	err := manager.UnregisterTarget("nonexistent", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Errorf("Unregister nonexistent should not error: %v", err)
	}
}
