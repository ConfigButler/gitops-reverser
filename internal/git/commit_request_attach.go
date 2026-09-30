// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"time"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ErrFinalizeQueueFull is reported when a work item cannot be enqueued because
// the worker's event queue is saturated.
var ErrFinalizeQueueFull = errors.New("branch worker event queue full; item dropped")

// ErrResyncSuperseded replies to a resync that a newer request for the same
// GitTarget and scope replaced while it was still queued. It is NOT a failure:
// the newer request carries at least as fresh a desired set and runs in the
// superseded one's place, so a caller must not count it as a resync that did not
// happen.
var ErrResyncSuperseded = errors.New("resync superseded by a newer request for the same scope")

// FinalizeOutcome is the terminal result of resolving a CommitRequest.
type FinalizeOutcome string

const (
	// FinalizeCommitted means an open commit window was finalized into a commit.
	FinalizeCommitted FinalizeOutcome = "Committed"
	// FinalizeNoOpenWindow means the request's attach timeout ran out before a matching
	// same-author window was there to attach to, so nothing was committed for it.
	FinalizeNoOpenWindow FinalizeOutcome = "NoOpenWindow"
	// FinalizeWindowMismatch means a window was open during the request's wait that belonged to
	// a different author or GitTarget, so it was left untouched and the wait ran out without a
	// window this request could attach to. It is a refusal a human can see: the author's own edits
	// went into somebody else's commit, under a generated message rather than theirs.
	//
	// Raised at expiry from pendingCommitRequest.sawForeignWindow, never at attach time: under
	// eager attach a request WAITS for a matching window rather than being refused on sight, so
	// "a foreign window is open right now" is not yet an outcome.
	FinalizeWindowMismatch FinalizeOutcome = "WindowMismatch"
	// FinalizeAlreadyPresent means a matching window was finalized but its events
	// produced no diff — the change already matches the remote, so no commit was
	// made (loop prevention). Resolved at finalize, never waiting on a push.
	FinalizeAlreadyPresent FinalizeOutcome = "AlreadyPresent"
)

// CommitRequestPhase is where an unresolved CommitRequest stands on the worker. It is what the
// controller reports while it waits, and it comes from the worker because sending an attach does
// not prove the request was registered, attached or committed.
type CommitRequestPhase string

const (
	// PhaseWaitingForWorker is a request whose GitTarget has no branch worker yet, at startup or
	// before the target's first reconcile. It is reported by the router, since there is no worker to
	// report it, and the request is sent again on the controller's next poll.
	PhaseWaitingForWorker CommitRequestPhase = "WaitingForWorker"
	// PhaseWaitingForWindow is a registered request waiting for a window to attach to.
	PhaseWaitingForWindow CommitRequestPhase = "WaitingForWindow"
	// PhaseCollectingWindow is an attached request collecting writes until the window's timers close it.
	PhaseCollectingWindow CommitRequestPhase = "CollectingWindow"
	// PhaseWaitingForPush is a request committed locally and not yet confirmed by the remote.
	PhaseWaitingForPush CommitRequestPhase = "WaitingForPush"
)

// FinalizeResult carries the resolved outcome of a CommitRequest back to the
// controller, polled via LookupCommitRequestOutcome.
type FinalizeResult struct {
	// Outcome is set when Err is nil.
	Outcome FinalizeOutcome
	// Commit is the resulting commit Commit when Outcome is FinalizeCommitted.
	Commit string
	// Branch is the branch the worker operates on.
	Branch string
	// Err is set when the request could not be completed.
	Err error
	// Phase is where an UNRESOLVED request stands on the worker; empty once resolved, and empty
	// before the worker has registered it.
	Phase CommitRequestPhase
}

// AttachCommitRequest is the "attach this CommitRequest to the author's window, with its
// message and its timers" work item. It rides the same per-worker FIFO
// event queue as resource events, so by audit-stream ordering it is processed
// after every earlier write for that worker. Re-sends are idempotent: the worker
// keys pending requests by identity and keeps everything the first delivery set.
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
	// Attach selects the window: the author's current one or the next, or only the next.
	Attach v1alpha3.AttachPolicy
	// AttachTimeout bounds the wait for a window, counted from the worker's registration.
	AttachTimeout time.Duration
	// IdleTimeout closes the attached window after this much silence; nil means no idle close.
	IdleTimeout *time.Duration
	// MaxDuration closes the attached window this long after the attach.
	MaxDuration time.Duration
	// CommitEmpty records Message in an empty commit when the request ends with nothing to
	// commit: whenNothingToCommit: CommitEmpty.
	CommitEmpty bool
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
// resolved: waiting for a same-author window to attach to, attached and collecting until the
// window's timers close it, or committed locally and awaiting the push that settles it.
type pendingCommitRequest struct {
	id                 commitRequestID
	author             string
	attribution        AttributionOutcome
	gitTargetName      string
	gitTargetNamespace string
	message            string
	// seq is the registration order: competing requests attach first come, first served.
	seq uint64
	// attachDeadline is registration + attachTimeout, stamped once on first registration
	// (idempotent re-sends keep it). A request still waiting when it passes resolves, and never
	// takes a later window.
	attachDeadline time.Time
	// timers are the request's own window timers, applied to the window it attaches to in place
	// of the GitTarget's.
	idleTimeout *time.Duration
	maxDuration time.Duration
	// commitEmpty records the message in an empty commit when the request ends with nothing to
	// commit. Never for a WindowMismatch: that would claim more than happened.
	commitEmpty bool
	// attached is true once this request's message is bound to the open window.
	attached bool
	// committed is true once the window this request attached to has been finalized into a local
	// commit. The request now rides that retained write and only the push can settle it, which is
	// why it is `committed` and not `published`: the work exists locally and is nowhere else yet.
	//
	// The flag exists because the request must stay IDENTIFIABLE while it waits. The controller
	// re-sends its attach every couple of seconds until it reads an outcome, and forgetting the
	// request at finalize made that re-send look like a brand-new one: it would register again,
	// expire against its fresh deadline, and report NoOpenWindow for work that was sitting in
	// pendingWrites waiting for the push cooldown — or, worse, attach to the next same-author window
	// and stamp this request's message onto a commit somebody else authored.
	committed bool
	// sawForeignWindow is set when a window was open during this request's wait that it could
	// not attach to, because the window belonged to a different author or GitTarget.
	//
	// It is STICKY rather than checked at expiry, and that is the whole point: a foreign window
	// runs on its own timer and is usually finalized before this request's wait runs out, so an
	// instant check at expiry would miss the common case and report the refusal as "nothing was
	// pending" — intermittently, which is worse than never.
	sawForeignWindow bool
}

// expiryOutcome is the terminal outcome for a request whose attach timeout ran out without it
// ever attaching to a window.
//
// The distinction it restores is the one the eager-attach refactor dropped: "nothing was pending
// to save" and "someone else held the window the whole time" are different events with the same
// shape, and only the second one silently substitutes a generated commit message for the sentence
// the request's author typed. FinalizeWindowMismatch has always been declared, surfaced by the
// controller as the WindowMismatch reason, and counted by commit_requests_total; from the eager
// attach onwards nothing produced it, so every one of those refusals reported as NoOpenWindow.
func (p *pendingCommitRequest) expiryOutcome() FinalizeOutcome {
	if p.sawForeignWindow {
		return FinalizeWindowMismatch
	}
	return FinalizeNoOpenWindow
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
// keeps "bob never attaches to alice's window" true regardless of what the outcomes say.
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
