// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GitBranchSpec defines the desired state of GitBranch.
//
// Every field is immutable: together they are the repository branch this object claims, and a
// claim that could be repointed would move a branch worker's checkout, credential and pending
// writes to another branch under the GitTargets that reference it. Delete and recreate instead.
// +kubebuilder:validation:XValidation:rule="self.gitProviderRef == oldSelf.gitProviderRef",message="spec.gitProviderRef is immutable; delete and recreate the GitBranch to change it"
// +kubebuilder:validation:XValidation:rule="self.branch == oldSelf.branch",message="spec.branch is immutable; delete and recreate the GitBranch to change it"
// +kubebuilder:validation:XValidation:rule="has(self.parentBranch) == has(oldSelf.parentBranch) && (!has(self.parentBranch) || self.parentBranch == oldSelf.parentBranch)",message="spec.parentBranch is immutable; delete and recreate the GitBranch to change it"
type GitBranchSpec struct {
	// A LocalObjectReference on purpose: creating a GitBranch then needs the right to create objects
	// in the namespace that holds the credential, which is what makes a branch claim an act of the
	// credential's owner. Other namespaces reach the branch through the GitProvider's accessFrom.

	// GitProviderRef names the GitProvider, in this GitBranch's namespace, whose repository and
	// credential this branch is written with. GitTargets in other namespaces may reference this
	// GitBranch while that GitProvider's spec.accessFrom admits their namespace.
	// Immutable: delete and recreate the GitBranch to change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitProviderRef.name must not be empty"
	GitProviderRef meta.LocalObjectReference `json:"gitProviderRef"`

	// Branch is the branch on the remote that this GitBranch writes. It must match one of the
	// GitProvider's allowedBranches. Only one GitBranch writes a repository branch: the oldest one
	// whose credential proves it can open a push session holds it, and any other GitBranch for the
	// same repository and branch reports Stalled with reason BranchConflict.
	// Immutable: delete and recreate the GitBranch to change it.
	// +required
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// Why "parent" and not base/source/revision: docs/definitions.md reserves those words, and the
	// field is flat because it is one fact. It decides what history a new write branch gets, and a
	// parent changed under a worker holding writes is the one case that needed its own replay, so it
	// is immutable. It can never hold a commit or tag: a pinned starting point would be a sibling.

	// ParentBranch is the branch spec.branch is created from, and the branch compared against while
	// spec.branch does not exist on the remote. Omitted: the branch the remote's HEAD points at (its
	// default branch), or a branch with no history when the repository is empty. When set, it must
	// exist on the remote for spec.branch to be created; otherwise this GitBranch reports
	// ParentBranchNotFound and nothing is written. Once spec.branch exists, it is used as it is and
	// the parent is not followed. It is only read, so it does not need to be in the GitProvider's
	// allowedBranches.
	// Immutable, including adding or removing it: delete and recreate the GitBranch to change it.
	// +optional
	// +kubebuilder:validation:MinLength=1
	ParentBranch string `json:"parentBranch,omitempty"`
}

// GitBranchStatus defines the observed state of GitBranch.
type GitBranchStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report whether this GitBranch holds its repository branch and can write it: the
	// Ready summary plus the kstatus Reconciling/Stalled pair.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Repository is the GitProvider's repository URL, normalized: host and path, with no scheme,
	// user or ".git" suffix. Together with spec.branch it is the key only one GitBranch may hold, so
	// two GitProviders spelling one repository differently still conflict.
	// +optional
	Repository string `json:"repository,omitempty"`

	// LastPushNoticeAt is when a GitReceiver last delivered a push to this branch, which made the
	// branch worker look at once instead of at its next refresh. It records receipt only: whether
	// that look reached the remote is status.remote's to say. Absent means no delivery has reached
	// this branch: no GitReceiver is set up, the Git host's webhook does not reach one, the delivery
	// names the repository by a URL other than the GitProvider's spec.url, or no branch worker writes
	// the branch because no GitTarget references this GitBranch. It advances at most once a minute,
	// and is cleared when the GitProvider is recreated for another repository.
	// +optional
	LastPushNoticeAt *metav1.Time `json:"lastPushNoticeAt,omitempty"`

	// Remote is where this branch is on the Git remote, and when that was last proved. It is
	// renewed by every confirmed look: a successful push as well as a fetch. Absent means nothing
	// has proved anything yet: nothing has looked, or the GitProvider was recreated against a
	// different repository and what was published no longer describes the one it names.
	// +optional
	Remote *GitBranchRemoteStatus `json:"remote,omitempty"`

	// GitTargetCount is how many GitTargets reference this GitBranch, from any namespace. More than
	// one means they share its branch worker; each reports its own folder's health on its own
	// conditions.
	// +optional
	GitTargetCount int32 `json:"gitTargetCount"`
}

// GitBranchRemoteStatus is the answer to "where is my branch, and when did we last prove it".
//
// It is sampled when the GitBranch reconciles, from what its branch worker last proved about the
// branch, and written at most once a minute. The first observation, and a correction after the
// repository changed under the branch, are written at once.
type GitBranchRemoteStatus struct {
	// Commit is the commit hash the branch is at on the remote. EMPTY means the branch is not on
	// the remote at all, which is not an error: a branch does not exist without a commit, and a
	// branch nothing has written to has nothing there yet.
	// +optional
	Commit string `json:"commit,omitempty"`

	// LastVerifiedAt is when the remote was last observed.
	// +optional
	LastVerifiedAt *metav1.Time `json:"lastVerifiedAt,omitempty"`

	// VerifiedBy is what proved it: `Push` means the server accepted a ref update of ours, so
	// this commit is our own work; `Fetch` means we went and looked, and this is what was
	// there. A `Fetch` next to a commit no publication of yours produced is how a foreign push
	// to the branch is read off kubectl.
	// +optional
	// +kubebuilder:validation:Enum=Push;Fetch
	VerifiedBy string `json:"verifiedBy,omitempty"`

	// Parent is what the branch would be created from, observed with it: present only while the
	// remote does not carry the branch. Once the branch exists its parent is not followed, so this
	// is omitted.
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
	// State is what the branch would be created from: Found (the parent's tip), Missing (the parent
	// is not on the remote; nothing is written) or Unborn (an empty repository, with no refs at
	// all, and no parent configured; the first commit has no history).
	// +required
	State GitBranchParentState `json:"state"`

	// Branch is the parent branch: spec.parentBranch, or the branch the remote's HEAD names when
	// that is omitted. Empty when State is Unborn, and when the remote names no default branch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Commit is the commit hash the parent branch is at on the remote: the commit the branch's
	// first commit would build on. Set only when State is Found.
	// +optional
	Commit string `json:"commit,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.gitProviderRef.name`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.remote.commit`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.status.repository`,priority=1
// +kubebuilder:printcolumn:name="Targets",type=integer,JSONPath=`.status.gitTargetCount`,priority=1
// +kubebuilder:printcolumn:name="Verified",type=date,JSONPath=`.status.remote.lastVerifiedAt`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GitBranch is one branch of a Git repository, written through a GitProvider's credential. It holds
// the branch for every GitTarget that references it: one branch worker, one credential, one
// checkout. Only one GitBranch may hold a repository branch at a time.
type GitBranch struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitBranch
	// +required
	Spec GitBranchSpec `json:"spec"`

	// status defines the observed state of GitBranch
	// +optional
	Status GitBranchStatus `json:"status,omitempty,omitzero"`
}

// GitProviderKey is the GitProvider this GitBranch writes through: spec.gitProviderRef, in this
// GitBranch's own namespace.
func (b *GitBranch) GitProviderKey() types.NamespacedName {
	return types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.GitProviderRef.Name}
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
