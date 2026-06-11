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

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// GitTargetEventStream forwards a GitTarget's live field-patch events to its branch worker.
//
// With the api-source-of-truth pivot (R3) the resource mirror is the per-type splice reconcile,
// not a live per-event stream: the long-lived object informers, the RECONCILING handover buffer,
// and the content-hash deduplication this type used to own are all gone. What remains is a thin
// route for the events the splice deliberately does not own — the /scale subresource translated
// into a parent-manifest field patch (redis_audit_consumer.routeScaleFieldPatch). "Newer?" is now
// answered by the audit stream's RV ordering and "changed?" by the writer's no-op detection
// (manifestedit.Decide at the commit boundary), so no hash is computed here. See
// docs/design/stream/api-source-of-truth-reconcile.md (DEC-6, DEC-7).
type GitTargetEventStream struct {
	gitTargetName      string
	gitTargetNamespace string

	branchWorker EventEnqueuer
	logger       logr.Logger
}

// EventEnqueuer enqueues live events onto a branch worker (allows mocking).
type EventEnqueuer interface {
	Enqueue(event git.Event)
}

// NewGitTargetEventStream creates a new event stream for a GitTarget.
func NewGitTargetEventStream(
	gitTargetName, gitTargetNamespace string,
	branchWorker EventEnqueuer,
	logger logr.Logger,
) *GitTargetEventStream {
	return &GitTargetEventStream{
		gitTargetName:      gitTargetName,
		gitTargetNamespace: gitTargetNamespace,
		branchWorker:       branchWorker,
		logger:             logger.WithValues("gitTarget", fmt.Sprintf("%s/%s", gitTargetNamespace, gitTargetName)),
	}
}

// OnWatchEvent forwards a live event to the GitTarget's branch worker. An event with no object
// payload that is neither a DELETE nor a field patch carries nothing to write and is dropped.
func (s *GitTargetEventStream) OnWatchEvent(event git.Event) {
	if event.Object == nil && !event.IsFieldPatch() && event.Operation != "DELETE" {
		s.logger.V(1).Info("Skipping event with no object payload",
			"resource", event.Identifier.Key(), "operation", event.Operation)
		return
	}

	event.GitTargetName = s.gitTargetName
	event.GitTargetNamespace = s.gitTargetNamespace
	s.branchWorker.Enqueue(event)
	s.logger.V(1).Info("Forwarded event", "resource", event.Identifier.Key(), "operation", event.Operation)
}

// String returns a string representation for debugging.
func (s *GitTargetEventStream) String() string {
	return fmt.Sprintf("GitTargetEventStream(gitTarget=%s/%s)", s.gitTargetNamespace, s.gitTargetName)
}
