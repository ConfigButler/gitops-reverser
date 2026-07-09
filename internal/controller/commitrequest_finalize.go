// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// commitRequestMessageMaxBytes caps the commit message length defensively;
// the CRD already validates length, this guards against oversized input
// arriving by any other path.
const commitRequestMessageMaxBytes = 1024

// CommitRequest condition reasons (CamelCase tokens surfaced on status.conditions).
const (
	crReasonWaitingForCloseDelay      = "WaitingForCloseDelay"
	crReasonCommitted                 = "Committed"
	crReasonNoWindowInGrace           = "NoWindowInGrace"
	crReasonWindowMismatch            = "WindowMismatch"
	crReasonAlreadyPresent            = "AlreadyPresent"
	crReasonFinalizeFailed            = "FinalizeFailed"
	crReasonUnexpectedOutcome         = "UnexpectedOutcome"
	crReasonAttributedFromAdmission   = "AttributedFromAdmission"
	crReasonAuthorAsserted            = "AuthorAsserted"
	crReasonAuthorAssertionUnverified = "AuthorAssertionUnverified"
	crReasonCommitterFallback         = "CommitterFallback"
	crReasonPushed                    = "Pushed"
)

// noWindowInGraceMessage is the prose for a NoWindowInGrace outcome: the grace
// elapsed with nothing pending to save.
const noWindowInGraceMessage = "no matching open commit window was collected within the grace; " +
	"nothing was pending to save"

// alreadyPresentMessage is the prose for an AlreadyPresent outcome: the finalized
// window produced no diff.
const alreadyPresentMessage = "the change already matches the remote, so no commit was made"

// notStalledMessage is the boilerplate message for a False Stalled condition.
const notStalledMessage = "the CommitRequest is not stalled"

// commitRequestAttribution is the settled author decision threaded from the
// attribute step into the terminal status write so the AuthorAttributed condition can
// record whether the commit author was named. It is binary and always settled at
// first sight (present-or-never, §2) — there is no pending or timed-out state.
type commitRequestAttribution int

const (
	// attributionFromAdmission means the submitter was captured at admission by the
	// validate-operator-types webhook and named as the commit author.
	attributionFromAdmission commitRequestAttribution = iota
	// attributionCommitter means no admission author record exists — the
	// validate-operator-types webhook is not configured (or did not record one) — so the
	// commit is authored by the configured committer.
	attributionCommitter
	// attributionAsserted means spec.author named the commit author and an admission
	// record confirms the requester held the assert-author verb on the GitTarget.
	attributionAsserted
	// attributionAssertionUnverified means spec.author was set but no authorized
	// admission record backs it: the webhook is off, was bypassed, or Redis is not
	// configured. The assertion is ignored and the commit is authored by the committer —
	// fail-closed, and independent of the webhook's failurePolicy.
	attributionAssertionUnverified
)

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

// Progress-condition messages. The author is settled synchronously at first sight
// (present-or-never, §2), so a CommitRequest has a single observable wait before it
// terminates: WaitingForCloseDelay — author settled, attached to the worker, waiting
// out the close delay before the window closes and the commit is made and pushed.
const (
	closeDelayMessage = "attached to the open commit window; waiting out the close delay " +
		"before the commit is made and pushed"
	pushPendingMessage = "the commit has not been pushed yet"
)

// setCommitRequestProgress stamps the four progress conditions (Ready=False,
// Reconciling=True, Stalled=False, Pushed=Unknown) with one unifying reason, so a
// single wait reads consistently across them.
func setCommitRequestProgress(cr *configv1alpha3.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionUnknown, reason, pushPendingMessage)
}

// markCommitRequestWaitingForCloseDelay records the single still-running state. The
// author is settled synchronously at first sight (present-or-never, §2), so there is no
// prior "waiting for the author" phase: the request is attributed and attached to the
// worker, waiting out the close delay before the window closes and the commit is made
// and pushed (Reconciling=True, reason WaitingForCloseDelay).
func markCommitRequestWaitingForCloseDelay(cr *configv1alpha3.CommitRequest, attribution commitRequestAttribution) {
	cr.Status.ObservedGeneration = cr.Generation
	setCommitRequestAttributed(cr, attribution)
	setCommitRequestProgress(cr, crReasonWaitingForCloseDelay, closeDelayMessage)
}

// setCommitRequestAttributed records the settled, binary author decision on the
// AuthorAttributed condition. False (CommitterFallback) is not a failure and does not
// affect Ready — it is the honest signal that no admission author record was found.
func setCommitRequestAttributed(cr *configv1alpha3.CommitRequest, attribution commitRequestAttribution) {
	switch attribution {
	case attributionAsserted:
		setCommitRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionTrue,
			crReasonAuthorAsserted,
			"spec.author named the commit author; the requester holds the assert-author verb on the GitTarget")
	case attributionAssertionUnverified:
		setCommitRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
			crReasonAuthorAssertionUnverified,
			"spec.author was set but no authorized admission record backs it (the validate-operator-types "+
				"webhook is not configured, was bypassed, or Redis is not set); the assertion was ignored "+
				"and the commit is authored by the configured committer")
	case attributionFromAdmission:
		setCommitRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionTrue,
			crReasonAttributedFromAdmission,
			"the submitter was captured at admission and named as the commit author")
	case attributionCommitter:
		setCommitRequestCondition(cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
			crReasonCommitterFallback,
			"no admission author record (the validate-operator-types webhook is not configured); "+
				"committed as the configured committer")
	}
}

// applyFinalizeResultToStatus maps a FinalizeResult (or a finalize error) and the
// settled attribution onto a CommitRequest's terminal conditions. Ready is the
// summary; Reconciling/Stalled carry kstatus; Attributed records the author
// decision; Pushed records whether a commit reached the remote.
func applyFinalizeResultToStatus(
	cr *configv1alpha3.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
	attribution commitRequestAttribution,
) {
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Branch = result.Branch
	setCommitRequestAttributed(cr, attribution)

	if finalizeErr != nil {
		failCommitRequest(cr, crReasonFinalizeFailed, finalizeErr.Error())
		return
	}

	switch result.Outcome {
	case git.FinalizeCommitted:
		cr.Status.SHA = result.SHA
		const committedMsg = "the open commit window was closed, committed, and pushed"
		setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, crReasonCommitted, committedMsg)
		setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, crReasonCommitted, notStalledMessage)
		setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionTrue, crReasonPushed,
			"the commit was pushed to the remote repository")
		setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted, committedMsg)
	case git.FinalizeNoOpenWindow:
		// Benign: the grace elapsed with nothing pending to save.
		rejectCommitRequest(cr, crReasonNoWindowInGrace, noWindowInGraceMessage)
	case git.FinalizeWindowMismatch:
		// The author-bound refusal: deliberately not a failure — the foreign
		// window stays open for its own author — but the reason is surfaced.
		rejectCommitRequest(cr, crReasonWindowMismatch, windowMismatchMessage)
	case git.FinalizeAlreadyPresent:
		// The change already matches the remote, so the commit was dropped.
		rejectCommitRequest(cr, crReasonAlreadyPresent, alreadyPresentMessage)
	default:
		// An empty or unknown outcome with no error is a bug, not a benign
		// rejection; fail it so it is not silently hidden.
		failCommitRequest(cr, crReasonUnexpectedOutcome, "unexpected finalize outcome: "+string(result.Outcome))
	}
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

// capCommitRequestMessage caps a user-supplied commit message at a defensive
// byte length. CRD validation already rejects control characters and bounds
// the length in Unicode characters, so the accepted message is used verbatim;
// this cap only guards against an object that somehow bypassed validation.
func capCommitRequestMessage(message string) string {
	if len(message) > commitRequestMessageMaxBytes {
		return truncateUTF8(message, commitRequestMessageMaxBytes)
	}
	return message
}

// truncateUTF8 returns the longest prefix of s that fits within maxBytes
// without splitting a multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}
