/*
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

// LocalObjectReference contains enough information to let you locate a
// referenced object inside the same namespace.
type LocalObjectReference struct {
	// Name is the name of the referent.
	// +required
	Name string `json:"name"`
}

// GitRepoConfigSpec defines the desired state of GitRepoConfig.
// +kubebuilder:validation:XValidation:rule="!has(self.accessPolicy) || !has(self.accessPolicy.namespacedRules) || !has(self.accessPolicy.namespacedRules.namespaceSelector) || self.accessPolicy.namespacedRules.mode == 'FromSelector'",message="namespaceSelector can only be set when mode is 'FromSelector'"
// +kubebuilder:validation:XValidation:rule="!has(self.accessPolicy) || !has(self.accessPolicy.namespacedRules) || self.accessPolicy.namespacedRules.mode != 'FromSelector' || has(self.accessPolicy.namespacedRules.namespaceSelector)",message="namespaceSelector is required when mode is 'FromSelector'"
type GitRepoConfigSpec struct {
	// RepoURL is the URL of the Git repository to commit to.
	// +required
	RepoURL string `json:"repoUrl"`

	// Branch is the Git branch to commit to.
	// +required
	Branch string `json:"branch"`

	// SecretRef specifies the Secret containing Git credentials.
	// For HTTPS repositories the Secret must contain 'username' and 'password'
	// fields for basic auth or 'bearerToken' field for token auth.
	// For SSH repositories the Secret must contain 'identity'
	// and 'known_hosts' fields.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// Push defines the strategy for pushing commits to the remote.
	// +optional
	Push *PushStrategy `json:"push,omitempty"`

	// AccessPolicy controls which WatchRules can reference this GitRepoConfig.
	// If not specified, defaults to SameNamespace mode (most restrictive).
	// +optional
	AccessPolicy *AccessPolicy `json:"accessPolicy,omitempty"`
}

// PushStrategy defines the rules for when to push commits.
type PushStrategy struct {
	// Interval is the maximum time to wait before pushing queued commits.
	// Defaults to "1m".
	// +optional
	Interval *string `json:"interval,omitempty"`

	// MaxCommits is the maximum number of commits to queue before pushing.
	// Defaults to 20.
	// +optional
	MaxCommits *int `json:"maxCommits,omitempty"`
}

// GitRepoConfigStatus defines the observed state of GitRepoConfig.
type GitRepoConfigStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedGeneration is the last generation that was successfully validated
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// GitRepoConfig is the Schema for the gitrepoconfigs API.
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

// GitRepoConfigList contains a list of GitRepoConfig.
type GitRepoConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitRepoConfig `json:"items"`
}

// AccessPolicy defines access control rules for GitRepoConfig.
type AccessPolicy struct {
	// NamespacedRules controls access from namespace-scoped WatchRules.
	// If not specified, defaults to SameNamespace mode.
	// +optional
	NamespacedRules *NamespacedRulesPolicy `json:"namespacedRules,omitempty"`

	// AllowClusterRules controls whether cluster-scoped ClusterWatchRules
	// can reference this GitRepoConfig.
	// Defaults to false for security (explicit opt-in required).
	// +optional
	// +kubebuilder:default=false
	AllowClusterRules bool `json:"allowClusterRules,omitempty"`
}

// NamespacedRulesPolicy defines which namespaces can access this GitRepoConfig.
type NamespacedRulesPolicy struct {
	// Mode determines the access control mode.
	// - SameNamespace (default): Only WatchRules in the same namespace
	// - AllNamespaces: WatchRules from any namespace can access
	// - FromSelector: Only namespaces matching the selector
	// +optional
	// +kubebuilder:default=SameNamespace
	// +kubebuilder:validation:Enum=SameNamespace;AllNamespaces;FromSelector
	Mode AccessPolicyMode `json:"mode,omitempty"`

	// NamespaceSelector selects which namespaces can access this GitRepoConfig.
	// ONLY evaluated when Mode is "FromSelector".
	// MUST be nil when Mode is NOT "FromSelector".
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// AccessPolicyMode defines the access control mode.
// +kubebuilder:validation:Enum=SameNamespace;AllNamespaces;FromSelector
type AccessPolicyMode string

const (
	// AccessPolicyModeSameNamespace allows only same namespace access (default, most secure).
	AccessPolicyModeSameNamespace AccessPolicyMode = "SameNamespace"

	// AccessPolicyModeAllNamespaces allows access from any namespace.
	AccessPolicyModeAllNamespaces AccessPolicyMode = "AllNamespaces"

	// AccessPolicyModeFromSelector allows access from matching namespaces only.
	AccessPolicyModeFromSelector AccessPolicyMode = "FromSelector"
)

func init() {
	SchemeBuilder.Register(&GitRepoConfig{}, &GitRepoConfigList{})
}
