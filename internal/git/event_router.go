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
	"fmt"

	"github.com/go-logr/logr"
)

// EventRouter dispatches events to the correct BranchWorker based on (repo, branch).
// This replaces the old central EventQueue with intelligent routing.
type EventRouter struct {
	WorkerManager *WorkerManager
	Log           logr.Logger
}

// NewEventRouter creates a new event router.
func NewEventRouter(workerManager *WorkerManager, log logr.Logger) *EventRouter {
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
	event SimplifiedEvent,
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
