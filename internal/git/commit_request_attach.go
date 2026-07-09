// SPDX-License-Identifier: Apache-2.0

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
	// FinalizeAlreadyPresent means a matching window was finalized but its events
	// produced no diff — the change already matches the remote, so no commit was
	// made (loop prevention). Resolved at finalize, never waiting on a push.
	FinalizeAlreadyPresent FinalizeOutcome = "AlreadyPresent"
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
	// AssertedAuthor, when set, is the identity a privileged client stated this commit
	// is for (CommitRequest spec.author, honored only against an authorized admission
	// record). It changes the attach in two ways: the request binds to ANY open window
	// for the GitTarget rather than only to one whose audit-derived author matches, and
	// the identity becomes the commit's author signature instead of merely selecting a
	// window. The committer is unaffected.
	AssertedAuthor *UserInfo
	// GitTargetName / GitTargetNamespace scope the finalize to one GitTarget.
	GitTargetName      string
	GitTargetNamespace string

	// Message is the verbatim commit message to attach to the window. Empty keeps
	// the generated grouped-commit message.
	Message string
	// CloseDelaySeconds is the close-delay collect window: the worker closes the
	// attached window and finalizes it at receipt + CloseDelaySeconds (the delay is
	// anchored at attribution, §6.4.4).
	CloseDelaySeconds int32
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
	assertedAuthor     *UserInfo
	gitTargetName      string
	gitTargetNamespace string
	message            string
	// finalizeAt is receipt + CloseDelaySeconds, stamped once on first registration
	// (idempotent re-sends keep it).
	finalizeAt time.Time
	// attached is true once this request's message is bound to the open window.
	attached bool
}

// matchesWindow reports whether the request identifies the given open window.
//
// Without an asserted author the match is by audit-derived author AND GitTarget: the
// request finalizes "the requesting author's open window", never someone else's.
//
// With an asserted author the match is by GitTarget alone. The assertion is a statement
// about the commit being made, by a caller who had to hold an RBAC verb to make it — not
// a claim to be whichever actor the audit stream happened to record for the pending edits.
func (p *pendingCommitRequest) matchesWindow(w *openWindow) bool {
	if p == nil || w == nil {
		return false
	}
	if p.gitTargetName != w.GitTarget || p.gitTargetNamespace != w.GitTargetNamespace {
		return false
	}
	if p.assertedAuthor != nil {
		return true
	}
	return p.author == w.Author
}

// commitRequestOutcomeEntry is a resolved outcome retained for the controller to
// poll, GC'd by age.
type commitRequestOutcomeEntry struct {
	result     FinalizeResult
	resolvedAt time.Time
}
