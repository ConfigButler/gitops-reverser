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
// A GitTarget materializes the watched resources at exactly one (provider, branch,
// folder). branch and path are mutable: changing either is a supported "retarget", in
// which the controller tears the old materialization down and rebuilds the folder from a
// fresh full snapshot at the new destination. The old folder's files are never deleted —
// deleting from Git is the one irreversible thing this operator can do, and a destination
// change is the moment an operator is least sure of what they meant. See
// docs/design/multi-tenant/gittarget-retarget.md.
//
// providerRef stays immutable: pointing at a different repository is not a move, it is a
// different object. There is nothing to migrate and nothing to observe.
//
// The initial-snapshot gate is preserved by making the destination a thing status
// OBSERVES rather than a thing the spec cannot change: a successful snapshot is valid for
// the destination recorded in status.observedDestination. When spec and
// status.observedDestination disagree, the snapshot is by definition stale, and the
// GitTarget reports Retargeting=True until the new folder is built.
//
// +kubebuilder:validation:XValidation:rule="self.providerRef == oldSelf.providerRef",message="spec.providerRef is immutable; delete and recreate the GitTarget to change its repository (spec.branch and spec.path are mutable — the controller retargets, see docs/design/multi-tenant/gittarget-retarget.md)"
type GitTargetSpec struct {
	// ProviderRef references the GitProvider that backs this target.
	// Immutable: delete and recreate the GitTarget to write to a different repository.
	// +required
	ProviderRef GitProviderReference `json:"providerRef"`

	// Branch to use for this target.
	// Must be one of the allowed branches in the provider.
	//
	// Mutable: changing it retargets the GitTarget. The old branch's folder is left
	// behind as ordinary, unmanaged Git content; the new branch's folder is built from a
	// fresh full snapshot. status.observedDestination names the destination the current
	// materialization belongs to.
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
	//
	// Mutable: changing it retargets the GitTarget, exactly as changing branch does. The
	// new path must not overlap another GitTarget on the same provider and branch; a
	// retarget onto a conflicting path is refused and the target keeps serving its
	// current destination.
	// +required
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// SourceCluster names the cluster this GitTarget mirrors FROM. Omitted means the
	// cluster the operator runs in, which is the single-cluster default and needs no
	// configuration.
	//
	// Setting it separates the two jobs one kubeconfig used to serve: the operator reads
	// its own CRs and Git credentials from the cluster it runs in (the config plane) and
	// watches resources on the cluster named here. Nothing but the watched resources then
	// has to live on the watched cluster — no Secret, no configbutler.ai CRDs at all —
	// and one operator can mirror many clusters, because each GitTarget carries its own
	// source. It is deliberately shaped like Flux's `Kustomization.spec.kubeConfig`.
	//
	// It sits on GitTarget rather than on WatchRule because a GitTarget already owns
	// exactly one materialization. Adding the source cluster makes that one
	// (cluster, provider, branch, folder) — still one owner, one folder, one desired
	// state. On WatchRule, two rules could name different clusters for one folder and the
	// mark-and-sweep would alternately delete each cluster's objects.
	//
	// WatchRule keeps its meaning: it watches the namespace it lives in, resolved on the
	// source cluster. A WatchRule in config-plane namespace "team-a" watches namespace
	// "team-a" on the source cluster. A ClusterWatchRule watches the whole source cluster.
	//
	// Mutable: changing it retargets the GitTarget, exactly as changing branch or path
	// does — the folder's contents would otherwise mean something different. Rotating the
	// Secret's CONTENTS is transparent and is not a retarget.
	// +optional
	SourceCluster *SourceClusterSpec `json:"sourceCluster,omitempty"`

	// Encryption defines encryption settings for Secret resource writes.
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`

	// Placement declares where NEW resources are written. It has no effect on a
	// resource that already has a document in Git — that document is always
	// updated in place at its existing location, wherever that is. Mutable: a
	// change only affects resources created after the change.
	// +optional
	Placement *GitTargetPlacementSpec `json:"placement,omitempty"`
}

// SourceClusterSpec names a remote cluster to mirror from, by the Secret holding its
// kubeconfig. The Secret is read from the GitTarget's own namespace — the config plane —
// so the credential for a cluster never has to live on that cluster.
type SourceClusterSpec struct {
	// KubeConfigSecretRef names the Secret holding the kubeconfig for the source cluster.
	// +required
	KubeConfigSecretRef SecretKeyReference `json:"kubeConfigSecretRef"`
}

// SecretKeyReference points at one key inside a Secret in the referring object's namespace.
type SecretKeyReference struct {
	// Name of the Secret, in the same namespace as the referring object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within the Secret's data holding the kubeconfig. Defaults to "value.yaml",
	// Flux's convention, so a Secret produced for a Flux Kustomization works unchanged.
	// +optional
	// +kubebuilder:default="value.yaml"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`
}

// GitTargetPlacementSpec declares where NEW resources are written when no document
// for their identity exists yet in Git — one exact-type map plus a fallback
// default template (Option B2 of
// docs/design/manifest/version2/gittarget-new-file-placement-rules.md). There is
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

// GitTargetDestination is one materialization site: the source cluster whose resources a
// GitTarget mirrors, and the branch and folder they live at.
type GitTargetDestination struct {
	// Branch the materialization lives on.
	// +required
	Branch string `json:"branch"`

	// Path the materialization lives under, relative to the repository root.
	// +required
	Path string `json:"path"`

	// SourceCluster names the kubeconfig Secret whose cluster this materialization was
	// mirrored from. Empty means the cluster the operator runs in.
	// +optional
	SourceCluster string `json:"sourceCluster,omitempty"`
}

// SourceClusterID identifies the cluster a GitTarget mirrors from, as the watch data plane
// keys its per-cluster clients, catalogs, and type registries. The empty string is the
// cluster the operator runs in.
//
// The Secret's key is part of the identity: two GitTargets naming the same Secret under
// different keys are pointed at different kubeconfigs, and so at different clusters.
func (g *GitTarget) SourceClusterID() string {
	if g.Spec.SourceCluster == nil {
		return ""
	}
	ref := g.Spec.SourceCluster.KubeConfigSecretRef
	key := ref.Key
	if key == "" {
		key = DefaultKubeConfigSecretKey
	}
	return g.Namespace + "/" + ref.Name + "/" + key
}

// DefaultKubeConfigSecretKey is the Secret data key a source-cluster kubeconfig is read
// from when none is given. It matches Flux's convention.
const DefaultKubeConfigSecretKey = "value.yaml"

// GitTargetStatus defines the observed state of GitTarget.
type GitTargetStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedDestination is the destination the current materialization belongs to. It
	// is written only once a snapshot has actually landed there and the path was
	// accepted, so it is the answer to "which folder is this GitTarget's content in?" —
	// which is not always what spec says.
	//
	// Absent means nothing has been materialized yet. When it disagrees with spec, the
	// GitTarget is retargeting and Retargeting=True; the folder it names is the one being
	// abandoned, so an operator can `git rm` it deliberately once the move settles.
	// +optional
	ObservedDestination *GitTargetDestination `json:"observedDestination,omitempty"`

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
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="Retargeting",type=string,JSONPath=`.status.conditions[?(@.type=="Retargeting")].status`,priority=1
// +kubebuilder:printcolumn:name="ObservedPath",type=string,JSONPath=`.status.observedDestination.path`,priority=1
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
