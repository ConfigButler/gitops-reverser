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

	// Registry of GitTargetEventStreams by gitDest key
	gitTargetStreams map[string]*reconcile.GitTargetEventStream
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
		gitTargetStreams:  make(map[string]*reconcile.GitTargetEventStream),
	}
}

// RouteEvent sends an event to the worker for (provider, branch).
// The target info is used to lookup the worker, then the event is queued.
// Returns an error if no worker exists for the given (provider, branch) combination.
func (r *EventRouter) RouteEvent(
	providerName, providerNamespace string,
	branch string,
	event git.Event,
) error {
	worker, exists := r.WorkerManager.GetWorkerForTarget(
		providerName, providerNamespace, branch,
	)

	if !exists {
		return fmt.Errorf("no worker for provider=%s/%s branch=%s",
			providerNamespace, providerName, branch)
	}

	worker.Enqueue(event)

	r.Log.V(1).Info("Event routed to worker",
		"provider", providerName,
		"namespace", providerNamespace,
		"branch", branch,
		"operation", event.Operation,
		"path", event.Path)

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
	// Look up GitTarget
	var gitTarget configv1alpha1.GitTarget
	if err := r.Client.Get(ctx, client.ObjectKey{
		Name:      event.GitDest.Name,
		Namespace: event.GitDest.Namespace,
	}, &gitTarget); err != nil {
		return fmt.Errorf("failed to get GitTarget: %w", err)
	}

	// Get BranchWorker
	worker, exists := r.WorkerManager.GetWorkerForTarget(
		gitTarget.Spec.Provider.Name,
		gitTarget.Namespace, // Provider is in same namespace
		gitTarget.Spec.Branch,
	)
	if !exists {
		return fmt.Errorf("no worker for %s", event.GitDest.String())
	}

	// Call BranchWorker service (synchronous)
	resources, err := worker.ListResourcesInPath(gitTarget.Spec.Path)
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

// RegisterGitTargetEventStream registers a GitTargetEventStream with the router.
// This allows routing events to specific GitTargetEventStreams for buffering and deduplication.
func (r *EventRouter) RegisterGitTargetEventStream(
	gitDest types.ResourceReference,
	stream *reconcile.GitTargetEventStream,
) {
	key := gitDest.Key()
	r.gitTargetStreams[key] = stream
	r.Log.Info("Registered GitTargetEventStream",
		"gitDest", gitDest.String(),
		"stream", stream.String())
}

// UnregisterGitTargetEventStream removes a GitTargetEventStream from the router.
// This is called during GitTarget deletion cleanup.
func (r *EventRouter) UnregisterGitTargetEventStream(gitDest types.ResourceReference) {
	key := gitDest.Key()
	if _, exists := r.gitTargetStreams[key]; exists {
		delete(r.gitTargetStreams, key)
		r.Log.Info("Unregistered GitTargetEventStream", "gitDest", gitDest.String())
	}
}

// RouteToGitTargetEventStream routes an event to a specific GitTargetEventStream.
// This replaces direct routing to BranchWorkers, enabling event buffering and deduplication.
func (r *EventRouter) RouteToGitTargetEventStream(
	event git.Event,
	gitDest types.ResourceReference,
) error {
	key := gitDest.Key()
	stream, exists := r.gitTargetStreams[key]

	if !exists {
		return fmt.Errorf("no GitTargetEventStream registered for %s", key)
	}

	stream.OnWatchEvent(event)

	r.Log.V(1).Info("Event routed to GitTargetEventStream",
		"gitDest", gitDest.String(),
		"operation", event.Operation,
		"path", event.Path,
		"resource", event.Identifier.String())

	return nil
}
