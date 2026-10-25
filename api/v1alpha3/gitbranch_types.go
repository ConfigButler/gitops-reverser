// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GitBranchSpec defines the desired state of GitBranch.
//
// gitProviderRef, branch and parentBranch are immutable: together they are the repository branch
// this object claims, and a claim that could be repointed would move a branch worker's checkout,
// credential and pending writes to another branch under the GitTargets that reference it. Delete
// and recreate instead. allowedGitTargets is the branch owner's policy and stays mutable.
//
// CEL cannot read metadata.namespace, so the reference is compared as written: spelling out the
// GitBranch's own namespace where it was omitted is refused too.
// +kubebuilder:validation:XValidation:rule="self.gitProviderRef == oldSelf.gitProviderRef",message="spec.gitProviderRef is immutable; delete and recreate the GitBranch to change it"
// +kubebuilder:validation:XValidation:rule="self.branch == oldSelf.branch",message="spec.branch is immutable; delete and recreate the GitBranch to change it"
// +kubebuilder:validation:XValidation:rule="has(self.parentBranch) == has(oldSelf.parentBranch) && (!has(self.parentBranch) || self.parentBranch == oldSelf.parentBranch)",message="spec.parentBranch is immutable; delete and recreate the GitBranch to change it"
type GitBranchSpec struct {
	// GitProviderRef names the GitProvider whose repository and credential write this branch. An
	// omitted namespace means this GitBranch's namespace. The GitProvider's spec.allowedGitBranches
	// must admit this namespace and spec.branch. Immutable: delete and recreate the GitBranch to
	// change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitProviderRef.name must not be empty"
	GitProviderRef meta.NamespacedObjectReference `json:"gitProviderRef"`

	// Branch is the branch on the remote that this GitBranch writes. It must match a branch pattern
	// that the GitProvider's spec.allowedGitBranches give this namespace. Only one GitBranch holds
	// a repository branch: the oldest admitted one whose credential can open a push session. Any
	// other reports Stalled, reason BranchConflict. Immutable: delete and recreate the GitBranch to
	// change it.
	// +required
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// Why "parent" and not base/source/revision: docs/definitions.md reserves those words, and the
	// field is flat because it is one fact. It decides what history a new write branch gets, and a
	// parent changed under a worker holding writes is the one case that needed its own replay, so it
	// is immutable. It can never hold a commit or tag: a pinned starting point would be a sibling.

	// ParentBranch is the branch spec.branch is created from, and compared against while
	// spec.branch does not exist on the remote. Omitted, it is the remote's default branch, or no
	// history at all in an empty repository. When set, it must exist on the remote for spec.branch
	// to be created; otherwise this GitBranch reports ParentBranchNotFound and writes nothing. Once
	// spec.branch exists, the parent branch is no longer followed. It is only read, so
	// spec.allowedGitBranches need not allow it. Immutable, including adding or removing it:
	// delete and recreate the GitBranch to change it.
	// +optional
	// +kubebuilder:validation:MinLength=1
	ParentBranch string `json:"parentBranch,omitempty"`

	// Mutable, unlike the fields above: claiming a branch includes the right to share it.

	// AllowedGitTargets says which GitTargets may write through this GitBranch. Omitted, it is one
	// grant for this namespace (consumerNamespaces: {from: Same}). A written list is the whole
	// policy, so it admits this namespace only if a grant says so, and [] admits nobody. Grants
	// combine by union.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={{consumerNamespaces: {from: Same}}}
	AllowedGitTargets []GitBranchGitTargetGrant `json:"allowedGitTargets"`
}

// GitBranchStatus defines the observed state of GitBranch.
type GitBranchStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this GitBranch's state: whether it holds its
	// repository branch and can write it. Ready, Reconciling and Stalled follow kstatus.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Repository is the GitProvider's repository URL, normalized to host and path with no scheme,
	// user or ".git" suffix. With spec.branch it is what only one GitBranch may hold, so two
	// GitProviders that spell one repository differently still conflict.
	// +optional
	Repository string `json:"repository,omitempty"`

	// LastPushNoticeAt is when a GitReceiver last delivered a push for this branch. It records that
	// the delivery arrived; status.remote says what the remote holds. Absent means no delivery has
	// reached this branch: no GitReceiver is set up or reachable, the delivery names the repository
	// by a URL other than the GitProvider's spec.url, or no GitTarget references this GitBranch. It
	// advances at most once a minute.
	// +optional
	LastPushNoticeAt *metav1.Time `json:"lastPushNoticeAt,omitempty"`

	// Remote is where this branch is on the Git remote, and when that was last checked by a push or
	// a fetch. It is updated at most once a minute. Absent until the first check, and again after
	// the GitProvider is recreated for another repository.
	// +optional
	Remote *GitBranchRemoteStatus `json:"remote,omitempty"`

	// GitTargetCount is how many GitTargets, in any namespace, reference this GitBranch. Each one
	// reports its own folder's health in its own conditions.
	// +optional
	GitTargetCount int32 `json:"gitTargetCount"`
}

// GitBranchRemoteStatus is the answer to "where is my branch, and when did we last prove it".
//
// It is sampled when the GitBranch reconciles, from what its branch worker last proved about the
// branch, and written at most once a minute. The first observation, and a correction after the
// repository changed under the branch, are written at once.
type GitBranchRemoteStatus struct {
	// Commit is the commit hash the branch is at on the remote. Empty means the branch is not on
	// the remote yet, which is not an error: a branch nothing has written to does not exist.
	// +optional
	Commit string `json:"commit,omitempty"`

	// LastVerifiedAt is when the remote was last observed.
	// +optional
	LastVerifiedAt *metav1.Time `json:"lastVerifiedAt,omitempty"`

	// VerifiedBy is how commit was learned: "Push" means the remote accepted a push from GitOps
	// Reverser, and "Fetch" means a fetch found it. "Fetch" next to a commit GitOps Reverser did not
	// make means somebody else pushed to the branch.
	// +optional
	// +kubebuilder:validation:Enum=Push;Fetch
	VerifiedBy string `json:"verifiedBy,omitempty"`

	// Parent is what the branch would be created from. Set only while the branch is not on the
	// remote.
	// +optional
	Parent *GitBranchParentStatus `json:"parent,omitempty"`
}

// GitBranchParentState is what a branch that does not exist yet would be created from.
// +kubebuilder:validation:Enum=Found;Missing;Unborn
type GitBranchParentState string

const (
	// GitBranchParentFound means the parent branch exists; the first commit builds on its tip.
	GitBranchParentFound GitBranchParentState = "Found"
	// GitBranchParentMissing means the parent is not on the remote either, so the branch cannot be
	// created; the GitBranch reports ParentBranchNotFound. The parent is spec.parentBranch, or, when
	// that is omitted, the default branch of a repository that is not empty but whose HEAD names no
	// branch it carries.
	GitBranchParentMissing GitBranchParentState = "Missing"
	// GitBranchParentUnborn means the repository is empty, with no refs at all (tags included), and
	// no parent is configured, so the first commit starts the branch with no history.
	GitBranchParentUnborn GitBranchParentState = "Unborn"
)

// GitBranchParentStatus is the parent branch of a branch that does not exist yet.
type GitBranchParentStatus struct {
	// State is what the branch would be created from: "Found" (the parent branch's tip), "Missing"
	// (the parent branch is not on the remote, so nothing is written) or "Unborn" (an empty
	// repository and no spec.parentBranch, so the first commit has no history).
	// +required
	State GitBranchParentState `json:"state"`

	// Branch is the parent branch: spec.parentBranch, or the remote's default branch when that is
	// omitted. Empty when state is "Unborn", and when the remote names no default branch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the commit hash the parent branch is at on the remote, which the branch's first
	// commit would build on. Set only when state is "Found".
	// +optional
	Commit string `json:"commit,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.gitProviderRef.name`
// +kubebuilder:printcolumn:name="ProviderNamespace",type=string,JSONPath=`.spec.gitProviderRef.namespace`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.remote.commit`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.status.repository`,priority=1
// +kubebuilder:printcolumn:name="Targets",type=integer,JSONPath=`.status.gitTargetCount`,priority=1
// +kubebuilder:printcolumn:name="Verified",type=date,JSONPath=`.status.remote.lastVerifiedAt`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitBranch is one branch of a Git repository, written with a GitProvider's credential. Every
// GitTarget that references it writes through it, and only one GitBranch may hold a repository
// branch at a time.
type GitBranch struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitBranch.
	// +required
	Spec GitBranchSpec `json:"spec"`

	// status defines the observed state of GitBranch.
	// +optional
	Status GitBranchStatus `json:"status,omitempty,omitzero"`
}

// GitProviderKey is the GitProvider this GitBranch writes through: spec.gitProviderRef, in this
// GitBranch's own namespace unless the reference names another one.
func (b *GitBranch) GitProviderKey() types.NamespacedName {
	namespace := b.Spec.GitProviderRef.Namespace
	if namespace == "" {
		namespace = b.Namespace
	}
	return types.NamespacedName{Namespace: namespace, Name: b.Spec.GitProviderRef.Name}
}

// +kubebuilder:object:root=true

// GitBranchList contains a list of GitBranch.
type GitBranchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitBranch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitBranch{}, &GitBranchList{})
}
