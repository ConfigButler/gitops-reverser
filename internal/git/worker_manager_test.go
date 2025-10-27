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
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = configv1alpha1.AddToScheme(scheme)
	return scheme
}

// TestWorkerManagerRegisterDestination verifies worker registration.
func TestWorkerManagerRegisterDestination(t *testing.T) {
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

	// Register first destination
	err := manager.RegisterDestination(ctx,
		"dest1", "default",
		"repo1", "gitops-system",
		"main", "clusters/prod")
	if err != nil {
		t.Fatalf("Failed to register destination: %v", err)
	}

	// Verify worker was created
	worker, exists := manager.GetWorkerForDestination("repo1", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist after registration")
	}
	if worker == nil {
		t.Fatal("Worker should not be nil")
	}

	// Verify worker has correct identity
	if worker.GitRepoConfigRef != "repo1" {
		t.Errorf("Worker RepoRef = %q, want 'repo1'", worker.GitRepoConfigRef)
	}
	if worker.GitRepoConfigNamespace != "gitops-system" {
		t.Errorf("Worker Namespace = %q, want 'gitops-system'", worker.GitRepoConfigNamespace)
	}
	if worker.Branch != "main" {
		t.Errorf("Worker Branch = %q, want 'main'", worker.Branch)
	}

	// Verify destination is tracked
	worker.destMu.RLock()
	destCount := len(worker.activeDestinations)
	worker.destMu.RUnlock()

	if destCount != 1 {
		t.Errorf("Worker should track 1 destination, got %d", destCount)
	}

	// Cleanup
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerMultipleDestinationsSameBranch verifies multiple destinations can share a worker.
func TestWorkerManagerMultipleDestinationsSameBranch(t *testing.T) {
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

	// Register two destinations for same repo+branch, different baseFolders
	err := manager.RegisterDestination(ctx,
		"dest-apps", "default",
		"shared-repo", "gitops-system",
		"main", "apps/")
	if err != nil {
		t.Fatalf("Failed to register dest-apps: %v", err)
	}

	err = manager.RegisterDestination(ctx,
		"dest-infra", "default",
		"shared-repo", "gitops-system",
		"main", "infra/")
	if err != nil {
		t.Fatalf("Failed to register dest-infra: %v", err)
	}

	// Verify only one worker exists
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 1 {
		t.Errorf("Should have exactly 1 worker for shared repo+branch, got %d", workerCount)
	}

	// Verify worker tracks both destinations
	worker, exists := manager.GetWorkerForDestination("shared-repo", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist")
	}

	worker.destMu.RLock()
	destCount := len(worker.activeDestinations)
	worker.destMu.RUnlock()

	if destCount != 2 {
		t.Errorf("Worker should track 2 destinations, got %d", destCount)
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

	// Register destinations for same repo, different branches
	err := manager.RegisterDestination(ctx,
		"dest-main", "default",
		"repo1", "gitops-system",
		"main", "base/")
	if err != nil {
		t.Fatalf("Failed to register dest-main: %v", err)
	}

	err = manager.RegisterDestination(ctx,
		"dest-dev", "default",
		"repo1", "gitops-system",
		"develop", "base/")
	if err != nil {
		t.Fatalf("Failed to register dest-dev: %v", err)
	}

	// Verify two workers exist
	manager.mu.RLock()
	workerCount := len(manager.workers)
	manager.mu.RUnlock()

	if workerCount != 2 {
		t.Errorf("Should have 2 workers for different branches, got %d", workerCount)
	}

	// Verify each worker exists and has correct branch
	workerMain, exists := manager.GetWorkerForDestination("repo1", "gitops-system", "main")
	if !exists || workerMain.Branch != "main" {
		t.Error("Main branch worker not found or has wrong branch")
	}

	workerDev, exists := manager.GetWorkerForDestination("repo1", "gitops-system", "develop")
	if !exists || workerDev.Branch != "develop" {
		t.Error("Develop branch worker not found or has wrong branch")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWorkerManagerUnregisterDestination verifies destination unregistration.
func TestWorkerManagerUnregisterDestination(t *testing.T) {
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

	// Register two destinations
	_ = manager.RegisterDestination(ctx,
		"dest1", "default",
		"repo1", "gitops-system",
		"main", "apps/")
	_ = manager.RegisterDestination(ctx,
		"dest2", "default",
		"repo1", "gitops-system",
		"main", "infra/")

	// Verify worker exists with 2 destinations
	worker, exists := manager.GetWorkerForDestination("repo1", "gitops-system", "main")
	if !exists {
		t.Fatal("Worker should exist")
	}

	worker.destMu.RLock()
	initialCount := len(worker.activeDestinations)
	worker.destMu.RUnlock()

	if initialCount != 2 {
		t.Errorf("Worker should have 2 destinations, got %d", initialCount)
	}

	// Unregister first destination
	err := manager.UnregisterDestination("dest1", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to unregister dest1: %v", err)
	}

	// Verify worker still exists (still has dest2)
	worker, exists = manager.GetWorkerForDestination("repo1", "gitops-system", "main")
	if !exists {
		t.Error("Worker should still exist with remaining destination")
	}

	worker.destMu.RLock()
	remainingCount := len(worker.activeDestinations)
	worker.destMu.RUnlock()

	if remainingCount != 1 {
		t.Errorf("Worker should have 1 destination after unregister, got %d", remainingCount)
	}

	// Unregister last destination
	err = manager.UnregisterDestination("dest2", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Fatalf("Failed to unregister dest2: %v", err)
	}

	// Verify worker was destroyed
	_, exists = manager.GetWorkerForDestination("repo1", "gitops-system", "main")
	if exists {
		t.Error("Worker should be destroyed when last destination unregistered")
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

	// Concurrently register multiple destinations
	done := make(chan bool, 10)
	for i := range 10 {
		go func(index int) {
			destName := "dest"
			err := manager.RegisterDestination(ctx,
				destName, "default",
				"repo1", "gitops-system",
				"main", "base/")
			if err != nil {
				t.Errorf("Failed to register destination %d: %v", index, err)
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

	worker, exists := manager.GetWorkerForDestination("nonexistent", "default", "main")
	if exists {
		t.Error("Should return exists=false for nonexistent worker")
	}
	if worker != nil {
		t.Error("Worker should be nil for nonexistent key")
	}
}

// TestWorkerManagerUnregisterNonexistent verifies unregistering nonexistent destination is safe.
func TestWorkerManagerUnregisterNonexistent(t *testing.T) {
	scheme := setupScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	log := logr.Discard()

	manager := NewWorkerManager(client, log)

	// Unregister should be idempotent and not error
	err := manager.UnregisterDestination("nonexistent", "default", "repo1", "gitops-system", "main")
	if err != nil {
		t.Errorf("Unregister nonexistent should not error: %v", err)
	}
}
