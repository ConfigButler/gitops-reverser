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

// Package reconcile provides components for cluster-as-source-of-truth reconciliation.
package reconcile

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// FolderReconciler reconciles Git base folder to match cluster state.
// It operates without time concerns (delegated to WatchManager) and focuses purely
// on reconciliation logic.
type FolderReconciler struct {
	gitDest types.ResourceReference

	// Current state snapshots
	clusterResources []types.ResourceIdentifier
	gitResources     []types.ResourceIdentifier

	// Dependencies for event emission
	eventEmitter   EventEmitter
	controlEmitter events.ControlEventEmitter
	logger         logr.Logger
}

// NewFolderReconciler creates a new FolderReconciler.
func NewFolderReconciler(
	gitDest types.ResourceReference,
	eventEmitter EventEmitter,
	controlEmitter events.ControlEventEmitter,
	logger logr.Logger,
) *FolderReconciler {
	return &FolderReconciler{
		gitDest:        gitDest,
		eventEmitter:   eventEmitter,
		controlEmitter: controlEmitter,
		logger:         logger.WithValues("gitDest", gitDest.String()),
	}
}

// StartReconciliation initiates the reconciliation process by requesting state.
func (r *FolderReconciler) StartReconciliation(_ context.Context) error {
	r.logger.Info("Starting reconciliation")

	// Emit control events to request both cluster and repo state
	if err := r.controlEmitter.EmitControlEvent(events.ControlEvent{
		Type:    events.RequestClusterState,
		GitDest: r.gitDest,
	}); err != nil {
		return fmt.Errorf("failed to emit RequestClusterState: %w", err)
	}

	if err := r.controlEmitter.EmitControlEvent(events.ControlEvent{
		Type:    events.RequestRepoState,
		GitDest: r.gitDest,
	}); err != nil {
		return fmt.Errorf("failed to emit RequestRepoState: %w", err)
	}

	return nil
}

// OnClusterState handles cluster state events and triggers reconciliation.
func (r *FolderReconciler) OnClusterState(event events.ClusterStateEvent) {
	if !event.GitDest.Equal(r.gitDest) {
		return
	}
	r.clusterResources = event.Resources
	r.logger.V(1).Info("Received cluster state", "resourceCount", len(event.Resources))
	r.reconcile()
}

// OnRepoState handles repository state events and triggers reconciliation.
func (r *FolderReconciler) OnRepoState(event events.RepoStateEvent) {
	if !event.GitDest.Equal(r.gitDest) {
		return
	}
	r.gitResources = event.Resources
	r.logger.V(1).Info("Received repo state", "resourceCount", len(event.Resources))
	r.reconcile()
}

// reconcile performs the reconciliation logic when both states are available.
func (r *FolderReconciler) reconcile() {
	// Only reconcile when we have both cluster and Git state
	if r.clusterResources == nil || r.gitResources == nil {
		return
	}

	// Compute reconciliation actions (pure logic, no time concerns)
	toCreate, toDelete, existingInBoth := r.findDifferences(r.clusterResources, r.gitResources)

	r.logger.V(1).Info("Reconciliation computed",
		"toCreate", len(toCreate),
		"toDelete", len(toDelete),
		"existingInBoth", len(existingInBoth))

	// Emit reconciliation events (time filtering happens in WatchManager)
	for _, resource := range toCreate {
		if err := r.eventEmitter.EmitCreateEvent(resource); err != nil {
			r.logger.Error(err, "Failed to emit create event", "resource", resource.String())
		}
	}

	for _, resource := range toDelete {
		if err := r.eventEmitter.EmitDeleteEvent(resource); err != nil {
			r.logger.Error(err, "Failed to emit delete event", "resource", resource.String())
		}
	}

	// For resources that exist in both places, emit reconcile event immediately
	for _, resource := range existingInBoth {
		if err := r.eventEmitter.EmitReconcileResourceEvent(resource); err != nil {
			r.logger.Error(err, "Failed to emit reconcile resource event", "resource", resource.String())
		}
	}
}

// findDifferences computes what needs to be created, deleted, and resources that exist in both.
func (r *FolderReconciler) findDifferences(
	clusterResources, gitResources []types.ResourceIdentifier,
) ([]types.ResourceIdentifier, []types.ResourceIdentifier, []types.ResourceIdentifier) {
	clusterSet := make(map[string]types.ResourceIdentifier)
	gitSet := make(map[string]types.ResourceIdentifier)

	// Build sets for efficient lookup
	for _, resource := range clusterResources {
		clusterSet[resource.String()] = resource
	}

	for _, resource := range gitResources {
		gitSet[resource.String()] = resource
	}

	// Find resources to create (in cluster but not in Git)
	var toCreate []types.ResourceIdentifier
	for _, resource := range clusterResources {
		if _, exists := gitSet[resource.String()]; !exists {
			toCreate = append(toCreate, resource)
		}
	}

	// Find resources to delete (in Git but not in cluster)
	var toDelete []types.ResourceIdentifier
	for _, resource := range gitResources {
		if _, exists := clusterSet[resource.String()]; !exists {
			toDelete = append(toDelete, resource)
		}
	}

	// Find resources that exist in both cluster and Git
	var existingInBoth []types.ResourceIdentifier
	for _, resource := range clusterResources {
		if _, exists := gitSet[resource.String()]; exists {
			existingInBoth = append(existingInBoth, resource)
		}
	}

	return toCreate, toDelete, existingInBoth
}

// HasBothStates returns true if the reconciler has received both cluster and Git state.
func (r *FolderReconciler) HasBothStates() bool {
	return r.clusterResources != nil && r.gitResources != nil
}

// GetGitDest returns the GitDestination reference this reconciler is responsible for.
func (r *FolderReconciler) GetGitDest() types.ResourceReference {
	return r.gitDest
}

// String returns a string representation for debugging.
func (r *FolderReconciler) String() string {
	return fmt.Sprintf("FolderReconciler(gitDest=%s)", r.gitDest.String())
}
