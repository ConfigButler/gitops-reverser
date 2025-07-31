/*
Copyright 2025.

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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// GitRepoConfigSpec defines the desired state of GitRepoConfig
type GitRepoConfigSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of GitRepoConfig. Edit gitrepoconfig_types.go to remove/update
	// +optional
	Foo *string `json:"foo,omitempty"`
}

// GitRepoConfigStatus defines the observed state of GitRepoConfig.
type GitRepoConfigStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// GitRepoConfig is the Schema for the gitrepoconfigs API
type GitRepoConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitRepoConfig
	// +required
	Spec GitRepoConfigSpec `json:"spec"`

	// status defines the observed state of GitRepoConfig
	// +optional
	Status GitRepoConfigStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// GitRepoConfigList contains a list of GitRepoConfig
type GitRepoConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitRepoConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitRepoConfig{}, &GitRepoConfigList{})
}
