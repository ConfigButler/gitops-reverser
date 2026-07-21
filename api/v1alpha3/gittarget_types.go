// SPDX-License-Identifier: Apache-2.0

package v1alpha3

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
	// +kubebuilder:validation:Enum=configbutler.ai
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
	// +kubebuilder:validation:MinLength=1
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
//
// spec.clusterProviderRef names the SOURCE cluster a GitTarget mirrors FROM (see its field doc). It
// is immutable — a folder's source cluster is part of what the folder means, like
// providerRef/branch/path above — and defaults to a ClusterProvider named "default", so it is
// always populated (never nil) and always jumpable.
// +kubebuilder:validation:XValidation:rule="self.clusterProviderRef == oldSelf.clusterProviderRef",message="spec.clusterProviderRef is immutable; delete and recreate the GitTarget to change the cluster it mirrors"
type GitTargetSpec struct {
	// ProviderRef references the GitProvider that backs this target.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	ProviderRef GitProviderReference `json:"providerRef"`

	// Branch to use for this target.
	// Must be one of the allowed branches in the provider.
	// Immutable: delete and recreate the GitTarget to change its destination.
	// +required
	// +kubebuilder:validation:MinLength=1
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

	// Placement declares where NEW resources are written. It has no effect on a
	// resource that already has a document in Git — that document is always
	// updated in place at its existing location, wherever that is. Mutable: a
	// change only affects resources created after the change.
	// +optional
	Placement *GitTargetPlacementSpec `json:"placement,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// It defaults to a concrete {name: "default"} rather than an implicit nil so a target that omits
	// it persists with a ref a reader can jump to. The operator never creates that provider: a
	// GitTarget naming one that does not exist is held unready rather than silently defaulting to
	// in-cluster access.

	// ClusterProviderRef names the SOURCE cluster this GitTarget mirrors FROM, by referencing a
	// cluster-scoped ClusterProvider by name. That ClusterProvider owns the cluster's connectivity
	// credential, namespace-access authorization, and author-attribution mode. The default provider
	// name is "default" and must exist; it may be in-cluster or remote.
	// Immutable: a folder's source cluster is part of what the folder means; delete and recreate.
	// +kubebuilder:default={name: "default"}
	// +optional
	ClusterProviderRef *ClusterProviderReference `json:"clusterProviderRef,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// There is deliberately NO self-namespace exception. An implicit carve-out would mean the field
	// does not actually bound what arrives here, so a reader auditing it would be wrong about the
	// target's contents — which is the whole reason the field exists. The resulting authoring
	// footgun (adding a policy for one override silently denies co-resident legacy rules) is
	// mitigated by being LOUD: SourceNamespaceAuthorized=False, Stalled=True, and a message naming
	// the exact fix. `selector: {}` is the replacement for the removed cluster-wide namespaced
	// ClusterWatchRule — declared by the destination owner rather than the rule author, and
	// self-updating as namespaces come and go. The exact-names half stays answerable without any
	// source-cluster Namespace access; that degradation path is deliberate, and it is the half most
	// likely to regress unnoticed.

	// AllowedSourceNamespaces bounds which SOURCE-cluster namespaces may be mirrored INTO this
	// target. It belongs to the DESTINATION, not to any requesting rule: once declared it is
	// exhaustive for every WatchRule that writes here, with no exception for a rule's own namespace.
	//
	// Omitted and empty differ. Omitted declares no policy, and a WatchRule keeps its own namespace;
	// a declared-but-empty policy admits nothing; `selector: {}` admits every source namespace.
	// Selector labels are read in the SOURCE cluster, so evaluating one needs Namespace
	// get/list/watch for that cluster's credential, while exact names need no such access. This is
	// also what a rules[].sourceNamespace of "*" resolves through. Naming any namespace other than
	// the WatchRule's own — including "*" — additionally requires the ClusterProvider to set
	// spec.allowSourceNamespaceOverride. It does NOT bound ClusterWatchRule, whose cluster-scoped
	// objects have no namespace. Full resolution table: docs/configuration.md.
	// +optional
	AllowedSourceNamespaces *NamespaceMatcher `json:"allowedSourceNamespaces,omitempty"`
}

// GitTargetPlacementSpec declares where NEW resources are written when no document
// for their identity exists yet in Git — one exact-type map plus a fallback
// default template (Option B2 of
// docs/spec/gittarget-new-file-placement-rules.md). There is
// deliberately no separate "sensitive" placement block: sensitivity is a
// write-safety classification the controller owns (encrypt the content, keep the
// path identity-complete, never append or co-mingle), not a second placement
// namespace the user has to configure. A user routes Secrets the same way they
// route anything else — by naming their type in ByType. When a resource's type
// has no ByType entry and no Default, placement falls back to following the layout
// already established by sibling resources in the repository, and finally to the
// canonical, versionless {namespaceOrCluster}/{group}/{resource}/{name}.yaml path
// when there is nothing to follow. Because that fallback omits the API version,
// objects that differ only by version share a file; a target that watches several
// versions of the same group/resource and wants them separated must use a
// ByType/Default template that includes {version}.
type GitTargetPlacementSpec struct {
	// ByType maps an exact resource type key ("{group}/{version}/{resource}", e.g.
	// "v1/configmaps", "apps/v1/deployments", or "v1/secrets"; core resources omit
	// the group) to the path template used for a new resource of that type. A path
	// selected for a sensitive resource (Secrets, plus any operator-configured
	// sensitive type) must be identity-complete so it cannot collide two distinct
	// sensitive resources onto one file.
	// +optional
	ByType map[string]string `json:"byType,omitempty"`

	// Default is the path template used for a new resource whose type has no ByType
	// entry. Omitted, it falls through to sibling-layout inference and then the
	// built-in canonical path. A bundling default (one that is not identity-complete,
	// such as "all.yaml") is only valid when a sensitive resource can never reach it
	// — give every sensitive type an explicit identity-complete ByType entry.
	// +optional
	Default string `json:"default,omitempty"`
}

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of an object's state
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastReconcileTime is the timestamp of the most recent reconcile attempt.
	// +optional
	LastReconcileTime metav1.Time `json:"lastReconcileTime,omitempty"`

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
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="GitPathAccepted",type=string,JSONPath=`.status.conditions[?(@.type=="GitPathAccepted")].status`,priority=1
// +kubebuilder:printcolumn:name="RenderMatchesLive",type=string,JSONPath=`.status.conditions[?(@.type=="RenderMatchesLive")].status`,priority=1
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="SourceReachable",type=string,JSONPath=`.status.conditions[?(@.type=="SourceClusterReachable")].reason`,priority=1
// +kubebuilder:printcolumn:name="ProviderReady",type=string,JSONPath=`.status.conditions[?(@.type=="GitProviderReady")].status`,priority=1
// +kubebuilder:printcolumn:name="ClusterProviderReady",type=string,JSONPath=`.status.conditions[?(@.type=="ClusterProviderReady")].status`,priority=1
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Encryption",type=string,JSONPath=`.spec.encryption.provider`,priority=1
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

// SourceCluster is the identity the watch data plane keys a GitTarget's source cluster on: the
// referenced ClusterProvider's NAME. It defaults to "default" when clusterProviderRef is unset —
// so a source-cluster-unaware caller still gets a concrete, non-empty name, and there is no ""
// sentinel. That name is a convention, not a claim about which physical cluster it is. The name is
// the cluster's identity everywhere: the fact-index key, the GVK→GVR registry key, and the
// /audit-webhook/<name> route.
func (g *GitTarget) SourceCluster() string {
	if g.Spec.ClusterProviderRef == nil || g.Spec.ClusterProviderRef.Name == "" {
		return DefaultClusterProviderName
	}
	return g.Spec.ClusterProviderRef.Name
}

// IsLocalSource reports whether this GitTarget references the "default" ClusterProvider, which the
// watch data plane maps to its local cluster context. It is a NAME test, not a claim about the
// physical cluster: a "default" provider may carry a kubeConfig. It only supplies the pre-discovery
// default for SourceClusterReachable, which the watch manager overwrites as soon as it is wired.
func (g *GitTarget) IsLocalSource() bool {
	return g.SourceCluster() == DefaultClusterProviderName
}

// DeclaresSourceNamespacePolicy reports whether this target declares spec.allowedSourceNamespaces
// at all. A declared policy is EXHAUSTIVE — it bounds every WatchRule item writing here, with no
// self-namespace exception — while an absent one leaves a WatchRule its own namespace. Callers
// must branch on this rather than on emptiness: a declared-but-empty policy admits nothing.
func (g *GitTarget) DeclaresSourceNamespacePolicy() bool {
	return g.Spec.AllowedSourceNamespaces.Declared()
}

// AllowsSourceNamespace reports whether a SOURCE-cluster namespace (by name and by the labels it
// carries IN THE SOURCE CLUSTER) may be mirrored into this target, per spec.allowedSourceNamespaces.
//
// It is the source-side twin of ClusterProvider.AllowsNamespace, and both are thin wrappers over
// NamespaceMatcher.Matches so the two policies cannot drift. It answers only the POLICY question:
// the delegation flag, the provider's own admission of this target's namespace, and the
// three-valued "can the labels be read at all" question are the caller's (see internal/authz).
// An undeclared policy admits nothing here — callers apply the legacy rule themselves.
func (g *GitTarget) AllowsSourceNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	return g.Spec.AllowedSourceNamespaces.Matches(nsName, nsLabels)
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
