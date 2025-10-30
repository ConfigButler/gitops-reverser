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

package watch

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// EventRouter orchestrates control flow between components.
// It dispatches events to BranchWorkers, calls services synchronously,
// and routes state events to reconcilers.
type EventRouter struct {
	WorkerManager     *git.WorkerManager
	ReconcilerManager *reconcile.ReconcilerManager
	WatchManager      *Manager
	Client            client.Client
	Log               logr.Logger

	// Registry of GitDestinationEventStreams by gitDest key
	gitDestStreams map[string]*reconcile.GitDestinationEventStream
}

// NewEventRouter creates a new event router.
func NewEventRouter(
	workerManager *git.WorkerManager,
	reconcilerManager *reconcile.ReconcilerManager,
	watchManager *Manager,
	client client.Client,
	log logr.Logger,
) *EventRouter {
	return &EventRouter{
		WorkerManager:     workerManager,
		ReconcilerManager: reconcilerManager,
		WatchManager:      watchManager,
		Client:            client,
		Log:               log,
		gitDestStreams:    make(map[string]*reconcile.GitDestinationEventStream),
	}
}

// RouteEvent sends an event to the worker for (repo, branch).
// The destination info is used to lookup the worker, then the event is queued.
// Returns an error if no worker exists for the given (repo, branch) combination.
func (r *EventRouter) RouteEvent(
	repoName, repoNamespace string,
	branch string,
	event git.Event,
) error {
	worker, exists := r.WorkerManager.GetWorkerForDestination(
		repoName, repoNamespace, branch,
	)

	if !exists {
		return fmt.Errorf("no worker for repo=%s/%s branch=%s",
			repoNamespace, repoName, branch)
	}

	worker.Enqueue(event)

	r.Log.V(1).Info("Event routed to worker",
		"repo", repoName,
		"namespace", repoNamespace,
		"branch", branch,
		"operation", event.Operation,
		"baseFolder", event.BaseFolder)

	return nil
}

// ProcessControlEvent handles control events from reconcilers.
func (r *EventRouter) ProcessControlEvent(ctx context.Context, event events.ControlEvent) error {
	r.Log.V(1).Info("Processing control event", "type", event.Type, "gitDest", event.GitDest.String())

	switch event.Type {
	case events.RequestClusterState:
		return r.handleRequestClusterState(ctx, event)
	case events.RequestRepoState:
		return r.handleRequestRepoState(ctx, event)
	case events.ReconcileResource:
		return r.handleReconcileResource(ctx, event)
	default:
		return fmt.Errorf("unknown control event type: %s", event.Type)
	}
}

// handleRequestClusterState processes RequestClusterState control events.
func (r *EventRouter) handleRequestClusterState(ctx context.Context, event events.ControlEvent) error {
	// Call WatchManager service (synchronous)
	resources, err := r.WatchManager.GetClusterStateForGitDest(ctx, event.GitDest)
	if err != nil {
		return fmt.Errorf("failed to get cluster state: %w", err)
	}

	// Wrap in event and route
	return r.RouteClusterStateEvent(events.ClusterStateEvent{
		GitDest:   event.GitDest,
		Resources: resources,
	})
}

// handleRequestRepoState processes RequestRepoState control events.
func (r *EventRouter) handleRequestRepoState(ctx context.Context, event events.ControlEvent) error {
	// Look up GitDestination
	var gitDest configv1alpha1.GitDestination
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      event.GitDest.Name,
		Namespace: event.GitDest.Namespace,
	}, &gitDest); err != nil {
		return fmt.Errorf("failed to get GitDestination: %w", err)
	}

	// Get BranchWorker
	worker, exists := r.WorkerManager.GetWorkerForDestination(
		gitDest.Spec.RepoRef.Name,
		gitDest.Spec.RepoRef.Namespace,
		gitDest.Spec.Branch,
	)
	if !exists {
		return fmt.Errorf("no worker for %s", event.GitDest.String())
	}

	// Call BranchWorker service (synchronous)
	resources, err := worker.ListResourcesInBaseFolder(gitDest.Spec.BaseFolder)
	if err != nil {
		return fmt.Errorf("failed to list resources: %w", err)
	}

	// Wrap in event and route
	return r.RouteRepoStateEvent(events.RepoStateEvent{
		GitDest:   event.GitDest,
		Resources: resources,
	})
}

// handleReconcileResource processes ReconcileResource control events.
func (r *EventRouter) handleReconcileResource(_ context.Context, event events.ControlEvent) error {
	// This would handle individual resource reconciliation
	// For now, just log it
	r.Log.V(1).Info("ReconcileResource event", "gitDest", event.GitDest.String(), "resource", event.Resource)
	return nil
}

// RouteRepoStateEvent routes RepoStateEvents to the appropriate FolderReconciler.
func (r *EventRouter) RouteRepoStateEvent(event events.RepoStateEvent) error {
	reconciler, exists := r.ReconcilerManager.GetReconciler(event.GitDest)
	if !exists {
		r.Log.V(1).Info("No reconciler found", "gitDest", event.GitDest.String())
		return nil
	}
	reconciler.OnRepoState(event)
	return nil
}

// RouteClusterStateEvent routes ClusterStateEvents to the appropriate FolderReconciler.
func (r *EventRouter) RouteClusterStateEvent(event events.ClusterStateEvent) error {
	reconciler, exists := r.ReconcilerManager.GetReconciler(event.GitDest)
	if !exists {
		r.Log.V(1).Info("No reconciler found", "gitDest", event.GitDest.String())
		return nil
	}
	reconciler.OnClusterState(event)
	return nil
}

// RegisterGitDestinationEventStream registers a GitDestinationEventStream with the router.
// This allows routing events to specific GitDestinationEventStreams for buffering and deduplication.
func (r *EventRouter) RegisterGitDestinationEventStream(
	gitDest types.ResourceReference,
	stream *reconcile.GitDestinationEventStream,
) {
	key := gitDest.Key()
	r.gitDestStreams[key] = stream
	r.Log.Info("Registered GitDestinationEventStream",
		"gitDest", gitDest.String(),
		"stream", stream.String())
}

// RouteToGitDestinationEventStream routes an event to a specific GitDestinationEventStream.
// This replaces direct routing to BranchWorkers, enabling event buffering and deduplication.
func (r *EventRouter) RouteToGitDestinationEventStream(
	event git.Event,
	gitDest types.ResourceReference,
) error {
	key := gitDest.Key()
	stream, exists := r.gitDestStreams[key]

	if !exists {
		return fmt.Errorf("no GitDestinationEventStream registered for %s", key)
	}

	stream.OnWatchEvent(event)

	r.Log.V(1).Info("Event routed to GitDestinationEventStream",
		"gitDest", gitDest.String(),
		"operation", event.Operation,
		"baseFolder", event.BaseFolder,
		"resource", event.Identifier.String())

	return nil
}
