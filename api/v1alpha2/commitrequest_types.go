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

	// CloseDelaySeconds optionally delays closing the open commit window for this
	// many seconds after the CommitRequest is attributed, acting as an extra collect
	// window: changes the author makes in the meantime still join the open commit
	// window and are included in the resulting commit. Omitted or 0 closes the window
	// as soon as the CommitRequest is attributed to its author. The window can still
	// be closed earlier by another author's change or by the provider's commit window
	// timer, exactly as without a CommitRequest.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	CloseDelaySeconds int32 `json:"closeDelaySeconds,omitempty"`
}

// CommitRequestStatus defines the observed state of CommitRequest. Progress and
// outcome are reported entirely through conditions (kstatus-compatible), so the
// object carries no lifecycle phase string:
//
//   - Ready (summary): True once the request reached a terminal outcome that is not
//     an error — a pushed commit, or a benign no-commit (nothing to save, already
//     present, or a foreign open window). False while in progress or when it failed.
//   - Reconciling / Stalled: the kstatus progress / blocked pair. Reconciling=True
//     while finalizing; Stalled=True when the finalize failed and needs attention.
//   - Attributed (domain): True once the author is settled — immediately True when
//     attribution is not required (committer-only), True when resolved from the
//     create audit event, and False if the audit event never arrived and the commit
//     was authored by the configured committer.
//   - Pushed (domain): True once the commit is in the remote repository.
type CommitRequestStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report the request's progress and terminal outcome: the Ready
	// summary, the kstatus Reconciling/Stalled pair, and the domain conditions
	// Attributed and Pushed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Branch is the Git branch the GitTarget commits to. Populated once the
	// finalize resolves.
	// +optional
	Branch string `json:"branch,omitempty"`

	// SHA is the resulting commit SHA. Set when the commit was pushed (Pushed=True).
	// +optional
	SHA string `json:"sha,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Attributed",type=string,JSONPath=`.status.conditions[?(@.type=="Attributed")].status`
// +kubebuilder:printcolumn:name="Pushed",type=string,JSONPath=`.status.conditions[?(@.type=="Pushed")].status`
// +kubebuilder:printcolumn:name="SHA",type=string,JSONPath=`.status.sha`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
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
