// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitProviderSpec defines the desired state of GitProvider.
//
// Only the repository URL is immutable. The URL is the destination identity
// that every referencing GitTarget materializes into; changing it would
// silently point those targets at a different repository and orphan their
// existing materialization (the same reason a GitTarget's destination is
// immutable). To repoint, delete and recreate the GitProvider. Everything else
// here is operational and deliberately stays mutable — notably
// allowedGitBranches (widening or narrowing who may claim which branches is a
// normal change that must not require tearing down every GitBranch), plus auth
// and commit identity/signing.
//
// +kubebuilder:validation:XValidation:rule="self.url == oldSelf.url",message="spec.url is immutable; delete and recreate the GitProvider to point at a different repository"
type GitProviderSpec struct {
	// URL is the repository's URL, over HTTPS or SSH. Immutable: delete and
	// recreate the GitProvider to change it.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// An empty name is refused rather than read as anonymous access: a typo there would otherwise
	// downgrade a private repository to anonymous silently, and surface as an authentication error
	// against the remote rather than as a mistake in this object.

	// SecretRef names the Secret, in this namespace, that holds the credentials
	// for the repository. Omitted means anonymous access.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.secretRef.name must not be empty; omit secretRef entirely for anonymous access"
	SecretRef *meta.LocalObjectReference `json:"secretRef,omitempty"`

	// KnownHostsRef names a ConfigMap or Secret, in this namespace, holding SSH
	// known_hosts, so several GitProviders on one host can share them. SSH only.
	//
	// Host keys come from the first of these that has any:
	//
	// 1. the known_hosts key of the credentials Secret, spec.secretRef;
	// 2. this reference;
	// 3. the installation's default.
	//
	// Without host keys, SSH fails unless the installation sets
	// --insecure-allow-missing-known-hosts.
	// +optional
	KnownHostsRef *KnownHostsReference `json:"knownHostsRef,omitempty"`

	// AllowedGitBranches says which GitBranches may claim a branch of this
	// repository through this GitProvider: a grant must admit the GitBranch's
	// namespace, and one of its branchPatterns must match the GitBranch's
	// spec.branch. Grants combine by union. Unlike the grant lists of GitBranch
	// and KubeProvider it has no default: omitted or empty, no GitBranch may claim
	// through it, including in this namespace.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	AllowedGitBranches []GitProviderGitBranchGrant `json:"allowedGitBranches,omitempty"`

	// Commit sets the committer identity and commit signing. Commit messages are
	// set on the GitTarget, in spec.commit.message.
	// +optional
	Commit *CommitSpec `json:"commit,omitempty"`
}

// KnownHostsReference points at a namespace-local ConfigMap or Secret that
// holds SSH known_hosts host-trust material. The data is read from the
// "known_hosts" key, falling back to "ssh_known_hosts" (the key Argo CD's
// argocd-ssh-known-hosts-cm ConfigMap uses, for host keys copied out of it).
type KnownHostsReference struct {
	// Kind is "ConfigMap" or "Secret". Defaults to "ConfigMap".
	// +optional
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	// +kubebuilder:default=ConfigMap
	Kind string `json:"kind,omitempty"`

	// Name is the name of the ConfigMap or Secret. Its "known_hosts" key is read,
	// or "ssh_known_hosts" (the key Argo CD's argocd-ssh-known-hosts-cm uses) when
	// that is absent.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// EncryptionSpec configures Secret encryption behavior for git writes.
type EncryptionSpec struct {
	// Provider is the encryption tool. Only "sops" is supported. Defaults to
	// "sops".
	// +kubebuilder:default=sops
	// +kubebuilder:validation:Enum=sops
	Provider string `json:"provider"`

	// SecretRef names the Secret, in this namespace, that holds the provider's
	// keys.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="encryption.secretRef.name must not be empty"
	// +optional
	SecretRef meta.LocalObjectReference `json:"secretRef,omitempty"`

	// Age configures age recipients for SOPS.
	// +optional
	Age *AgeEncryptionSpec `json:"age,omitempty"`
}

// AgeEncryptionSpec configures age recipient resolution behavior.
type AgeEncryptionSpec struct {
	// Enabled turns on age encryption: resolving recipients, and creating a key
	// when asked. Defaults to false.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Recipients says where the age recipients come from.
	// +optional
	Recipients AgeRecipientsSpec `json:"recipients,omitempty"`
}

// AgeRecipientsSpec defines age recipient source and key generation behavior.
type AgeRecipientsSpec struct {
	// PublicKeys is a fixed list of age recipients ("age1...").
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	PublicKeys []string `json:"publicKeys,omitempty"`

	// ExtractFromSecret adds the recipient of every *.agekey entry in
	// encryption.secretRef. Defaults to false.
	// +optional
	// +kubebuilder:default=false
	ExtractFromSecret bool `json:"extractFromSecret,omitempty"`

	// GenerateWhenMissing creates a date-named *.agekey entry in
	// encryption.secretRef when it has none. Defaults to false.
	// +optional
	// +kubebuilder:default=false
	GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}

// GitProviderStatus defines the observed state of GitProvider.
type GitProviderStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this GitProvider's state: whether
	// the repository answers with its credential, and whether commit signing is
	// ready. Ready, Reconciling and Stalled follow kstatus.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// SigningPublicKey is the SSH public key commits are signed with, in
	// authorized_keys format. Register it as a signing key with your Git host. Set
	// only when spec.commit.signing is configured and its key is available.
	// +optional
	SigningPublicKey string `json:"signingPublicKey,omitempty"`
	// The check runs on the steady reconcile interval, so a provider's resourceVersion moves at
	// that cadence. Providers are few and the check was already running; this adds no request.

	// LastVerifiedAt is the time of the last successful connectivity check
	// against the repository, using this GitProvider's credentials, or
	// anonymous access when spec.secretRef is omitted. It is never cleared, and
	// a failure leaves it unchanged, so it does not show when a failure began.
	// status.conditions gives the reason the GitProvider is not ready.
	// +optional
	LastVerifiedAt *metav1.Time `json:"lastVerifiedAt,omitempty"`
}

// CommitSpec configures the commit identity and signing a GitProvider uses.
// Message formatting lives on the GitTarget (spec.commit.message), because it
// describes the folder rather than the connection.
type CommitSpec struct {
	// Committer is the identity written as each commit's committer. With signing
	// enabled, its email must be a verified address of the account that owns the
	// signing key.
	// +optional
	Committer *CommitterSpec `json:"committer,omitempty"`

	// Signing configures SSH commit signing.
	// +optional
	Signing *CommitSigningSpec `json:"signing,omitempty"`
}

// CommitterSpec configures the bot identity used as the commit committer.
type CommitterSpec struct {
	// Name is the committer name. Defaults to "GitOps Reverser".
	// +optional
	// +kubebuilder:default="GitOps Reverser"
	Name string `json:"name,omitempty"`

	// Email is the committer email. Defaults to "noreply@configbutler.ai".
	// +optional
	// +kubebuilder:default="noreply@configbutler.ai"
	Email string `json:"email,omitempty"`
}

// CommitMessageSpec configures commit message formatting, on
// GitTarget.spec.commit.message.
type CommitMessageSpec struct {
	// LiveTemplate is the Go template for the message of a commit made from
	// live edits, whatever its size. Omitted, a built-in template is used.
	//
	// The template receives:
	//
	// - Author: the username, which may be empty.
	// - GitTarget: the GitTarget's name.
	// - Count: the number of edits after coalescing, before comparing with Git.
	// - Operations: the number of edits per operation.
	// - Resources: one entry per resource in the commit.
	// - LabelValues "key": the distinct values of one label across Resources.
	// - LabelValue "key": the value of one label that every resource shares,
	//   or empty when they differ.
	//
	// Each resource has Operation, Group, Version, Resource, Kind, Namespace,
	// Name, APIVersion, ResourceVersion, Generation and Labels. Namespace is
	// "_cluster" for a cluster-scoped resource. Read one label with
	// {{.Label "key"}}; {{.Labels.key}} fails the commit for a resource without
	// that label. Kind and Labels are empty for a deletion, ResourceVersion
	// can be empty, and Generation is 0 for a kind without a spec, so guard
	// them with {{with}}.
	//
	// A SaveRequest's message replaces this template without changing the
	// author; spec.commit.message.requestTemplate can wrap that message.
	// +optional
	LiveTemplate string `json:"liveTemplate,omitempty"`

	// Rationale, kept out of the doc comment (and so out of the CRD schema) by the blank line
	// below: a SaveRequest is created by whoever holds create on it in a namespace, so its
	// author supplies the message but must not set commit-message policy. Framing the message
	// rather than parsing it keeps that split — the operator owns the wording, the requester owns
	// the words, and nothing a requester writes is ever executed. Rejecting a template that never
	// renders RequestMessage is what keeps the split honest in the other direction: silently
	// discarding a requester's stated reason would be worse than having no template at all.

	// RequestTemplate is the Go template that frames a SaveRequest's message. It
	// is used only for a commit whose SaveRequest supplied a message, and gets
	// liveTemplate's fields plus RequestMessage, the message as written; template
	// syntax inside the message stays literal. Omitted, the message is committed
	// as written. A template that never renders RequestMessage is rejected, and
	// one that fails at commit time commits the message as written.
	// +optional
	RequestTemplate string `json:"requestTemplate,omitempty"`

	// ReconcileTemplate is the Go template for the message of a snapshot or resync
	// commit. Fields: Count, GitTarget, Group, Version, Resource, APIVersion,
	// Namespace and ResourceVersion (the list's, not one object's). The type and
	// namespace fields are empty for a snapshot of the whole folder, and
	// ResourceVersion can be empty, so guard them. A template naming Revision,
	// renamed to ResourceVersion, is rejected.
	// +optional
	ReconcileTemplate string `json:"reconcileTemplate,omitempty"`
}

// CommitSigningSpec configures commit signing.
type CommitSigningSpec struct {
	// SecretRef names the Secret, in this namespace, that holds the SSH signing
	// key.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="commit.signing.secretRef.name must not be empty"
	SecretRef meta.LocalObjectReference `json:"secretRef"`

	// GenerateWhenMissing creates a signing key in the Secret when it holds none.
	// Defaults to false.
	// +optional
	// +kubebuilder:default=false
	GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Verified",type=date,JSONPath=`.status.lastVerifiedAt`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitProvider is a connection to one Git repository: its URL, the credential
// that reaches it, the committer identity and commit signing. GitBranches
// claim branches of the repository through it, as its spec.allowedGitBranches
// allow.
type GitProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitProvider.
	// +required
	Spec GitProviderSpec `json:"spec"`

	// status defines the observed state of GitProvider.
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
