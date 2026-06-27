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

// GitProviderReference references the GitProvider that backs a GitTarget. Many GitTargets may
// reference the same GitProvider; the reference is always to a GitProvider in the GitTarget's own
// namespace. Group and Kind are typed (with defaults) for consistency with the project's other
// local references and so the schema is explicit about what it accepts — currently only
// configbutler.ai/GitProvider.
type GitProviderReference struct {
	// API Group of the referent.
	// +kubebuilder:default=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// Optional because this reference currently only supports a single kind (GitProvider).
	// Keeping it optional allows users to omit it while still benefiting from CRD defaulting.
	// +optional
	// +kubebuilder:validation:Enum=GitProvider
	// +kubebuilder:default=GitProvider
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	Name string `json:"name"`
}

// GitTargetSpec defines the desired state of GitTarget.
//
// The destination fields — providerRef, branch, and path — are immutable. A
// GitTarget materializes the watched resources at exactly one (provider, branch,
// folder); changing where it writes would orphan the old materialization and require
// migrating manifests between repositories/branches/folders. Instead of reconciling
// that move, the destination is fixed: to relocate a GitTarget, delete it and create a
// new one. This keeps the one-owner-per-folder invariant and the initial-snapshot gate
// simple — a successful snapshot can never be silently invalidated by a destination
// change.
//
// +kubebuilder:validation:XValidation:rule="self.providerRef == oldSelf.providerRef",message="spec.providerRef is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.branch == oldSelf.branch",message="spec.branch is immutable; delete and recreate the GitTarget to change its destination"
// +kubebuilder:validation:XValidation:rule="self.path == oldSelf.path",message="spec.path is immutable; delete and recreate the GitTarget to change its destination"
type GitTargetSpec struct {
	// ProviderRef references the GitProvider that backs this target.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	ProviderRef GitProviderReference `json:"providerRef"`

	// Branch to use for this target.
	// Must be one of the allowed branches in the provider.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	Branch string `json:"branch"`

	// Path within the repository to write resources to, relative to the repository
	// root. Required and must be non-empty — there is no default, so a GitTarget can
	// never silently write to the repository root. To deliberately target the
	// repository root, set it to "." (the ArgoCD/Flux convention); an empty string is
	// rejected because it is too easy to leave blank by accident to be a deliberate
	// root choice. Any leading slash (absolute path) and ".." are rejected, and a
	// trailing slash is normalized away.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Encryption defines encryption settings for Secret resource writes.
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`
}

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastReconcileTime is the timestamp of the most recent reconcile attempt.
	// +optional
	LastReconcileTime metav1.Time `json:"lastReconcileTime,omitempty"`

	// LastCommit is the SHA of the last commit processed.
	// +optional
	LastCommit string `json:"lastCommit,omitempty"`

	// LastPushTime is the timestamp of the last successful push.
	// +optional
	LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`

	// Streams is the bounded data-plane roll-up over this GitTarget's tracked types.
	// Counts, never a per-type list, so it stays bounded however many types are watched.
	// +optional
	Streams *GitTargetStreamsStatus `json:"streams,omitempty"`
}

// GitTargetStreamsStatus is a bounded roll-up of the stream readiness state for the
// types this GitTarget tracks.
type GitTargetStreamsStatus struct {
	// Summary is the display-only ready/total ratio.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Total is how many types this target tracks.
	Total int32 `json:"total"`

	// Ready is how many tracked types are Streaming.
	Ready int32 `json:"ready"`

	// Replaying is how many tracked types are still replaying their initial events.
	Replaying int32 `json:"replaying"`

	// Blocked is how many tracked types cannot currently be watched.
	Blocked int32 `json:"blocked"`

	// ObservedTime is when this roll-up was last computed.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reconciling",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciling")].status`
// +kubebuilder:printcolumn:name="Stalled",type=string,JSONPath=`.status.conditions[?(@.type=="Stalled")].status`
// +kubebuilder:printcolumn:name="GitPathAccepted",type=string,JSONPath=`.status.conditions[?(@.type=="GitPathAccepted")].status`
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Stalled")].reason`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Stalled")].message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitTarget is the Schema for the gittargets API.
type GitTarget struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitTarget
	// +required
	Spec GitTargetSpec `json:"spec"`

	// status defines the observed state of GitTarget
	// +optional
	Status GitTargetStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// GitTargetList contains a list of GitTarget.
type GitTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitTarget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitTarget{}, &GitTargetList{})
}
