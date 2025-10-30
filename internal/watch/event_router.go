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
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/events"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// EventRouter dispatches events to the correct BranchWorker based on (repo, branch).
// This replaces the old central EventQueue with intelligent routing.
// It also handles routing of RepoStateEvents to BaseFolderReconcilers.
type EventRouter struct {
	WorkerManager *git.WorkerManager
	Log           logr.Logger
}

// NewEventRouter creates a new event router.
func NewEventRouter(workerManager *git.WorkerManager, log logr.Logger) *EventRouter {
	return &EventRouter{
		WorkerManager: workerManager,
		Log:           log,
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

// RouteRepoStateEvent routes RepoStateEvents to the appropriate BaseFolderReconciler.
// This is called when a BranchWorker emits a RepoStateEvent in response to REQUEST_REPO_STATE.
func (r *EventRouter) RouteRepoStateEvent(event events.RepoStateEvent) error {
	// For now, we need to get the reconciler manager from somewhere
	// This will be injected when we integrate with the main application
	// For the refactor, we'll add this method signature and implement it later
	r.Log.V(1).Info("RepoStateEvent received - routing needed",
		"repo", event.RepoName,
		"branch", event.Branch,
		"baseFolder", event.BaseFolder,
		"resourceCount", len(event.Resources))

	// TODO: Route to BaseFolderReconciler when integration is complete
	return nil
}

// RouteClusterStateEvent routes ClusterStateEvents to the appropriate BaseFolderReconciler.
// This is called when WatchManager emits cluster state snapshots.
func (r *EventRouter) RouteClusterStateEvent(event events.ClusterStateEvent) error {
	r.Log.V(1).Info("ClusterStateEvent received - routing needed",
		"repo", event.RepoName,
		"branch", event.Branch,
		"baseFolder", event.BaseFolder,
		"resourceCount", len(event.Resources))

	// TODO: Route to BaseFolderReconciler when integration is complete
	return nil
}
