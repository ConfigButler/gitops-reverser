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
	"context"
	"errors"
	"sync"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReconcilerManager manages the lifecycle of FolderReconciler instances.
type ReconcilerManager struct {
	mu          sync.RWMutex
	reconcilers map[string]*FolderReconciler // key = gitDest.Key() = "namespace/name"
	eventRouter interface {
		ProcessControlEvent(ctx context.Context, event events.ControlEvent) error
	}
	logger logr.Logger
}

// NewReconcilerManager creates a new ReconcilerManager.
func NewReconcilerManager(
	eventRouter interface {
		ProcessControlEvent(ctx context.Context, event events.ControlEvent) error
	},
	logger logr.Logger,
) *ReconcilerManager {
	return &ReconcilerManager{
		reconcilers: make(map[string]*FolderReconciler),
		eventRouter: eventRouter,
		logger:      logger,
	}
}

// SetEventRouter sets the control-event processor dependency after construction.
func (m *ReconcilerManager) SetEventRouter(
	eventRouter interface {
		ProcessControlEvent(ctx context.Context, event events.ControlEvent) error
	},
) {
	m.eventRouter = eventRouter
}

// CreateReconciler creates or retrieves a FolderReconciler for the given GitDestination.
func (m *ReconcilerManager) CreateReconciler(
	gitDest types.ResourceReference,
	eventEmitter EventEmitter,
) *FolderReconciler {
	key := gitDest.Key()

	m.mu.Lock()
	defer m.mu.Unlock()

	if reconciler, exists := m.reconcilers[key]; exists {
		m.logger.V(1).Info("Reconciler already exists", "gitDest", gitDest.String())
		return reconciler
	}

	reconciler := NewFolderReconciler(gitDest, eventEmitter, m, m.logger)
	m.reconcilers[key] = reconciler
	m.logger.Info("Created new FolderReconciler", "gitDest", gitDest.String())
	return reconciler
}

// GetReconciler retrieves a FolderReconciler for the given GitDestination.
func (m *ReconcilerManager) GetReconciler(gitDest types.ResourceReference) (*FolderReconciler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	reconciler, exists := m.reconcilers[gitDest.Key()]
	return reconciler, exists
}

// DeleteReconciler removes a FolderReconciler from management.
func (m *ReconcilerManager) DeleteReconciler(gitDest types.ResourceReference) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := gitDest.Key()
	if _, exists := m.reconcilers[key]; !exists {
		m.logger.V(1).Info("Reconciler not found", "gitDest", gitDest.String())
		return false
	}
	delete(m.reconcilers, key)
	m.logger.Info("Deleted FolderReconciler", "gitDest", gitDest.String())
	return true
}

// EmitControlEvent implements ControlEventEmitter interface.
func (m *ReconcilerManager) EmitControlEvent(event events.ControlEvent) error {
	if m.eventRouter == nil {
		return errors.New("eventRouter not set")
	}
	return m.eventRouter.ProcessControlEvent(context.Background(), event)
}

// ListReconcilers returns all managed reconcilers.
func (m *ReconcilerManager) ListReconcilers() []*FolderReconciler {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var reconcilers []*FolderReconciler
	for _, reconciler := range m.reconcilers {
		reconcilers = append(reconcilers, reconciler)
	}
	return reconcilers
}

// CountReconcilers returns the number of managed reconcilers.
func (m *ReconcilerManager) CountReconcilers() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.reconcilers)
}
