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

// CommitRequest condition reasons (CamelCase tokens surfaced on status.conditions).
const (
	crReasonCommitted          = "Committed"
	crReasonNoWindow           = "NoWindow"
	crReasonWindowMismatch     = "WindowMismatch"
	crReasonAlreadyPresent     = "AlreadyPresent"
	crReasonFinalizeFailed     = "FinalizeFailed"
	crReasonUnexpectedOutcome  = "UnexpectedOutcome"
	crReasonGitTargetRefPruned = "GitTargetRefPruned"
	crReasonNotAllowed         = "NotAllowed"
	crReasonSaveInProgress     = "SaveInProgress"
)

// The bounded `outcome` values on gitopsreverser_commit_requests_total.
//
// They are derived from git.FinalizeOutcome rather than from the condition reasons above: the
// FinalizeResult is what DECIDES, and the reasons include non-terminal values
// that have no business on a terminal counter. The set is closed, and outcomeFailed absorbs every
// hard failure so an unmapped result can never go uncounted.
const (
	crOutcomeCommitted      = "committed"
	crOutcomeNoWindow       = "no_window"
	crOutcomeWindowMismatch = "window_mismatch"
	crOutcomeAlreadyPresent = "already_present"
	crOutcomeSaveInProgress = "save_in_progress"
	crOutcomeFailed         = "failed"
)

// commitRequestOutcome maps one finalize result to its counter value.
//
// A finalize error outranks the outcome because FinalizeResult.Outcome is only set when Err is nil,
// and an unrecognized outcome counts as failed for the same reason applyFinalizeResultToStatus
// fails it: an empty or unknown outcome with no error is a bug, and a bug that increments nothing
// is a bug nobody sees.
func commitRequestOutcome(result git.FinalizeResult, finalizeErr error) string {
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
	case git.FinalizeSaveInProgress:
		return crOutcomeSaveInProgress
	default:
		return crOutcomeFailed
	}
}

// commitRequestTarget is the GitTarget identity one terminal outcome is counted under.
//
// The zero value is the documented FALLBACK for a request whose target could not be resolved, and
// it publishes empty labels rather than the requested name. That is deliberate: spec.gitTargetRef
// is whatever the client wrote, so labelling an unresolved reference would let anyone with create
// rights on commitrequests mint an unbounded number of series by naming targets that do not exist.
// The requested reference is still on the object's conditions and in the logs, which is where an
// unbounded string belongs.
type commitRequestTarget struct {
	namespace string
	name      string
}

// resolvedCommitRequestTarget names the GitTarget a request was serviced against.
//
// It is only correct to call once the workflow has RESOLVED that target, which
// EventRouter.ServiceCommitRequest does by getting the GitTarget and returning an error when it is
// absent. So a nil service error is the proof that this identity exists, and no extra API lookup is
// needed to bound the label — the plan's rule that metric recording must not block terminal status
// on another failing call.
//
// The key is CommitRequest.GitTargetKey, the GitTarget the attach names, which is the key
// ServiceCommitRequest gets.
func resolvedCommitRequestTarget(cr *configv1alpha3.CommitRequest) commitRequestTarget {
	key := cr.GitTargetKey()
	return commitRequestTarget{namespace: key.Namespace, name: key.Name}
}

// recordCommitRequestOutcome increments the terminal-outcome counter once.
//
// It is called at the point the outcome is SETTLED, never from inside applyFinalizeResultToStatus:
// that runs inside writeTerminalStatus's conflict-retry loop, up to commitRequestStatusUpdateAttempts
// times for one request, so recording there would over-report exactly the contended case. The
// counter measures what the pipeline decided; whether the terminal status then reached the API
// server is a separate failure, already logged.
//
// See the instrument's doc comment for what this does and does not promise across reconciles.
func recordCommitRequestOutcome(ctx context.Context, outcome string, target commitRequestTarget) {
	if telemetry.CommitRequestsTotal == nil {
		return
	}
	telemetry.CommitRequestsTotal.Add(ctx, 1, metric.WithAttributes(
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
const notStalledMessage = "the CommitRequest is not stalled"

// setCommitRequestCondition upserts a condition keyed by type, stamping it with
// the request's generation so kstatus can tell a current status from a stale one.
func setCommitRequestCondition(
	cr *configv1alpha3.CommitRequest,
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
func progressFor(phase git.CommitRequestPhase, held string) (string, string) {
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

// setCommitRequestProgress stamps the four progress conditions (Ready=False,
// Reconciling=True, Stalled=False, Pushed=Unknown) with one unifying reason, so a
// single wait reads consistently across them.
func setCommitRequestProgress(cr *configv1alpha3.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionUnknown, reason, pushPendingMessage)
}

// markCommitRequestProgressing records the still-running state for a worker-reported phase
// (Reconciling=True). An empty phase is the generic Progressing: sent, not yet confirmed.
func markCommitRequestProgressing(
	cr *configv1alpha3.CommitRequest,
	phase git.CommitRequestPhase,
	held string,
) {
	cr.Status.ObservedGeneration = cr.Generation
	reason, message := progressFor(phase, held)
	setCommitRequestProgress(cr, reason, message)
}

// applyFinalizeResultToStatus maps a FinalizeResult (or a finalize error) onto a
// CommitRequest's terminal conditions. Ready is the summary; Reconciling/Stalled
// carry kstatus; Pushed records whether a commit reached the remote.
func applyFinalizeResultToStatus(
	cr *configv1alpha3.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
) {
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Branch = result.Branch

	if finalizeErr != nil {
		reason := crReasonFinalizeFailed
		if errors.Is(finalizeErr, errCommitRequestNotAllowed) {
			reason = crReasonNotAllowed
		}
		failCommitRequest(cr, reason, finalizeErr.Error())
		return
	}

	switch result.Outcome {
	case git.FinalizeCommitted:
		cr.Status.Commit = result.Commit
		const committedMsg = "the open commit window was closed, committed, and pushed"
		setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, crReasonCommitted, committedMsg)
		setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, crReasonCommitted, notStalledMessage)
		setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded,
			"the commit was pushed to the remote repository")
		setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted, committedMsg)
	case git.FinalizeNoOpenWindow:
		// Benign: the attach timeout ran out with nothing pending to save.
		resolveNothingToCommit(cr, result, crReasonNoWindow, noWindowMessage)
	case git.FinalizeWindowMismatch:
		// The author-bound refusal: deliberately not a failure — the foreign
		// window stays open for its own author — but the reason is surfaced.
		rejectCommitRequest(cr, crReasonWindowMismatch, windowMismatchMessage)
	case git.FinalizeAlreadyPresent:
		// The change already matches the remote, so the commit was dropped.
		resolveNothingToCommit(cr, result, crReasonAlreadyPresent, alreadyPresentMessage)
	case git.FinalizeSaveInProgress:
		// Refused, not failed by the worker: the spec is immutable, so the caller saves again with a
		// new request once the earlier one resolved.
		failCommitRequest(cr, crReasonSaveInProgress, saveInProgressMessage)
	default:
		// An empty or unknown outcome with no error is a bug, not a benign
		// rejection; fail it so it is not silently hidden.
		failCommitRequest(cr, crReasonUnexpectedOutcome, "unexpected finalize outcome: "+string(result.Outcome))
	}
}

// recordedEmptyMessage follows the cause when whenNothingToCommit: CommitEmpty recorded the request's
// message anyway. The commit proves the message is in Git; the reason still says why it is empty.
const recordedEmptyMessage = "; the message was recorded in an empty commit"

// resolveNothingToCommit records an outcome with nothing to commit. Without a commit it is a benign
// rejection. With one, whenNothingToCommit: CommitEmpty recorded the message: the reason keeps the
// cause, and status.commit and Pushed=True say the empty commit reached the remote.
func resolveNothingToCommit(cr *configv1alpha3.CommitRequest, result git.FinalizeResult, reason, message string) {
	if result.Commit == "" {
		rejectCommitRequest(cr, reason, message)
		return
	}
	cr.Status.Commit = result.Commit
	message += recordedEmptyMessage
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded,
		"the empty commit was pushed to the remote repository")
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, reason, message)
}

// rejectCommitRequest records a benign terminal outcome that produced no commit:
// Ready=True (the request was serviced correctly, with the specific reason),
// Pushed=False, and Stalled=False — kstatus Current, not Failed.
func rejectCommitRequest(cr *configv1alpha3.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, reason, message)
}

// failCommitRequest records a hard terminal failure: Ready=False, Pushed=False, and
// Stalled=True — kstatus Failed, a human-fixable block.
func failCommitRequest(cr *configv1alpha3.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, "no commit was pushed")
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
}

// commitRequestIsTerminal reports whether the request has reached a terminal
// outcome: Ready=True (committed or benignly rejected) or Stalled=True (failed).
func commitRequestIsTerminal(cr *configv1alpha3.CommitRequest) bool {
	return conditionIsTrue(cr.Status.Conditions, ConditionTypeReady) ||
		conditionIsTrue(cr.Status.Conditions, ConditionTypeStalled)
}

// commitRequestConditionStatus returns the status of one condition as a loggable
// string, or "" when the condition is absent.
func commitRequestConditionStatus(cr *configv1alpha3.CommitRequest, conditionType string) string {
	if c := apimeta.FindStatusCondition(cr.Status.Conditions, conditionType); c != nil {
		return string(c.Status)
	}
	return ""
}

// commitRequestReadyReason returns the Ready condition's reason and message for
// logging (both empty when the condition is absent).
func commitRequestReadyReason(cr *configv1alpha3.CommitRequest) (string, string) {
	if c := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady); c != nil {
		return c.Reason, c.Message
	}
	return "", ""
}
