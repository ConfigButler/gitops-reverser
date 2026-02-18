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
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// WorkerManager manages BranchWorkers.
// Creates workers per (repo, branch), shared by multiple GitDestinations.
// Implements controller-runtime's Runnable interface for lifecycle management.
type WorkerManager struct {
	Client client.Client
	Log    logr.Logger

	mu      sync.RWMutex
	workers map[BranchKey]*BranchWorker
	ctx     context.Context
}

// NewWorkerManager creates a new worker manager.
func NewWorkerManager(client client.Client, log logr.Logger) *WorkerManager {
	return &WorkerManager{
		Client:  client,
		Log:     log,
		workers: make(map[BranchKey]*BranchWorker),
	}
}

// RegisterTarget ensures a worker exists for the target's (provider, branch)
// and registers the target with that worker.
// This is called by GitTarget controller when a target becomes Ready.
func (m *WorkerManager) RegisterTarget(
	_ context.Context,
	targetName, targetNamespace string,
	providerName, providerNamespace string,
	branch, path string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := BranchKey{
		RepoNamespace: providerNamespace,
		RepoName:      providerName,
		Branch:        branch,
	}

	// Get or create worker for this (provider, branch)
	if _, exists := m.workers[key]; !exists {
		m.Log.Info("Creating new branch worker", "key", key.String())
		worker := NewBranchWorker(
			m.Client,
			m.Log.WithName("branch-worker"),
			providerName,
			providerNamespace,
			branch,
			newContentWriter(),
		)

		if err := worker.Start(m.ctx); err != nil {
			return fmt.Errorf("failed to start worker for %s: %w", key.String(), err)
		}

		m.workers[key] = worker
	}

	worker := m.workers[key]
	if err := worker.EnsurePathBootstrapped(path, targetName, targetNamespace); err != nil {
		return fmt.Errorf("failed to ensure path bootstrap for %s/%s: %w", targetNamespace, targetName, err)
	}

	m.Log.Info("GitTarget registered with branch worker",
		"target", fmt.Sprintf("%s/%s", targetNamespace, targetName),
		"workerKey", key.String(),
		"path", path)

	return nil
}

// UnregisterTarget removes a GitTarget from its worker.
// Destroys the worker if it was the last target using it.
// This is called by GitTarget controller when a target is deleted.
func (m *WorkerManager) UnregisterTarget(
	_, _ string,
	providerName, providerNamespace string,
	branch string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := BranchKey{
		RepoNamespace: providerNamespace,
		RepoName:      providerName,
		Branch:        branch,
	}

	worker, exists := m.workers[key]
	if !exists {
		return nil
	}

	// Worker no longer tracks targets internally - always destroy worker
	// since WorkerManager handles all lifecycle decisions
	m.Log.Info("Unregistering target, destroying worker", "key", key.String())
	worker.Stop()
	delete(m.workers, key)

	return nil
}

// GetWorkerForTarget finds the worker for a target's (provider, branch).
// Returns the worker and true if found, nil and false otherwise.
// This is used by EventRouter to dispatch events to the correct worker.
func (m *WorkerManager) GetWorkerForTarget(
	providerName, providerNamespace string,
	branch string,
) (*BranchWorker, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := BranchKey{
		RepoNamespace: providerNamespace,
		RepoName:      providerName,
		Branch:        branch,
	}

	worker, exists := m.workers[key]
	return worker, exists
}

// ReconcileWorkers checks active GitTargets and cleans up orphaned workers.
// This ensures workers are removed when their GitTargets are deleted.
func (m *WorkerManager) ReconcileWorkers(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get all GitTargets
	var targetList configv1alpha1.GitTargetList
	if err := m.Client.List(ctx, &targetList); err != nil {
		return fmt.Errorf("failed to list GitTargets: %w", err)
	}

	// Build set of needed workers from active GitTargets
	neededWorkers := make(map[BranchKey]bool)
	for _, target := range targetList.Items {
		// Skip deleted targets
		if !target.DeletionTimestamp.IsZero() {
			continue
		}

		// Determine namespace (Provider is always in same namespace as Target)
		providerNS := target.Namespace

		key := BranchKey{
			RepoNamespace: providerNS,
			RepoName:      target.Spec.ProviderRef.Name,
			Branch:        target.Spec.Branch,
		}
		neededWorkers[key] = true
	}

	// Cleanup orphaned workers
	for key, worker := range m.workers {
		if !neededWorkers[key] {
			m.Log.Info("Cleaning up orphaned worker", "key", key.String())
			worker.Stop()
			delete(m.workers, key)
		}
	}

	m.Log.V(1).Info("Worker reconciliation complete",
		"activeWorkers", len(m.workers),
		"neededWorkers", len(neededWorkers))

	return nil
}

// Start implements manager.Runnable interface.
// This is called by controller-runtime when the manager starts.
func (m *WorkerManager) Start(ctx context.Context) error {
	m.ctx = ctx
	m.Log.Info("WorkerManager started")

	<-ctx.Done()

	m.Log.Info("WorkerManager shutting down")
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop all workers gracefully
	for key, worker := range m.workers {
		m.Log.Info("Stopping worker for shutdown", "key", key.String())
		worker.Stop()
	}

	m.workers = make(map[BranchKey]*BranchWorker)
	m.Log.Info("WorkerManager stopped")
	return nil
}

// NeedLeaderElection ensures only the elected leader manages workers.
// This prevents multiple pods from managing the same workers.
func (m *WorkerManager) NeedLeaderElection() bool {
	return true
}
