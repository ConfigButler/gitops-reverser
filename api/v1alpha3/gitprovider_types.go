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

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitProviderSpec defines the desired state of GitProvider.
//
// Only the repository URL is immutable. The URL is the destination identity that every
// referencing GitTarget materializes into; changing it would silently point those
// targets at a different repository and orphan their existing materialization (the same
// reason a GitTarget's destination is immutable). To repoint, delete and recreate the
// GitProvider. Everything else here is operational and deliberately stays mutable —
// notably allowedBranches (widening or narrowing the writable set is a normal change
// that must not require tearing down every GitTarget), plus auth, push tuning, and
// commit identity/signing.
//
// +kubebuilder:validation:XValidation:rule="self.url == oldSelf.url",message="spec.url is immutable; delete and recreate the GitProvider to point at a different repository"
type GitProviderSpec struct {
	// URL of the repository (HTTP/SSH).
	// Immutable: delete and recreate the GitProvider to point at a different repository.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// SecretRef for authentication credentials (may be nil for public repos)
	SecretRef *LocalSecretReference `json:"secretRef,omitempty"`

	// KnownHostsRef optionally points at a namespace-local ConfigMap or Secret holding SSH
	// known_hosts, so host trust can be centralized across GitProviders on the same host instead
	// of repeated in every credentials Secret. It is used only for SSH and ignored for HTTP auth.
	// Host keys are resolved in priority order: the credentials Secret's own known_hosts, then this
	// ref, then the install-level default known-hosts ConfigMap; if none yields valid keys, SSH
	// fails closed.
	// +optional
	KnownHostsRef *KnownHostsReference `json:"knownHostsRef,omitempty"`

	// AllowedBranches restricts which branches can be written to.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	AllowedBranches []string `json:"allowedBranches"`

	// Push controls how events are coalesced into commits before pushing.
	// +optional
	Push *PushStrategy `json:"push,omitempty"`

	// Commit configures commit identity, message formatting, and signing behavior.
	// +optional
	Commit *CommitSpec `json:"commit,omitempty"`
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

// KnownHostsReference points at a namespace-local ConfigMap or Secret that holds SSH known_hosts
// host-trust material. The data is read from the "known_hosts" key, falling back to
// "ssh_known_hosts" (the key Argo CD's argocd-ssh-known-hosts-cm ConfigMap uses, for host keys
// copied out of it).
type KnownHostsReference struct {
	// Kind of the referent: ConfigMap (default) or Secret.
	// +optional
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	// +kubebuilder:default=ConfigMap
	Kind string `json:"kind,omitempty"`

	// Name of the ConfigMap or Secret.
	// +required
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
	// +optional
	SecretRef LocalSecretReference `json:"secretRef,omitempty"`

	// Age configures age-specific encryption behavior for SOPS.
	// +optional
	Age *AgeEncryptionSpec `json:"age,omitempty"`
}

// AgeEncryptionSpec configures age recipient resolution behavior.
type AgeEncryptionSpec struct {
	// Enabled toggles age-based recipient resolution and bootstrap behavior.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Recipients defines how recipients are resolved.
	// +optional
	Recipients AgeRecipientsSpec `json:"recipients,omitempty"`
}

// AgeRecipientsSpec defines age recipient source and key generation behavior.
type AgeRecipientsSpec struct {
	// PublicKeys is a static list of age recipients (age1...).
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	PublicKeys []string `json:"publicKeys,omitempty"`

	// ExtractFromSecret derives recipients from all *.agekey entries in encryption.secretRef.
	// +optional
	// +kubebuilder:default=false
	ExtractFromSecret bool `json:"extractFromSecret,omitempty"`

	// GenerateWhenMissing creates a date-named *.agekey entry in encryption.secretRef when no *.agekey exists.
	// +optional
	// +kubebuilder:default=false
	GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}

// GitProviderStatus defines the observed state of GitProvider.
type GitProviderStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report repository validation and commit configuration readiness:
	// the Ready summary plus the kstatus Reconciling/Stalled pair.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SigningPublicKey is the operator's SSH signing public key in authorized_keys format.
	// Register this as a signing key on your git platform.
	// Only populated when commit.signing is configured and a signing key is available.
	// +optional
	SigningPublicKey string `json:"signingPublicKey,omitempty"`
}

// CommitSpec configures how gitops-reverser creates commits for a GitProvider.
type CommitSpec struct {
	// Committer configures the operator identity written as the commit committer.
	// When signing is enabled, Email must be a verified address on the account
	// that owns the signing key.
	// +optional
	Committer *CommitterSpec `json:"committer,omitempty"`

	// Message configures commit message formatting.
	// +optional
	Message *CommitMessageSpec `json:"message,omitempty"`

	// Signing configures commit signing.
	// +optional
	Signing *CommitSigningSpec `json:"signing,omitempty"`
}

// CommitterSpec configures the bot identity used as the commit committer.
type CommitterSpec struct {
	// Name is the git committer name.
	// +optional
	// +kubebuilder:default="GitOps Reverser"
	Name string `json:"name,omitempty"`

	// Email is the git committer email.
	// +optional
	// +kubebuilder:default="noreply@configbutler.ai"
	Email string `json:"email,omitempty"`
}

// CommitMessageSpec configures commit message formatting.
type CommitMessageSpec struct {
	// EventTemplate is a Go text/template string for per-event commit messages
	// (used when commitWindow is "0s"; one event per commit).
	// Available variables: Operation, Group, Version, Resource, Namespace, Name,
	// APIVersion, Username, GitTarget.
	// +optional
	EventTemplate string `json:"eventTemplate,omitempty"`

	// ReconcileTemplate is a Go text/template string for reconcile commit messages
	// (the mark-and-sweep reconcile path; one commit per synced type).
	// Available variables: Count, GitTarget, Group, Version, Resource, APIVersion, Revision.
	// Group/Version/Resource/APIVersion name the synced type for a per-type reconcile and
	// Revision is the cluster resourceVersion the reconcile was pinned to; both are empty
	// for a whole-target reconcile or a pure sweep, so a template referencing them must
	// render cleanly when they are absent (the default guards them with {{if}}).
	// +optional
	ReconcileTemplate string `json:"reconcileTemplate,omitempty"`

	// GroupTemplate is a Go text/template string for grouped commit messages
	// (the commit-window path; one commit per (author, gitTarget) group
	// produced by the batching pipeline).
	// Available variables: Author, GitTarget, Count, Operations (map of
	// CREATE/UPDATE/DELETE counts), Resources (slice of {Group, Version,
	// Resource, Namespace, Name}).
	// +optional
	GroupTemplate string `json:"groupTemplate,omitempty"`
}

// CommitSigningSpec configures commit signing.
type CommitSigningSpec struct {
	// SecretRef references the Secret containing the signing key material.
	// Expected keys will be defined by the signing implementation.
	SecretRef LocalSecretReference `json:"secretRef"`

	// GenerateWhenMissing causes the operator to generate signing key material
	// in the referenced Secret when it is missing.
	// +optional
	// +kubebuilder:default=false
	GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
