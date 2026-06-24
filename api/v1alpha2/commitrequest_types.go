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

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CommitRequestPhase enumerates the lifecycle states of a CommitRequest.
type CommitRequestPhase string

const (
	// CommitRequestPhaseWaitingForAuditEvent is the initial phase: the
	// CommitRequest's own create audit event — the source its author is
	// attributed from, and the anchor that orders the finalize after the
	// author's earlier changes — has not been observed yet, or the finalize
	// it gates (optional delay + audit-pipeline drain + commit) has not
	// completed.
	CommitRequestPhaseWaitingForAuditEvent CommitRequestPhase = "WaitingForAuditEvent"
	// CommitRequestPhaseCommitted is terminal: the open commit window was
	// finalized, pushed to the remote, and status.branch / status.sha are set.
	CommitRequestPhaseCommitted CommitRequestPhase = "Committed"
	// CommitRequestPhaseRejected is terminal: the request was handled correctly
	// but produced no commit. status.reason distinguishes why (NoWindowInGrace,
	// WindowMismatch, AlreadyPresent). This is not an error.
	CommitRequestPhaseRejected CommitRequestPhase = "Rejected"
	// CommitRequestPhaseFailed is terminal: the finalize could not be completed
	// (for example a failed local commit, a push that can never land, or
	// attribution that never arrived). status.message carries the failure detail.
	CommitRequestPhaseFailed CommitRequestPhase = "Failed"
)

// CommitRequestRejectReason explains a Rejected CommitRequest: the request was
// handled correctly but produced no commit. It is set only when phase is Rejected.
// +kubebuilder:validation:Enum=NoWindowInGrace;WindowMismatch;AlreadyPresent
type CommitRequestRejectReason string

const (
	// RejectNoWindowInGrace means the grace period elapsed with no matching
	// same-author window — nothing was pending to save.
	RejectNoWindowInGrace CommitRequestRejectReason = "NoWindowInGrace"
	// RejectWindowMismatch means an open window existed but belonged to a different
	// author or GitTarget, so it was deliberately left untouched.
	RejectWindowMismatch CommitRequestRejectReason = "WindowMismatch"
	// RejectAlreadyPresent means a matching window was finalized but produced no
	// diff — the change already matches the remote, so the commit was dropped (loop
	// prevention).
	RejectAlreadyPresent CommitRequestRejectReason = "AlreadyPresent"
)

// CommitRequestSpec defines the desired state of CommitRequest. The spec is
// immutable after creation: a CEL validation rule rejects any update that
// changes it, so a delayed audit event always acts on the spec the object was
// created with.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="CommitRequest spec is immutable after creation"
type CommitRequestSpec struct {
	// TargetRef names the GitTarget whose open commit window to finalize.
	// The GitTarget must be in the same namespace as this CommitRequest.
	// +required
	TargetRef LocalTargetReference `json:"targetRef"`

	// Message is an optional commit message for the finalized commit. When
	// omitted, the generated grouped-commit message is used.
	//
	// When present it is limited to 1-1024 Unicode characters and used
	// verbatim as the commit message. Newlines are allowed so a subject and
	// body can be supplied; all other ASCII control characters (including tab
	// and carriage return) are rejected.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^\x00-\x09\x0B-\x1F\x7F]*$`
	Message string `json:"message,omitempty"`

	// DelaySeconds optionally holds the finalize for this many seconds after
	// the CommitRequest's creation, acting as an extra collect window:
	// changes the author makes in the meantime still join the open commit
	// window and are included in the finalized commit. Omitted or 0 finalizes
	// as soon as the CommitRequest is attributed to its author. The window
	// can still be closed earlier by another author's change or by the
	// provider's commit window timer, exactly as without a CommitRequest.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	DelaySeconds int32 `json:"delaySeconds,omitempty"`
}

// CommitRequestStatus defines the observed state of CommitRequest.
type CommitRequestStatus struct {
	// Phase is the lifecycle state of this CommitRequest.
	// +optional
	// +kubebuilder:validation:Enum=WaitingForAuditEvent;Committed;Rejected;Failed
	Phase CommitRequestPhase `json:"phase,omitempty"`

	// Reason explains a Rejected phase: the machine-readable discriminator that
	// status consumers and tests assert on. Empty for non-Rejected phases.
	// +optional
	Reason CommitRequestRejectReason `json:"reason,omitempty"`

	// Message is a human-readable detail for the terminal phase. When Phase is
	// Failed it carries the reason the finalize could not complete; when Phase is
	// Rejected it carries the prose for status.reason.
	// +optional
	Message string `json:"message,omitempty"`

	// Branch is the Git branch the commit landed on. Set when Phase is Committed.
	// +optional
	Branch string `json:"branch,omitempty"`

	// SHA is the resulting commit SHA. Set when Phase is Committed.
	// +optional
	SHA string `json:"sha,omitempty"`

	// ObservedTime is the timestamp at which the terminal phase was recorded.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.status.branch`
// +kubebuilder:printcolumn:name="SHA",type=string,JSONPath=`.status.sha`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.spec.message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CommitRequest is a one-shot "save" signal: creating one finalizes the open
// commit window for the referenced GitTarget instead of waiting for the
// silence timer. The resulting commit SHA is reported back in status.
type CommitRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of CommitRequest
	// +required
	Spec CommitRequestSpec `json:"spec"`

	// status defines the observed state of CommitRequest
	// +optional
	Status CommitRequestStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CommitRequestList contains a list of CommitRequest.
type CommitRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CommitRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CommitRequest{}, &CommitRequestList{})
}
