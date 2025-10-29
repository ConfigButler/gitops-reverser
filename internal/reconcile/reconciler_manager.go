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
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
)

// ReconcilerKey uniquely identifies a BaseFolderReconciler.
type ReconcilerKey struct {
	RepoName   string
	Branch     string
	BaseFolder string
}

// String returns a string representation for logging and debugging.
func (k ReconcilerKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.RepoName, k.Branch, k.BaseFolder)
}

// ReconcilerManager manages the lifecycle of BaseFolderReconciler instances.
type ReconcilerManager struct {
	reconcilers  map[ReconcilerKey]*BaseFolderReconciler
	eventEmitter events.EventEmitter
	logger       logr.Logger
}

// NewReconcilerManager creates a new ReconcilerManager.
func NewReconcilerManager(eventEmitter events.EventEmitter, logger logr.Logger) *ReconcilerManager {
	return &ReconcilerManager{
		reconcilers:  make(map[ReconcilerKey]*BaseFolderReconciler),
		eventEmitter: eventEmitter,
		logger:       logger,
	}
}

// CreateReconciler creates or retrieves a BaseFolderReconciler for the given scope.
func (m *ReconcilerManager) CreateReconciler(
	repoName, branch, baseFolder string,
) *BaseFolderReconciler {
	key := ReconcilerKey{
		RepoName:   repoName,
		Branch:     branch,
		BaseFolder: baseFolder,
	}

	if reconciler, exists := m.reconcilers[key]; exists {
		m.logger.V(1).Info("Reconciler already exists", "key", key.String())
		return reconciler
	}

	reconciler := NewBaseFolderReconciler(repoName, branch, baseFolder, m.eventEmitter, m.logger)
	m.reconcilers[key] = reconciler

	m.logger.Info("Created new BaseFolderReconciler", "key", key.String())
	return reconciler
}

// GetReconciler retrieves a BaseFolderReconciler for the given scope.
func (m *ReconcilerManager) GetReconciler(repoName, branch, baseFolder string) (*BaseFolderReconciler, bool) {
	key := ReconcilerKey{
		RepoName:   repoName,
		Branch:     branch,
		BaseFolder: baseFolder,
	}

	reconciler, exists := m.reconcilers[key]
	return reconciler, exists
}

// DeleteReconciler removes a BaseFolderReconciler from management.
func (m *ReconcilerManager) DeleteReconciler(repoName, branch, baseFolder string) bool {
	key := ReconcilerKey{
		RepoName:   repoName,
		Branch:     branch,
		BaseFolder: baseFolder,
	}

	if _, exists := m.reconcilers[key]; !exists {
		m.logger.V(1).Info("Reconciler not found for deletion", "key", key.String())
		return false
	}

	delete(m.reconcilers, key)
	m.logger.Info("Deleted BaseFolderReconciler", "key", key.String())
	return true
}

// ListReconcilers returns all managed reconcilers.
func (m *ReconcilerManager) ListReconcilers() []*BaseFolderReconciler {
	var reconcilers []*BaseFolderReconciler
	for _, reconciler := range m.reconcilers {
		reconcilers = append(reconcilers, reconciler)
	}
	return reconcilers
}

// CountReconcilers returns the number of managed reconcilers.
func (m *ReconcilerManager) CountReconcilers() int {
	return len(m.reconcilers)
}
