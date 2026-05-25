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

package queue

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

const (
	// commitRequestResource is the plural resource name of the CommitRequest CRD.
	commitRequestResource = "commitrequests"

	// commitRequestMessageMaxBytes caps the commit message length defensively;
	// the CRD already validates length, this guards against oversized input
	// arriving by any other path.
	commitRequestMessageMaxBytes = 1024

	// commitRequestStatusUpdateAttempts bounds the status-update conflict retry.
	commitRequestStatusUpdateAttempts = 3
)

// isCommitRequestCreate reports whether the audit event is the `create` of an
// CommitRequest object (and not, for example, an `commitrequests/status`
// update emitted by the controller).
func (c *AuditConsumer) isCommitRequestCreate(event auditv1.Event) bool {
	ref := event.ObjectRef
	if ref == nil {
		return false
	}
	if !strings.EqualFold(event.Verb, "create") {
		return false
	}
	if ref.Subresource != "" {
		return false
	}
	if ref.Resource != commitRequestResource {
		return false
	}
	group, _ := auditutil.ObjectRefGroupVersion(ref)
	return group == configv1alpha1.GroupVersion.Group
}

// handleCommitRequest reacts to a CommitRequest `create` audit event: it
// reads the persisted object, finalizes the open commit window for the
// referenced GitTarget, and records the terminal phase in the object's status.
//
// By audit-stream ordering every resource mutation the author made before
// creating the CommitRequest produced an earlier audit event, so by the time
// this runs those writes have already been applied to the open window.
//
// The object identity is resolved through auditutil.IdentityFromAuditEvent so
// that `metadata.generateName` creates — whose audit objectRef carries no
// name — are handled by reading the server-allocated name from
// responseObject.metadata.
func (c *AuditConsumer) handleCommitRequest(ctx context.Context, log logr.Logger, event auditv1.Event) {
	identity := auditutil.IdentityFromAuditEvent(event, configv1alpha1.OperationCreate)
	log = log.WithValues("commitRequest", identity.Namespace+"/"+identity.Name)

	if c.kubeClient == nil || c.apiReader == nil {
		log.Info("CommitRequest handling disabled: no Kubernetes client configured; skipping")
		return
	}

	if identity.Namespace == "" || identity.Name == "" {
		// A create audit event without a resolvable name (e.g. a body-less event
		// for a generateName create) cannot be acted on. The persisted object
		// will be reconciled via the regular window-close path; nothing to do.
		log.Info("CommitRequest audit event did not identify an object; skipping",
			"namespace", identity.Namespace, "name", identity.Name)
		return
	}

	var commitRequest configv1alpha1.CommitRequest
	if err := c.apiReader.Get(ctx, client.ObjectKey{
		Namespace: identity.Namespace,
		Name:      identity.Name,
	}, &commitRequest); err != nil {
		if apierrors.IsNotFound(err) {
			// The object was deleted before its audit event was processed.
			// There is no status left to write, so just skip.
			log.Info("CommitRequest no longer exists; skipping")
			return
		}
		log.Error(err, "Failed to read CommitRequest; skipping")
		return
	}

	// Honor the identity the audit event gave us: a delayed event must not act
	// on a same-named object that was deleted and recreated since.
	if !auditIdentityMatchesObject(identity.UID, commitRequest.UID) {
		log.Info("CommitRequest UID does not match the audit event; skipping stale event",
			"eventUID", identity.UID, "objectUID", commitRequest.UID)
		return
	}

	if isTerminalCommitRequestPhase(commitRequest.Status.Phase) {
		log.V(1).Info("CommitRequest already in a terminal phase; skipping",
			"phase", commitRequest.Status.Phase)
		return
	}

	author := resolveUserInfo(event).Username
	message := capCommitRequestMessage(commitRequest.Spec.Message)

	result, err := c.eventRouter.FinalizeGitTargetWindow(
		ctx,
		author,
		commitRequest.Spec.GitTargetRef.Name,
		commitRequest.Namespace,
		message,
	)
	if err != nil {
		log.Error(err, "Failed to finalize commit window for CommitRequest",
			"author", author, "gitTarget", commitRequest.Spec.GitTargetRef.Name)
	}

	c.writeCommitRequestStatus(ctx, log, identity.Namespace, identity.Name, commitRequest.UID, result, err)
}

// writeCommitRequestStatus records the terminal phase produced by a finalize
// signal onto the CommitRequest object, retrying on optimistic-concurrency
// conflicts. A non-nil finalizeErr records the Failed terminal phase.
func (c *AuditConsumer) writeCommitRequestStatus(
	ctx context.Context,
	log logr.Logger,
	namespace, name string,
	expectedUID types.UID,
	result git.FinalizeResult,
	finalizeErr error,
) {
	now := metav1.Now()

	for attempt := 1; attempt <= commitRequestStatusUpdateAttempts; attempt++ {
		var commitRequest configv1alpha1.CommitRequest
		if err := c.apiReader.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}, &commitRequest); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("CommitRequest deleted before status could be written; skipping")
				return
			}
			log.Error(err, "Failed to re-read CommitRequest for status update")
			return
		}

		// The object may have been deleted and recreated between the finalize
		// and this write; never stamp status onto a different incarnation.
		if expectedUID != "" && commitRequest.UID != expectedUID {
			log.Info("CommitRequest UID changed before status could be written; skipping",
				"expectedUID", expectedUID, "objectUID", commitRequest.UID)
			return
		}

		if isTerminalCommitRequestPhase(commitRequest.Status.Phase) {
			// A concurrent processing of the same audit event (e.g. an
			// auto-claimed redelivery) already wrote the terminal phase.
			return
		}

		applyFinalizeResultToStatus(&commitRequest, result, finalizeErr, now)

		if err := c.kubeClient.Status().Update(ctx, &commitRequest); err != nil {
			if apierrors.IsConflict(err) {
				log.V(1).Info("Conflict writing CommitRequest status; retrying", "attempt", attempt)
				continue
			}
			log.Error(err, "Failed to write CommitRequest status")
			return
		}

		log.Info("CommitRequest finalized",
			"phase", commitRequest.Status.Phase,
			"branch", commitRequest.Status.Branch,
			"sha", commitRequest.Status.SHA,
			"message", commitRequest.Status.Message)
		return
	}

	log.Error(nil, "Gave up writing CommitRequest status after repeated conflicts")
}

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
		commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseNoOpenWindow
	default:
		// An empty or unknown outcome with no error is a bug, not a benign
		// "no open window"; record it as Failed so it is not silently hidden.
		commitRequest.Status.Phase = configv1alpha1.CommitRequestPhaseFailed
		commitRequest.Status.Message = "unexpected finalize outcome: " + string(result.Outcome)
	}
}

// isTerminalCommitRequestPhase reports whether the phase is one of the
// terminal states (anything other than the initial WaitingForAuditEvent).
func isTerminalCommitRequestPhase(phase configv1alpha1.CommitRequestPhase) bool {
	return phase == configv1alpha1.CommitRequestPhaseCommitted ||
		phase == configv1alpha1.CommitRequestPhaseNoOpenWindow ||
		phase == configv1alpha1.CommitRequestPhaseFailed
}

// auditIdentityMatchesObject reports whether the audit event identifies the
// fetched object by UID. An empty event UID (e.g. a Metadata-level audit
// policy that omits it, or a bodyless event whose objectRef carried no UID)
// is treated as a match.
func auditIdentityMatchesObject(eventUID, objectUID types.UID) bool {
	if eventUID == "" {
		return true
	}
	return eventUID == objectUID
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
