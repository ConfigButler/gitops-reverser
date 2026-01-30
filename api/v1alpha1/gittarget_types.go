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

// GitProviderReference references the GitProvider or Flux GitRepository.
type GitProviderReference struct {
	// API Group of the referent.
	// +kubebuilder:enum=configbutler.ai,source.toolkit.fluxcd.io
	// +kubebuilder:default=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// NOTE: Support for reading from Flux GitRepository is not yet implemented!
	// +optional
	// +kubebuilder:enum=GitProvider,GitRepository
	// +kubebuilder:default=GitProvider
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	Name string `json:"name"`
}

// GitTargetSpec defines the desired state of GitTarget.
type GitTargetSpec struct {
	// ProviderRef references the GitProvider or Flux GitRepository.
	// +required
	ProviderRef GitProviderReference `json:"providerRef"`

	// Branch to use for this target.
	// Must be one of the allowed branches in the provider.
	// +required
	Branch string `json:"branch"`

	// Path within the repository to write resources to.
	// +optional
	Path string `json:"path,omitempty"`
}

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastCommit is the SHA of the last commit processed.
	// +optional
	LastCommit string `json:"lastCommit,omitempty"`

	// LastPushTime is the timestamp of the last successful push.
	// +optional
	LastPushTime *metav1.Time `json:"lastPushTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
