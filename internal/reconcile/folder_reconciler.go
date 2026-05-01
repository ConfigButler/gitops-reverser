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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// WriteRequestEmitter emits a complete reconcile write request as a single unit.
type WriteRequestEmitter interface {
	EmitWriteRequest(request git.WriteRequest) error
}

// FolderReconciler reconciles Git base folder to match cluster state.
// It operates without time concerns (delegated to WatchManager) and focuses purely
// on reconciliation logic.
type FolderReconciler struct {
	gitDest types.ResourceReference

	// Current state snapshots
	clusterResources []types.ResourceIdentifier
	gitResources     []types.ResourceIdentifier
	clusterStateSeen bool
	gitStateSeen     bool

	// Full cluster objects keyed by ResourceIdentifier.Key(), populated alongside
	// clusterResources so that write events can be hydrated with real payloads.
	clusterObjects map[string]unstructured.Unstructured

	// Dependencies for event emission
	reconcileEmitter WriteRequestEmitter
	controlEmitter   events.ControlEventEmitter
	logger           logr.Logger

	lastSnapshotStats SnapshotStats
}

// SnapshotStats captures the latest create/update/delete counts from reconciliation.
type SnapshotStats struct {
	Created int
	Updated int
	Deleted int
}

// NewFolderReconciler creates a new FolderReconciler.
func NewFolderReconciler(
	gitDest types.ResourceReference,
	reconcileEmitter WriteRequestEmitter,
	controlEmitter events.ControlEventEmitter,
	logger logr.Logger,
) *FolderReconciler {
	return &FolderReconciler{
		gitDest:          gitDest,
		reconcileEmitter: reconcileEmitter,
		controlEmitter:   controlEmitter,
		logger:           logger.WithValues("gitDest", gitDest.String()),
	}
}

// StartReconciliation initiates the reconciliation process by requesting state.
func (r *FolderReconciler) StartReconciliation(_ context.Context) error {
	r.ResetState()

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

// ResetState clears any previously observed repo/cluster snapshots so the next
// reconciliation cycle only runs on a fresh pair of state events.
func (r *FolderReconciler) ResetState() {
	r.clusterResources = nil
	r.gitResources = nil
	r.clusterObjects = nil
	r.clusterStateSeen = false
	r.gitStateSeen = false
}

// OnClusterState handles cluster state events and triggers reconciliation.
func (r *FolderReconciler) OnClusterState(event events.ClusterStateEvent) {
	if !event.GitDest.Equal(r.gitDest) {
		return
	}
	r.clusterResources = event.Resources
	r.clusterObjects = event.Objects
	r.clusterStateSeen = true
	r.logger.V(1).Info("Received cluster state", "resourceCount", len(event.Resources))
	r.reconcile()
}

// OnRepoState handles repository state events and triggers reconciliation.
func (r *FolderReconciler) OnRepoState(event events.RepoStateEvent) {
	if !event.GitDest.Equal(r.gitDest) {
		return
	}
	r.gitResources = event.Resources
	r.gitStateSeen = true
	r.logger.V(1).Info("Received repo state", "resourceCount", len(event.Resources))
	r.reconcile()
}

// reconcile performs the reconciliation logic when both states are available.
// It collects all changes into a single write request and emits it atomically.
func (r *FolderReconciler) reconcile() {
	// Only reconcile when we have both cluster and Git state
	if !r.clusterStateSeen || !r.gitStateSeen {
		return
	}

	// Compute reconciliation actions (pure logic, no time concerns)
	toCreate, toDelete, existingInBoth := r.findDifferences(r.clusterResources, r.gitResources)
	r.lastSnapshotStats = SnapshotStats{
		Created: len(toCreate),
		Updated: len(existingInBoth),
		Deleted: len(toDelete),
	}

	r.logger.V(1).Info("Reconciliation computed",
		"toCreate", len(toCreate),
		"toDelete", len(toDelete),
		"existingInBoth", len(existingInBoth))

	total := len(toCreate) + len(toDelete) + len(existingInBoth)
	if total == 0 {
		r.logger.V(1).Info("No differences found, skipping write request emission")
		return
	}

	// Build the complete event list for this reconcile run
	var batchEvents []git.Event

	for _, resource := range toCreate {
		obj := r.objectForResource(resource)
		batchEvents = append(batchEvents, git.Event{
			Operation:  "CREATE",
			Identifier: resource,
			Object:     obj,
		})
	}

	for _, resource := range toDelete {
		batchEvents = append(batchEvents, git.Event{
			Operation:  "DELETE",
			Identifier: resource,
		})
	}

	for _, resource := range existingInBoth {
		obj := r.objectForResource(resource)
		batchEvents = append(batchEvents, git.Event{
			Operation:  string(events.ReconcileResource),
			Identifier: resource,
			Object:     obj,
		})
	}

	request := git.WriteRequest{
		Events:     batchEvents,
		CommitMode: git.CommitModeAtomic,
		CommitMessage: fmt.Sprintf(
			"Reconcile snapshot: %d created, %d deleted, %d reconciled",
			len(toCreate),
			len(toDelete),
			len(existingInBoth),
		),
	}

	if err := r.reconcileEmitter.EmitWriteRequest(request); err != nil {
		r.logger.Error(err, "Failed to emit reconcile write request")
	}
}

// GetLastSnapshotStats returns stats from the latest completed reconciliation diff.
func (r *FolderReconciler) GetLastSnapshotStats() SnapshotStats {
	return r.lastSnapshotStats
}

// objectForResource returns a pointer to the cached cluster object for the given resource,
// or nil if the object is not available (e.g. ClusterStateEvent carried no objects).
func (r *FolderReconciler) objectForResource(resource types.ResourceIdentifier) *unstructured.Unstructured {
	if r.clusterObjects == nil {
		return nil
	}
	obj, ok := r.clusterObjects[resource.Key()]
	if !ok {
		return nil
	}
	return &obj
}

// findDifferences computes what needs to be created, deleted, and resources that exist in both.
func (r *FolderReconciler) findDifferences(
	clusterResources, gitResources []types.ResourceIdentifier,
) ([]types.ResourceIdentifier, []types.ResourceIdentifier, []types.ResourceIdentifier) {
	clusterSet := make(map[string]types.ResourceIdentifier)
	gitSet := make(map[string]types.ResourceIdentifier)

	// Build sets for efficient lookup
	for _, resource := range clusterResources {
		clusterSet[resource.Key()] = resource
	}

	for _, resource := range gitResources {
		gitSet[resource.Key()] = resource
	}

	// Find resources to create (in cluster but not in Git)
	var toCreate []types.ResourceIdentifier
	for _, resource := range clusterResources {
		if _, exists := gitSet[resource.Key()]; !exists {
			toCreate = append(toCreate, resource)
		}
	}

	// Find resources to delete (in Git but not in cluster)
	var toDelete []types.ResourceIdentifier
	for _, resource := range gitResources {
		if _, exists := clusterSet[resource.Key()]; !exists {
			toDelete = append(toDelete, resource)
		}
	}

	// Find resources that exist in both cluster and Git
	var existingInBoth []types.ResourceIdentifier
	for _, resource := range clusterResources {
		if _, exists := gitSet[resource.Key()]; exists {
			existingInBoth = append(existingInBoth, resource)
		}
	}

	return toCreate, toDelete, existingInBoth
}

// HasBothStates returns true if the reconciler has received both cluster and Git state.
func (r *FolderReconciler) HasBothStates() bool {
	return r.clusterStateSeen && r.gitStateSeen
}

// GetGitDest returns the GitDestination reference this reconciler is responsible for.
func (r *FolderReconciler) GetGitDest() types.ResourceReference {
	return r.gitDest
}

// String returns a string representation for debugging.
func (r *FolderReconciler) String() string {
	return fmt.Sprintf("FolderReconciler(gitDest=%s)", r.gitDest.String())
}
