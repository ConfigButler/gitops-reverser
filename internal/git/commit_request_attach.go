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
	"errors"
	"time"
)

// ErrFinalizeQueueFull is reported when a work item cannot be enqueued because
// the worker's event queue is saturated.
var ErrFinalizeQueueFull = errors.New("branch worker event queue full; item dropped")

// FinalizeOutcome is the terminal result of resolving a CommitRequest.
type FinalizeOutcome string

const (
	// FinalizeCommitted means an open commit window was finalized into a commit.
	FinalizeCommitted FinalizeOutcome = "Committed"
	// FinalizeNoOpenWindow means no matching same-author window was collected
	// within the grace, so nothing was committed for the request.
	FinalizeNoOpenWindow FinalizeOutcome = "NoOpenWindow"
	// FinalizeWindowMismatch means the open window belonged to a different author
	// or GitTarget than the request, so it was left untouched.
	FinalizeWindowMismatch FinalizeOutcome = "WindowMismatch"
)

// FinalizeResult carries the resolved outcome of a CommitRequest back to the
// controller, polled via LookupCommitRequestOutcome.
type FinalizeResult struct {
	// Outcome is set when Err is nil.
	Outcome FinalizeOutcome
	// SHA is the resulting commit SHA when Outcome is FinalizeCommitted.
	SHA string
	// Branch is the branch the worker operates on.
	Branch string
	// Err is set when the request could not be completed.
	Err error
}

// AttachCommitRequest is the "bind this CommitRequest's message to the author's
// open window, then finalize that window after the grace" work item (§6.4 of
// docs/design/stream/commitrequest-design.md). It rides the same per-worker FIFO
// event queue as resource events, so by audit-stream ordering it is processed
// after every earlier write for that worker. Re-sends are idempotent: the worker
// keys pending requests by identity and keeps the first finalize deadline.
type AttachCommitRequest struct {
	// Namespace, Name, UID identify the CommitRequest. UID may be empty (a
	// Metadata-level audit policy can omit it); identity then keys on
	// namespace/name only.
	Namespace string
	Name      string
	UID       string

	// Author is the effective user that requested the finalize, attributed from
	// the CommitRequest's own create audit event. Only a window whose author
	// matches is attached; this binds "the open window" to "the requesting
	// author's open window".
	Author string
	// GitTargetName / GitTargetNamespace scope the finalize to one GitTarget.
	GitTargetName      string
	GitTargetNamespace string

	// Message is the verbatim commit message to attach to the window. Empty keeps
	// the generated grouped-commit message.
	Message string
	// DelaySeconds is the collect-grace: the worker finalizes the attached window
	// at receipt + DelaySeconds (the grace is anchored at attribution, §6.4.4).
	DelaySeconds int32
}

// commitRequestID is the worker-local key for a CommitRequest: its namespaced
// name plus UID when available. Two CommitRequests with the same name but
// different UIDs (a delete-and-recreate) are distinct.
type commitRequestID struct {
	Namespace string
	Name      string
	UID       string
}

func (a AttachCommitRequest) id() commitRequestID {
	return commitRequestID{Namespace: a.Namespace, Name: a.Name, UID: a.UID}
}

// pendingCommitRequest is a CommitRequest registered with the worker and not yet
// resolved: either waiting for a same-author window to attach to, or already
// attached and awaiting its finalize deadline.
type pendingCommitRequest struct {
	id                 commitRequestID
	author             string
	gitTargetName      string
	gitTargetNamespace string
	message            string
	// finalizeAt is receipt + DelaySeconds, stamped once on first registration
	// (idempotent re-sends keep it).
	finalizeAt time.Time
	// attached is true once this request's message is bound to the open window.
	attached bool
}

// matchesWindow reports whether the request identifies the given open window by
// author and GitTarget.
func (p *pendingCommitRequest) matchesWindow(w *openWindow) bool {
	if p == nil || w == nil {
		return false
	}
	return p.author == w.Author &&
		p.gitTargetName == w.GitTarget &&
		p.gitTargetNamespace == w.GitTargetNamespace
}

// commitRequestOutcomeEntry is a resolved outcome retained for the controller to
// poll, GC'd by age.
type commitRequestOutcomeEntry struct {
	result     FinalizeResult
	resolvedAt time.Time
}
