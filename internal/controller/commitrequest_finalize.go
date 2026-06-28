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

package controller

import (
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// commitRequestMessageMaxBytes caps the commit message length defensively;
// the CRD already validates length, this guards against oversized input
// arriving by any other path.
const commitRequestMessageMaxBytes = 1024

// CommitRequest condition reasons (CamelCase tokens surfaced on status.conditions).
const (
	crReasonWaitingForAuditEvent   = "WaitingForAuditEvent"
	crReasonWaitingForCloseDelay   = "WaitingForCloseDelay"
	crReasonCommitted              = "Committed"
	crReasonNoWindowInGrace        = "NoWindowInGrace"
	crReasonWindowMismatch         = "WindowMismatch"
	crReasonAlreadyPresent         = "AlreadyPresent"
	crReasonFinalizeFailed         = "FinalizeFailed"
	crReasonUnexpectedOutcome      = "UnexpectedOutcome"
	crReasonAttributionNotRequired = "AttributionNotRequired"
	crReasonAttributedFromAudit    = "AttributedFromAuditEvent"
	crReasonAuditEventNotObserved  = "AuditEventNotObserved"
	crReasonPushed                 = "Pushed"
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
// attribute step into the terminal status write so the Attributed condition can
// record how (and whether) the commit author was named.
type commitRequestAttribution int

const (
	// attributionPending means the attribution decision is not settled yet
	// (attributed mode, still waiting for the create audit event).
	attributionPending commitRequestAttribution = iota
	// attributionNotRequired means attribution is disabled (committer-only); the
	// author is settled immediately as the configured committer.
	attributionNotRequired
	// attributionResolved means the author was named from the create audit event.
	attributionResolved
	// attributionTimedOut means the audit event never arrived within the bound, so
	// the commit is authored by the configured committer.
	attributionTimedOut
)

// setCommitRequestCondition upserts a condition keyed by type, stamping it with
// the request's generation so kstatus can tell a current status from a stale one.
func setCommitRequestCondition(
	cr *configv1alpha2.CommitRequest,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	cr.Status.Conditions = upsertCondition(cr.Status.Conditions, conditionType, status, reason, message, cr.Generation)
}

// Progress-condition messages. A CommitRequest passes through two distinct,
// observable waits before it terminates: WaitingForAuditEvent (waiting to learn the
// author) and WaitingForCloseDelay (author settled, attached to the worker, waiting
// out the close delay before the window closes and the commit is made and pushed).
const (
	waitingForAuditMessage = "waiting for the CommitRequest's create audit event to attribute the author"
	closeDelayMessage      = "attached to the open commit window; waiting out the close delay " +
		"before the commit is made and pushed"
	pushPendingMessage = "the commit has not been pushed yet"
)

// setCommitRequestProgress stamps the four progress conditions (Ready=False,
// Reconciling=True, Stalled=False, Pushed=Unknown) with one unifying reason, so a
// single wait reads consistently across them.
func setCommitRequestProgress(cr *configv1alpha2.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionUnknown, reason, pushPendingMessage)
}

// markCommitRequestInProgress stamps the first-sight, still-running conditions.
// attributionRequired is false in committer-only mode, where the author is settled
// immediately (Attributed=True) and the request enters the WaitingForCloseDelay wait
// at once; otherwise it starts in the WaitingForAuditEvent wait (Attributed=Unknown)
// until its create audit event arrives.
func markCommitRequestInProgress(cr *configv1alpha2.CommitRequest, attributionRequired bool) {
	cr.Status.ObservedGeneration = cr.Generation
	if attributionRequired {
		setCommitRequestProgress(cr, crReasonWaitingForAuditEvent, waitingForAuditMessage)
		setCommitRequestCondition(cr, ConditionTypeAttributed, metav1.ConditionUnknown,
			crReasonWaitingForAuditEvent, "waiting for the create audit event that names the author")
		return
	}
	// Committer-only: no audit wait — settle the author and enter the close-delay wait.
	setCommitRequestAttributed(cr, attributionNotRequired)
	setCommitRequestProgress(cr, crReasonWaitingForCloseDelay, closeDelayMessage)
}

// markCommitRequestWaitingForCloseDelay records the post-attribution wait — the
// second of the two progress waits — as its own observable state: the author is
// settled (Attributed), and the request is attached to the worker, waiting out the
// close delay before the window closes and the commit is made and pushed
// (Reconciling=True, reason WaitingForCloseDelay).
func markCommitRequestWaitingForCloseDelay(cr *configv1alpha2.CommitRequest, attribution commitRequestAttribution) {
	cr.Status.ObservedGeneration = cr.Generation
	setCommitRequestAttributed(cr, attribution)
	setCommitRequestProgress(cr, crReasonWaitingForCloseDelay, closeDelayMessage)
}

// setCommitRequestAttributed records the settled author decision on the Attributed
// condition. attributionPending is left to the in-progress stamp and never reaches here.
func setCommitRequestAttributed(cr *configv1alpha2.CommitRequest, attribution commitRequestAttribution) {
	switch attribution {
	case attributionResolved:
		setCommitRequestCondition(cr, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributedFromAudit,
			"the author was attributed from the CommitRequest's create audit event")
	case attributionTimedOut:
		setCommitRequestCondition(cr, ConditionTypeAttributed, metav1.ConditionFalse, crReasonAuditEventNotObserved,
			"the create audit event was not observed within the bound; committed as the configured committer")
	case attributionNotRequired, attributionPending:
		setCommitRequestCondition(cr, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributionNotRequired,
			"attribution is disabled; committing as the configured committer")
	}
}

// applyFinalizeResultToStatus maps a FinalizeResult (or a finalize error) and the
// settled attribution onto a CommitRequest's terminal conditions. Ready is the
// summary; Reconciling/Stalled carry kstatus; Attributed records the author
// decision; Pushed records whether a commit reached the remote.
func applyFinalizeResultToStatus(
	cr *configv1alpha2.CommitRequest,
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
func rejectCommitRequest(cr *configv1alpha2.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionFalse, reason, notStalledMessage)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionTrue, reason, message)
}

// failCommitRequest records a hard terminal failure: Ready=False, Pushed=False, and
// Stalled=True — kstatus Failed, a human-fixable block.
func failCommitRequest(cr *configv1alpha2.CommitRequest, reason, message string) {
	setCommitRequestCondition(cr, ConditionTypeReconciling, metav1.ConditionFalse, reason, message)
	setCommitRequestCondition(cr, ConditionTypePushed, metav1.ConditionFalse, reason, "no commit was pushed")
	setCommitRequestCondition(cr, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
	setCommitRequestCondition(cr, ConditionTypeReady, metav1.ConditionFalse, reason, message)
}

// commitRequestIsTerminal reports whether the request has reached a terminal
// outcome: Ready=True (committed or benignly rejected) or Stalled=True (failed).
func commitRequestIsTerminal(cr *configv1alpha2.CommitRequest) bool {
	return conditionIsTrue(cr.Status.Conditions, ConditionTypeReady) ||
		conditionIsTrue(cr.Status.Conditions, ConditionTypeStalled)
}

// commitRequestConditionStatus returns the status of one condition as a loggable
// string, or "" when the condition is absent.
func commitRequestConditionStatus(cr *configv1alpha2.CommitRequest, conditionType string) string {
	if c := apimeta.FindStatusCondition(cr.Status.Conditions, conditionType); c != nil {
		return string(c.Status)
	}
	return ""
}

// commitRequestReadyReason returns the Ready condition's reason and message for
// logging (both empty when the condition is absent).
func commitRequestReadyReason(cr *configv1alpha2.CommitRequest) (string, string) {
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
