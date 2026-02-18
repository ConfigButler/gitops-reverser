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

// GitProviderSpec defines the desired state of GitProvider.
type GitProviderSpec struct {
	// URL of the repository (HTTP/SSH)
	URL string `json:"url"`

	// SecretRef for authentication credentials (may be nil for public repos)
	SecretRef *LocalSecretReference `json:"secretRef,omitempty"`

	// AllowedBranches restricts which branches can be written to.
	// +required
	AllowedBranches []string `json:"allowedBranches"`

	// Push defines the strategy for pushing commits (batching).
	// +optional
	Push *PushStrategy `json:"push,omitempty"`
}

// LocalSecretReference is a typed reference to a Secret in the same namespace.
type LocalSecretReference struct {
	// Group of the referent.
	// +kubebuilder:default=""
	// +optional
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// +kubebuilder:validation:Enum=Secret
	// +kubebuilder:default=Secret
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// EncryptionSpec configures Secret encryption behavior for git writes.
type EncryptionSpec struct {
	// Provider selects the encryption provider.
	// +kubebuilder:default=sops
	// +kubebuilder:validation:Enum=sops
	Provider string `json:"provider"`

	// SecretRef references namespace-local Secret data used by the encryption provider.
	SecretRef LocalSecretReference `json:"secretRef"`
}

// GitProviderStatus defines the observed state of GitProvider.
type GitProviderStatus struct {
	// conditions represent the current state of the GitProvider resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// GitProvider is the Schema for the gitproviders API.
type GitProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitProvider
	// +required
	Spec GitProviderSpec `json:"spec"`

	// status defines the observed state of GitProvider
	// +optional
	Status GitProviderStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// GitProviderList contains a list of GitProvider.
type GitProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitProvider{}, &GitProviderList{})
}
