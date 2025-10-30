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

// RegisterDestination ensures a worker exists for the destination's (repo, branch)
// and registers the destination with that worker.
// This is called by GitDestination controller when a destination becomes Ready.
func (m *WorkerManager) RegisterDestination(
	_ context.Context,
	destName string, destNamespace string,
	repoName, repoNamespace string,
	branch, baseFolder string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := BranchKey{
		RepoNamespace: repoNamespace,
		RepoName:      repoName,
		Branch:        branch,
	}

	// Get or create worker for this (repo, branch)
	worker, exists := m.workers[key]
	if !exists {
		m.Log.Info("Creating new branch worker", "key", key.String())
		worker = NewBranchWorker(m.Client, m.Log.WithName("branch-worker"),
			repoName, repoNamespace, branch)

		if err := worker.Start(m.ctx); err != nil {
			return fmt.Errorf("failed to start worker for %s: %w", key.String(), err)
		}

		m.workers[key] = worker
	}

	// Register this destination with the worker
	worker.RegisterDestination("", destNamespace, baseFolder)

	m.Log.Info("GitDestination registered with branch worker",
		"destination", fmt.Sprintf("%s/%s", destNamespace, ""),
		"workerKey", key.String(),
		"baseFolder", baseFolder)

	return nil
}

// UnregisterDestination removes a GitDestination from its worker.
// Destroys the worker if it was the last destination using it.
// This is called by GitDestination controller when a destination is deleted.
func (m *WorkerManager) UnregisterDestination(
	destName, destNamespace string,
	repoName, repoNamespace string,
	branch string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := BranchKey{
		RepoNamespace: repoNamespace,
		RepoName:      repoName,
		Branch:        branch,
	}

	worker, exists := m.workers[key]
	if !exists {
		// Worker already gone - this is fine (idempotent cleanup)
		return nil
	}

	// Worker no longer tracks destinations internally - always destroy worker
	// since WorkerManager handles all lifecycle decisions
	m.Log.Info("Unregistering destination, destroying worker", "key", key.String())
	worker.Stop()
	delete(m.workers, key)

	return nil
}

// GetWorkerForDestination finds the worker for a destination's (repo, branch).
// Returns the worker and true if found, nil and false otherwise.
// This is used by EventRouter to dispatch events to the correct worker.
func (m *WorkerManager) GetWorkerForDestination(
	repoName, repoNamespace string,
	branch string,
) (*BranchWorker, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := BranchKey{
		RepoNamespace: repoNamespace,
		RepoName:      repoName,
		Branch:        branch,
	}

	worker, exists := m.workers[key]
	return worker, exists
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
