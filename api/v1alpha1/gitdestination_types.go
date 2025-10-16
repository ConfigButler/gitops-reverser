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

// GitDestinationSpec defines the desired state of GitDestination.
//
// GitDestination binds a repository reference (GitRepoConfig) with a target
// branch and a baseFolder where objects owned by this destination will be
// written in the Git repository.
//
// Alpha constraints:
//   - No exclusive mode or ownership semantics are provided
//   - baseFolder must be a POSIX-like relative path (no leading slash, no "..")
type GitDestinationSpec struct {
	// RepoRef references the GitRepoConfig to use (namespaced).
	// +required
	RepoRef NamespacedName `json:"repoRef"`

	// Branch is the Git branch to write to for this destination.
	// In MVP, no allowlist is enforced here; controllers may validate existence later.
	// +required
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// BaseFolder is the relative POSIX-like path under which files will be written.
	// It must not start with "/" and must not contain ".." path traversal segments.
	// Examples: "clusters/prod", "audit/cluster-a"
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=^([A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+$
	// Note: Additional path traversal checks are enforced at runtime; CEL rules removed for compatibility.
	BaseFolder string `json:"baseFolder"`
}

// GitDestinationStatus defines the observed state of GitDestination.
//
// Controllers set Ready condition to signal that the destination is valid:
//   - GitRepoConfig exists (and optionally Ready), branch is syntactically valid,
//     and baseFolder passed validation. No remote connectivity checks are required here.
type GitDestinationStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the last generation that was reconciled
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repoRef.name`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="BaseFolder",type=string,JSONPath=`.spec.baseFolder`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitDestination binds repo+branch+baseFolder for a write subtree in Git.
type GitDestination struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of GitDestination
	// +required
	Spec GitDestinationSpec `json:"spec"`

	// status defines the observed state of GitDestination
	// +optional
	Status GitDestinationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GitDestinationList contains a list of GitDestination.
type GitDestinationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitDestination `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitDestination{}, &GitDestinationList{})
}
