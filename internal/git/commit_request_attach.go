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
// open window, then finalize that window after the grace" work item. It rides the same per-worker FIFO
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

	// Author is the effective user that requested the finalize, captured from
	// validating admission. Only a window whose author
	// matches is attached; this binds "the open window" to "the requesting
	// author's open window".
	Author string
	// Attribution is the outcome of attributing THIS CommitRequest, from the command-authorship
	// path. It is matched alongside Author rather than inferred from it, because an empty Author
	// alone cannot say whether an actor was sought: it is both "attribution is off" and
	// "attribution ran and named nobody". Only the NamesActor half is compared against the
	// window's outcome — see matchesWindow for why the enums themselves must not be.
	Attribution AttributionOutcome
	// GitTargetName / GitTargetNamespace scope the finalize to one GitTarget.
	GitTargetName      string
	GitTargetNamespace string

	// Message is the verbatim commit message to attach to the window. Empty keeps
	// the generated grouped-commit message.
	Message string
	// CloseDelaySeconds is the close-delay collect window: the worker closes the
	// attached window and finalizes it at receipt + CloseDelaySeconds (the delay is
	// anchored at attach receipt).
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
	attribution        AttributionOutcome
	gitTargetName      string
	gitTargetNamespace string
	message            string
	// finalizeAt is receipt + CloseDelaySeconds, stamped once on first registration
	// (idempotent re-sends keep it).
	finalizeAt time.Time
	// attached is true once this request's message is bound to the open window.
	attached bool
}

// matchesWindow reports whether the request identifies the given open window, by GitTarget and
// by author.
//
// The two attribution outcomes compared here are produced by DIFFERENT, INDEPENDENTLY
// CONFIGURED subsystems: the window's comes from mirrored-resource attribution
// (--author-attribution, audit facts), the request's from command authorship
// (--admission-webhook, its own Redis corner) — see cmd/main.go:311-316. So they are matched on
// AttributionOutcome.NamesActor, not for enum equality. Requiring the enums to be equal silently
// couples the two flags: with attribution off and the webhook on the window says "not attempted"
// while the request says "unresolved", and with attribution on but missing and the webhook off
// it is the other way round. Both are real deployments, both attach correctly today, and exact
// equality would stop both — dropping the user's commit message into a separate default-message
// commit with no error anywhere.
//
// The author comparison stays UNCONDITIONAL, and the outcome class is an additional guard on
// top of it — never a replacement for it. Skipping the author check when neither side names an
// actor looks equivalent (an unnamed actor leaves the author empty on both sides, so the two
// empties compare equal anyway) but is not: it makes cross-author attachment depend on the
// outcome fields being right, so any path that leaves an outcome unset while the author IS set
// would let one author's request finalize another's window. Comparing both costs nothing and
// keeps "bob never claims alice's window" true regardless of what the outcomes say.
func (p *pendingCommitRequest) matchesWindow(w *openWindow) bool {
	if p == nil || w == nil {
		return false
	}
	return p.author == w.Author &&
		p.attribution.NamesActor() == w.Attribution.NamesActor() &&
		p.gitTargetName == w.GitTarget &&
		p.gitTargetNamespace == w.GitTargetNamespace
}

// commitRequestOutcomeEntry is a resolved outcome retained for the controller to
// poll, GC'd by age.
type commitRequestOutcomeEntry struct {
	result     FinalizeResult
	resolvedAt time.Time
}
