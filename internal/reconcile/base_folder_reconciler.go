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
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// BaseFolderReconciler reconciles Git base folder to match cluster state.
// It operates without time concerns (delegated to WatchManager) and focuses purely
// on reconciliation logic.
type BaseFolderReconciler struct {
	repoName   string
	branch     string
	baseFolder string

	// Current state snapshots
	clusterResources []types.ResourceIdentifier
	gitResources     []types.ResourceIdentifier

	// Dependencies for event emission
	eventEmitter EventEmitter
	logger       logr.Logger
}

// NewBaseFolderReconciler creates a new BaseFolderReconciler.
func NewBaseFolderReconciler(
	repoName, branch, baseFolder string,
	eventEmitter EventEmitter,
	logger logr.Logger,
) *BaseFolderReconciler {
	return &BaseFolderReconciler{
		repoName:     repoName,
		branch:       branch,
		baseFolder:   baseFolder,
		eventEmitter: eventEmitter,
		logger:       logger,
	}
}

// OnClusterState handles cluster state events and triggers reconciliation.
func (r *BaseFolderReconciler) OnClusterState(event events.ClusterStateEvent) {
	if r.matchesEvent(event) {
		r.clusterResources = event.Resources
		r.reconcile()
	}
}

// OnRepoState handles repository state events and triggers reconciliation.
func (r *BaseFolderReconciler) OnRepoState(event events.RepoStateEvent) {
	if r.matchesEvent(event) {
		r.gitResources = event.Resources
		r.reconcile()
	}
}

// matchesEvent checks if the event is for this reconciler's scope.
func (r *BaseFolderReconciler) matchesEvent(event interface{}) bool {
	switch e := event.(type) {
	case events.ClusterStateEvent:
		return e.RepoName == r.repoName && e.Branch == r.branch && e.BaseFolder == r.baseFolder
	case events.RepoStateEvent:
		return e.RepoName == r.repoName && e.Branch == r.branch && e.BaseFolder == r.baseFolder
	default:
		return false
	}
}

// reconcile performs the reconciliation logic when both states are available.
func (r *BaseFolderReconciler) reconcile() {
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
func (r *BaseFolderReconciler) findDifferences(
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
func (r *BaseFolderReconciler) HasBothStates() bool {
	return r.clusterResources != nil && r.gitResources != nil
}

// GetRepoName returns the repository name this reconciler is responsible for.
func (r *BaseFolderReconciler) GetRepoName() string {
	return r.repoName
}

// GetBranch returns the branch this reconciler is responsible for.
func (r *BaseFolderReconciler) GetBranch() string {
	return r.branch
}

// GetBaseFolder returns the base folder this reconciler is responsible for.
func (r *BaseFolderReconciler) GetBaseFolder() string {
	return r.baseFolder
}

// RequestClusterState creates a control event to request cluster state.
func (r *BaseFolderReconciler) RequestClusterState() events.ControlEvent {
	return events.ControlEvent{
		Type:       events.RequestClusterState,
		RepoName:   r.repoName,
		Branch:     r.branch,
		BaseFolder: r.baseFolder,
	}
}

// RequestRepoState creates a control event to request repository state.
func (r *BaseFolderReconciler) RequestRepoState() events.ControlEvent {
	return events.ControlEvent{
		Type:       events.RequestRepoState,
		RepoName:   r.repoName,
		Branch:     r.branch,
		BaseFolder: r.baseFolder,
	}
}

// String returns a string representation for debugging.
func (r *BaseFolderReconciler) String() string {
	return fmt.Sprintf("BaseFolderReconciler(repo=%s, branch=%s, baseFolder=%s)", r.repoName, r.branch, r.baseFolder)
}
