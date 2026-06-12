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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// commitRequestMessageMaxBytes caps the commit message length defensively;
// the CRD already validates length, this guards against oversized input
// arriving by any other path.
const commitRequestMessageMaxBytes = 1024

// noWindowInGraceMessage is the prose for a Rejected/NoWindowInGrace request: the
// grace elapsed with nothing pending to save.
const noWindowInGraceMessage = "no matching open commit window was collected within the grace; " +
	"nothing was pending to save"

// alreadyPresentMessage is the prose for a Rejected/AlreadyPresent request: the
// finalized window produced no diff.
const alreadyPresentMessage = "the change already matches the remote, so no commit was made"

// applyFinalizeResultToStatus maps a FinalizeResult (or a finalize error) onto
// a CommitRequest's status.
func applyFinalizeResultToStatus(
	commitRequest *configv1alpha1.CommitRequest,
	result git.FinalizeResult,
	finalizeErr error,
	now metav1.Time,
) {
	commitRequest.Status.ObservedTime = &now
	commitRequest.Status.Branch = result.Branch
	commitRequest.Status.Message = ""
	commitRequest.Status.Reason = ""

	if finalizeErr != nil {
		commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseFailed
		commitRequest.Status.Message = finalizeErr.Error()
		return
	}

	switch result.Outcome {
	case git.FinalizeCommitted:
		commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseCommitted
		commitRequest.Status.SHA = result.SHA
	case git.FinalizeNoOpenWindow:
		// Benign: the grace elapsed with nothing pending to save.
		rejectCommitRequest(commitRequest, configv1alpha1.RejectNoWindowInGrace, noWindowInGraceMessage)
	case git.FinalizeWindowMismatch:
		// The author-bound refusal: deliberately not a failure — the foreign
		// window stays open for its own author — but the reason is surfaced.
		rejectCommitRequest(commitRequest, configv1alpha1.RejectWindowMismatch, windowMismatchMessage)
	case git.FinalizeAlreadyPresent:
		// The change already matches the remote, so the commit was dropped.
		rejectCommitRequest(commitRequest, configv1alpha1.RejectAlreadyPresent, alreadyPresentMessage)
	default:
		// An empty or unknown outcome with no error is a bug, not a benign
		// rejection; record it as Failed so it is not silently hidden.
		commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseFailed
		commitRequest.Status.Message = "unexpected finalize outcome: " + string(result.Outcome)
	}
}

// rejectCommitRequest stamps the Rejected phase with its structured reason and prose.
func rejectCommitRequest(
	commitRequest *configv1alpha1.CommitRequest,
	reason configv1alpha1.CommitRequestRejectReason,
	message string,
) {
	commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseRejected
	commitRequest.Status.Reason = reason
	commitRequest.Status.Message = message
}

// isTerminalCommitRequestPhase reports whether the phase is one of the
// terminal states (anything other than the initial WaitingForAuditEvent).
func isTerminalCommitRequestPhase(phase configv1alpha1.CommitRequestPhase) bool {
	return phase == configv1alpha1.CommitRequestPhaseCommitted ||
		phase == configv1alpha1.CommitRequestPhaseRejected ||
		phase == configv1alpha1.CommitRequestPhaseFailed
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
