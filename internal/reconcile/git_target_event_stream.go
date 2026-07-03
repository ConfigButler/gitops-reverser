// SPDX-License-Identifier: Apache-2.0

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

// EventEnqueuer enqueues live events onto a branch worker (allows mocking). Enqueue
// reports whether the event entered the worker's FIFO; a false return means the queue
// was full and the event was dropped, so the caller must not advance a durable watch
// cursor past it.
type EventEnqueuer interface {
	Enqueue(event git.Event) bool
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
// payload that is neither a DELETE nor a field patch carries nothing to write and is dropped
// (returns nil — there is nothing to durably hand off). It returns a non-nil error only when the
// worker's queue is full and a real event was dropped, so the watch loop does not advance its
// durable cursor past an event the worker never accepted: the watch reconnects from the
// un-advanced cursor and redelivers, which is safe because the writer's no-op detection at the
// commit boundary makes redelivery idempotent.
func (s *GitTargetEventStream) OnWatchEvent(event git.Event) error {
	if event.Object == nil && !event.IsFieldPatch() && event.Operation != "DELETE" {
		s.logger.V(1).Info("Skipping event with no object payload",
			"resource", event.Identifier.Key(), "operation", event.Operation)
		return nil
	}

	event.GitTargetName = s.gitTargetName
	event.GitTargetNamespace = s.gitTargetNamespace
	if !s.branchWorker.Enqueue(event) {
		return fmt.Errorf("branch worker queue full for gitTarget %s/%s; dropped %s event for %s",
			s.gitTargetNamespace, s.gitTargetName, event.Operation, event.Identifier.Key())
	}
	s.logger.V(1).Info("Forwarded event", "resource", event.Identifier.Key(), "operation", event.Operation)
	return nil
}

// String returns a string representation for debugging.
func (s *GitTargetEventStream) String() string {
	return fmt.Sprintf("GitTargetEventStream(gitTarget=%s/%s)", s.gitTargetNamespace, s.gitTargetName)
}
