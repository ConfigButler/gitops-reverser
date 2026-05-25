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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CommitRequestPhase enumerates the lifecycle states of a CommitRequest.
type CommitRequestPhase string

const (
	// CommitRequestPhaseWaitingForAuditEvent is the initial phase: the object
	// was created but gitops-reverser has not yet observed its audit event.
	CommitRequestPhaseWaitingForAuditEvent CommitRequestPhase = "WaitingForAuditEvent"
	// CommitRequestPhaseCommitted is terminal: the open commit window was
	// finalized and status.branch / status.sha are set.
	CommitRequestPhaseCommitted CommitRequestPhase = "Committed"
	// CommitRequestPhaseNoOpenWindow is terminal: the audit event arrived but
	// there was no open commit window to finalize. This is not an error.
	CommitRequestPhaseNoOpenWindow CommitRequestPhase = "NoOpenWindow"
	// CommitRequestPhaseFailed is terminal: the audit event arrived but the
	// open commit window could not be finalized (for example a failed local
	// commit or a saturated branch-worker queue). status.message carries the
	// failure detail.
	CommitRequestPhaseFailed CommitRequestPhase = "Failed"
)

// CommitRequestGitTargetReference references the GitTarget whose open commit
// window should be finalized. The GitTarget must live in the same namespace as
// the CommitRequest.
type CommitRequestGitTargetReference struct {
	// Name of the referenced GitTarget.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// CommitRequestSpec defines the desired state of CommitRequest. The spec is
// immutable after creation: a CEL validation rule rejects any update that
// changes it, so a delayed audit event always acts on the spec the object was
// created with.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="CommitRequest spec is immutable after creation"
type CommitRequestSpec struct {
	// GitTargetRef names the GitTarget whose open commit window to finalize.
	// The GitTarget must be in the same namespace as this CommitRequest.
	// +required
	GitTargetRef CommitRequestGitTargetReference `json:"gitTargetRef"`

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
}

// CommitRequestStatus defines the observed state of CommitRequest.
type CommitRequestStatus struct {
	// Phase is the lifecycle state of this CommitRequest.
	// +optional
	// +kubebuilder:validation:Enum=WaitingForAuditEvent;Committed;NoOpenWindow;Failed
	Phase CommitRequestPhase `json:"phase,omitempty"`

	// Message is a human-readable detail for the terminal phase. It is set when
	// Phase is Failed and carries the reason the finalize could not complete.
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
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
