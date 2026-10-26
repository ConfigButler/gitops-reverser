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

// ErrResyncDiscarded replies to a resync whose snapshot the worker did not use: it was collected
// while Git was ahead of the cluster, or across an apply, so an apply was about to make it stale
// (docs/design/applier-echoes.md, F3). It is NOT a failure: the worker asks for a fresh snapshot of
// the GitTarget once Git is no longer ahead.
var ErrResyncDiscarded = errors.New("resync snapshot discarded: an apply made it stale; a fresh one is requested")

// ErrResyncDeferred replies to a resync the worker could not apply yet because the remote could not
// be reached, or the parent branch a new write branch is created from is missing. It is NOT lost:
// it stays pending in its place and is applied with the writes around it once the remote answers.
var ErrResyncDeferred = errors.New("resync deferred until the remote can be reached")

// FinalizeOutcome is the terminal result of resolving a SaveRequest.
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
	// Raised at expiry from pendingSaveRequest.sawForeignWindow, never at attach time: under
	// eager attach a request WAITS for a matching window rather than being refused on sight, so
	// "a foreign window is open right now" is not yet an outcome.
	FinalizeWindowMismatch FinalizeOutcome = "WindowMismatch"
	// FinalizeAlreadyPresent means a matching window was finalized but its events
	// produced no diff — the change already matches the remote, so no commit was
	// made (loop prevention). Resolved at finalize, never waiting on a push.
	FinalizeAlreadyPresent FinalizeOutcome = "AlreadyPresent"
	// FinalizeSaveInProgress means an earlier request of the same author on the same GitTarget was
	// still waiting for a window, or collecting the open one, when this one was registered. Both
	// would race for one window, which carries one message, so this one is refused at once rather
	// than left to lose its message to the first, or its edits to the first's commit.
	FinalizeSaveInProgress FinalizeOutcome = "SaveInProgress"
)

// SaveRequestPhase is where an unresolved SaveRequest stands on the worker. It is what the
// controller reports while it waits, and it comes from the worker because sending an attach does
// not prove the request was registered, attached or committed.
type SaveRequestPhase string

const (
	// PhaseWaitingForWorker is a request whose GitTarget has no branch worker yet, at startup or
	// before the target's first reconcile. It is reported by the router, since there is no worker to
	// report it, and the request is sent again on the controller's next poll.
	PhaseWaitingForWorker SaveRequestPhase = "WaitingForWorker"
	// PhaseWaitingForWindow is a registered request waiting for a window to attach to.
	PhaseWaitingForWindow SaveRequestPhase = "WaitingForWindow"
	// PhaseCollectingWindow is an attached request collecting writes until the window's timers close it.
	PhaseCollectingWindow SaveRequestPhase = "CollectingWindow"
	// PhaseWaitingForPush is a request whose write is decided and not yet confirmed by the remote.
	PhaseWaitingForPush SaveRequestPhase = "WaitingForPush"
)

// Held reports whether the worker has acted on a request in this phase: it is attached to a window
// or committed. From there only the worker can say how it ends, so the controller waits for it
// however long that takes, instead of failing the request on a clock of its own.
func (p SaveRequestPhase) Held() bool {
	return p == PhaseCollectingWindow || p == PhaseWaitingForPush
}

// ErrSaveRequestWithdrawn is the outcome of a request the controller withdrew before the worker
// acted on it. The worker records it, so a late re-send of the attach can never register the
// request again, and the controller's failure and the worker's answer are the same answer.
var ErrSaveRequestWithdrawn = errors.New("the SaveRequest was withdrawn before the worker acted on it")

// FinalizeResult carries the resolved outcome of a SaveRequest back to the
// controller, polled via LookupSaveRequestOutcome.
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
	Phase SaveRequestPhase
	// Held is why a request the worker holds has not reached the remote yet: the worker's
	// publication report while it cannot publish, empty otherwise. See PublicationStatus.
	Held string
}

// AttachSaveRequest is the "attach this SaveRequest to the author's window, with its
// message and its timers" work item. It rides the same per-worker FIFO
// event queue as resource events, so by audit-stream ordering it is processed
// after every earlier write for that worker. Re-sends are idempotent: the worker
// keys pending requests by identity and keeps everything the first delivery set.
type AttachSaveRequest struct {
	// Namespace, Name, UID identify the SaveRequest. UID may be empty (a
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
	// Attribution is the outcome of attributing THIS SaveRequest, from the command-authorship
	// path. It is matched alongside Author rather than inferred from it, because an empty Author
	// alone cannot say whether an actor was sought: it is both "attribution is off" and
	// "attribution ran and named nobody". Only the NamesActor half is compared against the
	// window's outcome — see matchesWindow for why the enums themselves must not be.
	Attribution AttributionOutcome
	// GitTargetName / GitTargetNamespace scope the finalize to one GitTarget.
	GitTargetName      string
	GitTargetNamespace string
	// GitTargetUID is the UID of the GitTarget that admitted the request. A GitTarget of that name
	// with another UID replaced it and admitted nothing, so it is never asked to take the request.
	// Empty matches any.
	GitTargetUID string

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
	// CatchUpNow asks the GitTarget's applier to apply the tip as soon as the request's commit is
	// pushed: catchUp: AfterPush.
	CatchUpNow bool
	// Unattributed is what a request that names its submitter may claim of a window on its GitTarget
	// whose writes name no author: nothing, its message (SavedBy), or the window (TakeOver). The
	// controller sets more than nothing only when the request asked, its GitTarget allows it, and
	// the GitTarget's KubeProvider has never received an audit fact. Once it has, a write naming no
	// author means its fact went missing, and that window stays another's.
	Unattributed UnattributedClaim
}

// UnattributedClaim is what a SaveRequest may claim of a commit window whose writes name no author.
type UnattributedClaim string

const (
	// UnattributedClaimNone attaches only to the submitter's own window.
	UnattributedClaimNone UnattributedClaim = ""
	// UnattributedClaimSavedBy gives the window the request's message and names no author: the
	// commit stays unattributed, and its message ends with a Saved-By trailer.
	UnattributedClaimSavedBy UnattributedClaim = "SavedBy"
	// UnattributedClaimTakeOver takes the window over: the submitter authors the commit, and its
	// message ends with an Author-Source trailer.
	UnattributedClaimTakeOver UnattributedClaim = "TakeOver"
)

// saveRequestID is the worker-local key for a SaveRequest: its namespaced
// name plus UID when available. Two SaveRequests with the same name but
// different UIDs (a delete-and-recreate) are distinct.
type saveRequestID struct {
	Namespace string
	Name      string
	UID       string
}

func (a AttachSaveRequest) id() saveRequestID {
	return saveRequestID{Namespace: a.Namespace, Name: a.Name, UID: a.UID}
}

// pendingSaveRequest is a SaveRequest registered with the worker and not yet
// resolved: waiting for a same-author window to attach to, attached and collecting until the
// window's timers close it, or committed locally and awaiting the push that settles it.
type pendingSaveRequest struct {
	id                 saveRequestID
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
	// catchUpNow asks the GitTarget's applier for a converge once the request's commit is pushed.
	catchUpNow bool
	// unattributed is what the request may claim of a window whose writes name no author
	// (AttachSaveRequest.Unattributed).
	unattributed UnattributedClaim
	// attached is true once this request is bound to a write: its message to the open window, or
	// its own empty-commit record. From then on only that write's outcome settles it, and it stays
	// set while the write is pending, waiting for the push.
	//
	// It stays set because the request must stay IDENTIFIABLE while it waits. The controller
	// re-sends its attach every couple of seconds until it reads an outcome, and forgetting the
	// request at finalize made that re-send look like a brand-new one: it would register again,
	// expire against its fresh deadline, and report NoOpenWindow for work that was sitting in
	// pending, waiting for the push cooldown, or, worse, attach to the next same-author window
	// and stamp this request's message onto a commit somebody else authored.
	attached bool
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
// controller as the WindowMismatch reason, and counted by save_requests_total; from the eager
// attach onwards nothing produced it, so every one of those refusals reported as NoOpenWindow.
func (p *pendingSaveRequest) expiryOutcome() FinalizeOutcome {
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
func (p *pendingSaveRequest) matchesWindow(w *openWindow) bool {
	if p == nil || w == nil {
		return false
	}
	if p.gitTargetName != w.GitTarget || p.gitTargetNamespace != w.GitTargetNamespace {
		return false
	}
	return p.author == w.Author && p.attribution.NamesActor() == w.Attribution.NamesActor() || p.claims(w)
}

// sameSaver reports whether two requests would save one window: the same GitTarget and the same
// author, by the comparison matchesWindow makes.
func (p *pendingSaveRequest) sameSaver(other *pendingSaveRequest) bool {
	return p.gitTargetName == other.gitTargetName && p.gitTargetNamespace == other.gitTargetNamespace &&
		p.author == other.author && p.attribution.NamesActor() == other.attribution.NamesActor()
}

// claims reports whether the request matches a window on its GitTarget whose writes name no author
// only because it may claim such a window: it names its submitter, and asked for more than its own
// windows (AttachSaveRequest.Unattributed).
func (p *pendingSaveRequest) claims(w *openWindow) bool {
	return p.unattributed != UnattributedClaimNone && p.attribution.NamesActor() && !w.Attribution.NamesActor()
}

// saveRequestOutcomeEntry is a resolved outcome retained for the controller to
// poll, GC'd by age.
type saveRequestOutcomeEntry struct {
	result     FinalizeResult
	resolvedAt time.Time
}
