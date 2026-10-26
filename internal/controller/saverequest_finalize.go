// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// SaveRequest condition reasons (CamelCase tokens surfaced on status.conditions).
const (
	crReasonCommitted               = "Committed"
	crReasonNoWindow                = "NoWindow"
	crReasonWindowMismatch          = "WindowMismatch"
	crReasonAlreadyPresent          = "AlreadyPresent"
	crReasonFinalizeFailed          = "FinalizeFailed"
	crReasonUnexpectedOutcome       = "UnexpectedOutcome"
	crReasonAttributedFromAdmission = "AttributedFromAdmission"
	crReasonCommitterFallback       = "CommitterFallback"
	crReasonAuthorCaptureDisabled   = "AuthorCaptureDisabled"
	crReasonGitTargetRefPruned      = "GitTargetRefPruned"
	crReasonTakeOverNotAllowed      = "TakeOverNotAllowed"
	crReasonNotAllowed              = "NotAllowed"
)

// The bounded `outcome` values on gitopsreverser_save_requests_total.
//
// They are derived from git.FinalizeOutcome rather than from the condition reasons above: the
// FinalizeResult is what DECIDES, and the reasons include non-terminal and attribution-only values
// that have no business on a terminal counter. The set is closed, and outcomeFailed absorbs every
// hard failure so an unmapped result can never go uncounted.
const (
	crOutcomeCommitted      = "committed"
	crOutcomeNoWindow       = "no_window"
	crOutcomeWindowMismatch = "window_mismatch"
	crOutcomeAlreadyPresent = "already_present"
	crOutcomeFailed         = "failed"
)

// saveRequestOutcome maps one finalize result to its counter value.
//
// A finalize error outranks the outcome because FinalizeResult.Outcome is only set when Err is nil,
// and an unrecognized outcome counts as failed for the same reason applyFinalizeResultToStatus
// fails it: an empty or unknown outcome with no error is a bug, and a bug that increments nothing
// is a bug nobody sees.
func saveRequestOutcome(result git.FinalizeResult, finalizeErr error) string {
	if finalizeErr != nil {
		return crOutcomeFailed
	}
	switch result.Outcome {
	case git.FinalizeCommitted:
		return crOutcomeCommitted
	case git.FinalizeNoOpenWindow:
		return crOutcomeNoWindow
	case git.FinalizeWindowMismatch:
		return crOutcomeWindowMismatch
	case git.FinalizeAlreadyPresent:
		return crOutcomeAlreadyPresent
	default:
		return crOutcomeFailed
	}
}

// saveRequestTarget is the GitTarget identity one terminal outcome is counted under.
//
// The zero value is the documented FALLBACK for a request whose target could not be resolved, and
// it publishes empty labels rather than the requested name. That is deliberate: spec.gitTargetRef
// is whatever the client wrote, so labelling an unresolved reference would let anyone with create
// rights on saverequests mint an unbounded number of series by naming targets that do not exist.
// The requested reference is still on the object's conditions and in the logs, which is where an
// unbounded string belongs.
type saveRequestTarget struct {
	namespace string
	name      string
}

// resolvedSaveRequestTarget names the GitTarget a request was serviced against.
//
// It is only correct to call once the workflow has RESOLVED that target, which
// EventRouter.ServiceSaveRequest does by getting the GitTarget and returning an error when it is
// absent. So a nil service error is the proof that this identity exists, and no extra API lookup is
// needed to bound the label — the plan's rule that metric recording must not block terminal status
// on another failing call.
//
// The key is SaveRequest.GitTargetKey, the GitTarget the attach names, which is the key
// ServiceSaveRequest gets.
func resolvedSaveRequestTarget(cr *configv1alpha3.SaveRequest) saveRequestTarget {
	key := cr.GitTargetKey()
	return saveRequestTarget{namespace: key.Namespace, name: key.Name}
}

// recordSaveRequestOutcome increments the terminal-outcome counter once.
//
// It is called at the point the outcome is SETTLED, never from inside applyFinalizeResultToStatus:
// that runs inside writeTerminalStatus's conflict-retry loop, up to saveRequestStatusUpdateAttempts
// times for one request, so recording there would over-report exactly the contended case. The
// counter measures what the pipeline decided; whether the terminal status then reached the API
// server is a separate failure, already logged.
//
// See the instrument's doc comment for what this does and does not promise across reconciles.
func recordSaveRequestOutcome(ctx context.Context, outcome string, target saveRequestTarget) {
	if telemetry.SaveRequestsTotal == nil {
		return
	}
	telemetry.SaveRequestsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("gittarget_namespace", target.namespace),
		attribute.String("gittarget_name", target.name),
	))
}

// noWindowMessage is the prose for a NoWindow outcome: the attach timeout ran out before a window
// the request could attach to was there.
const noWindowMessage = "no matching commit window was there to attach to before attachTimeout ran out; " +
	"nothing was pending to save"

// alreadyPresentMessage is the prose for an AlreadyPresent outcome: the finalized
// window produced no diff.
const alreadyPresentMessage = "the change already matches the remote, so no commit was made"

// notStalledMessage is the boilerplate message for a False Stalled condition.
const notStalledMessage = "the SaveRequest is not stalled"

// saveRequestAttribution is the settled author decision threaded from the
// attribute step into the terminal status write so the AuthorAttributed condition can
// record whether the commit author was named. It is binary and always settled at
// first sight (present-or-never, §2) — there is no pending or timed-out state.
type saveRequestAttribution int

const (
	// attributionUnset is the defensive zero value. It must never report a resolved
	// actor when a caller neglected to initialize the attribution decision.
	attributionUnset saveRequestAttribution = iota
	// attributionFromAdmission means the submitter was captured at admission by the
	// validate-operator-types webhook and named as the commit author.
	attributionFromAdmission
	// attributionCommitter means command-author capture ran but no admission author record
	// exists, so the request does not claim an actor.
	attributionCommitter
	// attributionNotAttempted means command-author capture is switched off entirely (the
	// validate-operator-types webhook is not running), so no submitter was ever sought. It is
	// distinct from attributionCommitter, where capture DID run and found nothing.
	attributionNotAttempted
)

// gitOutcome projects the controller's attribution state onto the git layer's outcome, which
// is what the worker matches windows on. Keeping the projection here — rather than passing the
// controller's own enum down — means the git package never learns about admission records.
func (a saveRequestAttribution) gitOutcome() git.AttributionOutcome {
	switch a {
	case attributionUnset:
		return git.AttributionNotAttempted
	case attributionFromAdmission:
		return git.AttributionResolved
	case attributionCommitter:
		return git.AttributionUnresolved
	case attributionNotAttempted:
		return git.AttributionNotAttempted
	default:
		return git.AttributionNotAttempted
	}
}

// setSaveRequestCondition upserts a condition keyed by type, stamping it with
// the request's generation so kstatus can tell a current status from a stale one.
func setSaveRequestCondition(
	cr *configv1alpha3.SaveRequest,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	cr.Status.Conditions = upsertCondition(cr.Status.Conditions, conditionType, status, reason, message, cr.Generation)
}

// Progress-condition messages, one per phase the worker reports. The author is settled
// synchronously at first sight (present-or-never, §2), so there is no "waiting for the author"
// phase. Until the worker has registered the request the reason is the generic Progressing.
const (
	progressingMessage      = "sent to the branch worker; waiting for it to register the request"
	waitingForWorkerMessage = "the GitTarget has no branch worker yet; waiting for its reconcile to start one"
	waitingForWindowMessage = "registered with the worker; waiting for a matching commit window to attach to"
	collectingWindowMessage = "attached to a commit window; collecting writes until its timers close it"
	waitingForPushMessage   = "committed locally; waiting for the push to reach the remote"
	pushPendingMessage      = "the commit has not been pushed yet"
)

// progressFor maps a worker-reported phase to its progress reason and message. held is why a
// request waiting for its push has not reached the remote yet: the branch worker's publication
// report, in the words its GitTarget uses.
func progressFor(phase git.SaveRequestPhase, held string) (string, string) {
	switch phase {
	case git.PhaseWaitingForWorker:
		return string(phase), waitingForWorkerMessage
	case git.PhaseWaitingForWindow:
		return string(phase), waitingForWindowMessage
	case git.PhaseCollectingWindow:
		return string(phase), collectingWindowMessage
	case git.PhaseWaitingForPush:
		if held != "" {
			return string(phase), waitingForPushMessage + ". " + held
		}
		return string(phase), waitingForPushMessage
	default:
		return ReasonProgressing, progressingMessage
	}
}

// setSaveRequestProgress stamps the four progress conditions (Ready=False,
// Reconciling=True, Stalled=False, Pushed=Unknown) with one unifying reason, so a
// single wait reads consistently across them.
func setSaveRequestProgress(cr *configv1alpha3.SaveRequest, reason, message string) {
	setSaveRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	setSaveRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	setSaveRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setSaveRequestCondition(cr, ConditionTypePushed, metav1.ConditionUnknown, reason, pushPendingMessage)
}

// markSaveRequestProgressing records the still-running state for a worker-reported phase
// (Reconciling=True). An empty phase is the generic Progressing: sent, not yet confirmed.
func markSaveRequestProgressing(
	cr *configv1alpha3.SaveRequest,
	attribution saveRequestAttribution,
	phase git.SaveRequestPhase,
	held string,
) {
	cr.Status.ObservedGeneration = cr.Generation
	setSaveRequestAttributed(cr, attribution)
	reason, message := progressFor(phase, held)
	setSaveRequestProgress(cr, reason, message)
}

// setSaveRequestAttributed records the settled, binary author decision on the
// AuthorAttributed condition. A False reason distinguishes an absent record
// (CommitterFallback) from capture disabled (AuthorCaptureDisabled). Neither is a failure or
// affects Ready; the request claims no actor.
func setSaveRequestAttributed(cr *configv1alpha3.SaveRequest, attribution saveRequestAttribution) {
	switch attribution {
	case attributionUnset:
		setSaveRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
			crReasonAuthorCaptureDisabled,
			"command-author capture state was unset; the request can attach only to a window with no named actor")
	case attributionFromAdmission:
		setSaveRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionTrue,
			crReasonAttributedFromAdmission,
			"the submitter was captured at admission and named as the commit author")
	case attributionCommitter:
		// Capture RAN and found nothing. Distinct from attributionNotAttempted below, whose
		// message this used to carry — reporting "the webhook is not configured" for a
		// webhook that is configured and simply missed sends operators to the wrong place.
		setSaveRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
			crReasonCommitterFallback,
			"no admission author record was found for this request; it can attach only to a "+
				"window with no named actor")
	case attributionNotAttempted:
		setSaveRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
			crReasonAuthorCaptureDisabled,
			"command-author capture is disabled (the validate-operator-types webhook is not "+
				"configured); the request can attach only to a window with no named actor")
	}
}

// applyFinalizeResultToStatus maps a FinalizeResult (or a finalize error) and the
// settled attribution onto a SaveRequest's terminal conditions. Ready is the
// summary; Reconciling/Stalled carry kstatus; Attributed records the author
// decision; Pushed records whether a commit reached the remote.
func applyFinalizeResultToStatus(
	cr *configv1alpha3.SaveRequest,
	result git.FinalizeResult,
	finalizeErr error,
	attribution saveRequestAttribution,
) {
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Branch = result.Branch
	setSaveRequestAttributed(cr, attribution)

	if finalizeErr != nil {
		reason := crReasonFinalizeFailed
		switch {
		case errors.Is(finalizeErr, errTakeOverNotAllowed):
			reason = crReasonTakeOverNotAllowed
		case errors.Is(finalizeErr, errSaveRequestNotAllowed):
			reason = crReasonNotAllowed
		}
		failSaveRequest(cr, reason, finalizeErr.Error())
		return
	}

	switch result.Outcome {
	case git.FinalizeCommitted:
		cr.Status.Commit = result.Commit
		const committedMsg = "the open commit window was closed, committed, and pushed"
		setSaveRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, crReasonCommitted, committedMsg)
		setSaveRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, crReasonCommitted, notStalledMessage)
		setSaveRequestCondition(cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded,
			"the commit was pushed to the remote repository")
		setSaveRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted, committedMsg)
	case git.FinalizeNoOpenWindow:
		// Benign: the attach timeout ran out with nothing pending to save.
		resolveNothingToCommit(cr, result, crReasonNoWindow, noWindowMessage)
	case git.FinalizeWindowMismatch:
		// The author-bound refusal: deliberately not a failure — the foreign
		// window stays open for its own author — but the reason is surfaced.
		rejectSaveRequest(cr, crReasonWindowMismatch, windowMismatchMessage)
	case git.FinalizeAlreadyPresent:
		// The change already matches the remote, so the commit was dropped.
		resolveNothingToCommit(cr, result, crReasonAlreadyPresent, alreadyPresentMessage)
	default:
		// An empty or unknown outcome with no error is a bug, not a benign
		// rejection; fail it so it is not silently hidden.
		failSaveRequest(cr, crReasonUnexpectedOutcome, "unexpected finalize outcome: "+string(result.Outcome))
	}
}

// recordedEmptyMessage follows the cause when whenNothingToCommit: CommitEmpty recorded the request's
// message anyway. The commit proves the message is in Git; the reason still says why it is empty.
const recordedEmptyMessage = "; the message was recorded in an empty commit"

// resolveNothingToCommit records an outcome with nothing to commit. Without a commit it is a benign
// rejection. With one, whenNothingToCommit: CommitEmpty recorded the message: the reason keeps the
// cause, and status.commit and Pushed=True say the empty commit reached the remote.
func resolveNothingToCommit(cr *configv1alpha3.SaveRequest, result git.FinalizeResult, reason, message string) {
	if result.Commit == "" {
		rejectSaveRequest(cr, reason, message)
		return
	}
	cr.Status.Commit = result.Commit
	message += recordedEmptyMessage
	setSaveRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setSaveRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setSaveRequestCondition(cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded,
		"the empty commit was pushed to the remote repository")
	setSaveRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, reason, message)
}

// rejectSaveRequest records a benign terminal outcome that produced no commit:
// Ready=True (the request was serviced correctly, with the specific reason),
// Pushed=False, and Stalled=False — kstatus Current, not Failed.
func rejectSaveRequest(cr *configv1alpha3.SaveRequest, reason, message string) {
	setSaveRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setSaveRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setSaveRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, message)
	setSaveRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, reason, message)
}

// failSaveRequest records a hard terminal failure: Ready=False, Pushed=False, and
// Stalled=True — kstatus Failed, a human-fixable block.
func failSaveRequest(cr *configv1alpha3.SaveRequest, reason, message string) {
	setSaveRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setSaveRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, "no commit was pushed")
	setSaveRequestCondition(cr, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
	setSaveRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
}

// saveRequestIsTerminal reports whether the request has reached a terminal
// outcome: Ready=True (committed or benignly rejected) or Stalled=True (failed).
func saveRequestIsTerminal(cr *configv1alpha3.SaveRequest) bool {
	return conditionIsTrue(cr.Status.Conditions, ConditionTypeReady) ||
		conditionIsTrue(cr.Status.Conditions, ConditionTypeStalled)
}

// saveRequestConditionStatus returns the status of one condition as a loggable
// string, or "" when the condition is absent.
func saveRequestConditionStatus(cr *configv1alpha3.SaveRequest, conditionType string) string {
	if c := apimeta.FindStatusCondition(cr.Status.Conditions, conditionType); c != nil {
		return string(c.Status)
	}
	return ""
}

// saveRequestReadyReason returns the Ready condition's reason and message for
// logging (both empty when the condition is absent).
func saveRequestReadyReason(cr *configv1alpha3.SaveRequest) (string, string) {
	if c := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady); c != nil {
		return c.Reason, c.Message
	}
	return "", ""
}
