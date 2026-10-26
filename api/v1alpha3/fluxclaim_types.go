// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// FluxClaimFinalizer is the finalizer a FluxClaim carries while it holds its Kustomization
// suspended: from before its first adoption write until it has handed the Kustomization back or
// over. Deleting the applier claim, or the last eligible GitTarget naming it, suspends the Kustomization
// once more, waits until nothing started before that suspend is running, and then hands it back to
// Flux as status.previousSuspendState recorded it: spec.suspend is released, so the Kustomization is resumed,
// unless it was suspended before the applier claim held it, and stays so. An applier claim that never held its
// Kustomization has none.
const FluxClaimFinalizer = "configbutler.ai/flux-claim"

// FluxClaimSpec names the Flux Kustomization the applier claim holds, and the
// cluster it is in.
type FluxClaimSpec struct {
	// Why immutable: the applier claim holds the Kustomization it names (it suspends it while idle and
	// hands it back when deleted), so pointing it elsewhere would leave the first one held by
	// nothing. Delete and recreate the applier claim instead; its finalizer hands the old one back.

	// KubeProviderRef names the KubeProvider of the cluster that runs the
	// Kustomization, such as a management cluster running Flux. An omitted
	// namespace means this FluxClaim's namespace. The KubeProvider's
	// spec.allowedApplierClaims must admit this namespace, with the
	// Kustomization's namespace in applierNamespaces, and its identity needs get
	// and patch on the Kustomization and its GitRepository. Immutable: delete and
	// recreate the FluxClaim to change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kubeProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kubeProviderRef is immutable; delete and recreate the FluxClaim to hold a Kustomization elsewhere"
	KubeProviderRef meta.NamespacedObjectReference `json:"kubeProviderRef"`

	// KustomizationRef names the Flux Kustomization that applies the folders of
	// the GitTargets naming this FluxClaim. Its spec.sourceRef must be a
	// GitRepository that follows a branch. An omitted namespace means this
	// FluxClaim's namespace. Immutable: delete and recreate the FluxClaim to
	// change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kustomizationRef is immutable; delete and recreate the FluxClaim to hold another Kustomization"
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kustomizationRef.name must not be empty"
	KustomizationRef meta.NamespacedObjectReference `json:"kustomizationRef"`

	// Why a timer: a push of GitOps Reverser's own needs no apply for the cluster (the flush
	// already put the edit there), so none is asked for at once; but what reads the applier's own
	// state (its applied revision, its source artifact) would stay at the last foreign commit until
	// someone else pushed. One apply per interval keeps that level and batches a burst of saves.

	// CatchUpAfter is how long the Kustomization stays behind the branch after
	// a push of GitOps Reverser's own before an apply of the tip is asked for,
	// as a Go duration ("1m", "90s"). Such a push changes nothing on the
	// cluster, which already holds the edit, but leaves
	// status.lastAppliedRevision and the GitRepository's artifact at the commit
	// before it. The interval starts when the Kustomization is first seen
	// behind, so a burst of saves costs one apply, and one at the tip costs
	// none. The apply can still wait for startup, a retry backoff or edits
	// being published. "0s" never asks for this reason, and drops an apply it
	// asked for that has not started. Otherwise at least "10s" and at most
	// "24h". Defaults to "1m".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="1m"
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || (duration(self) >= duration('10s') && duration(self) <= duration('24h'))",message="spec.catchUpAfter must be 0s, or a Go duration from 10s to 24h"
	CatchUpAfter *metav1.Duration `json:"catchUpAfter,omitempty"`
}

// FluxClaimStatus is what the applier claim knows about its Kustomization, and
// the apply it started.
type FluxClaimStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this FluxClaim's state. Ready,
	// Reconciling and Stalled follow kstatus.
	//
	// Configured is True when an eligible GitTarget names this FluxClaim, and the
	// Kustomization exists, applies a GitRepository that follows a branch, may be
	// patched, and no other FluxClaim holds it. An eligible GitTarget is in this
	// namespace, is admitted by its KubeProviders, and writes the branch the
	// GitRepository fetches. Of several FluxClaims for one Kustomization, the one
	// with an apply in flight holds it, then the one holding it already, then the
	// oldest; the others are Configured=False, reason HeldByAnotherClaim.
	//
	// InControl says whether GitOps Reverser decides when the Kustomization
	// applies:
	//
	// - True, reason Suspended: the Kustomization is suspended, and GitOps
	//   Reverser starts every apply.
	// - True, reason Applying: an apply GitOps Reverser started is running;
	//   the Kustomization is suspended again when it ends.
	// - False, reason Suspending: Flux has not yet handled the suspend.
	// - False, reason Releasing or Revoking: the FluxClaim is handing the
	//   Kustomization back.
	// - False, reason NotReferenced: no GitTarget names this FluxClaim.
	// - Unknown, reason NotConfigured: Configured is not True, or another
	//   FluxClaim holds the Kustomization.
	//
	// A FluxClaim no GitTarget names reads nothing and is Ready=True, reason
	// NotReferenced.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Source is the GitRepository the Kustomization applies, as last read. It is
	// published while an eligible GitTarget names this FluxClaim, and that
	// GitTarget must write the repository and branch it fetches.
	// +optional
	Source *FluxClaimSource `json:"source,omitempty"`

	// Operation is the apply GitOps Reverser started on the Kustomization: the one
	// in flight, or the last one. Absent until the first.
	// +optional
	Operation *ApplyOperation `json:"operation,omitempty"`

	// RepairPending is true while a Repair apply is pending: an apply ran a commit
	// older than an edit made through the API and reverted it on the cluster. Git
	// holds the edit, and the cluster runs the older value until the next apply.
	// +optional
	RepairPending bool `json:"repairPending,omitempty"`

	// PreviousSuspendState is how the Kustomization stood before this FluxClaim
	// first suspended it, and what handing it back restores: one that was
	// Suspended stays suspended, one that was NotSuspended is resumed. It is
	// cleared once the Kustomization is handed back. Absent, handing it back
	// resumes the Kustomization.
	// +optional
	PreviousSuspendState SuspendState `json:"previousSuspendState,omitempty"`
}

// FluxClaimSource is the GitRepository a Kustomization applies.
type FluxClaimSource struct {
	// GitRepository is the Kustomization's spec.sourceRef, with its namespace
	// filled in.
	// +required
	GitRepository meta.NamespacedObjectReference `json:"gitRepository"`

	// URL is the GitRepository's spec.url.
	// +optional
	URL string `json:"url,omitempty"`

	// Branch is the GitRepository's spec.ref.branch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Why information only (decision 2 of 2026-10-17 in docs/design/apply-trigger.md): one
	// GitRepository for several Kustomizations is a normal setup, and an object no applier claim names
	// applies a folder that has no promise of protection anyway.

	// AlsoAppliedBy lists the other objects that apply this GitRepository and that
	// no FluxClaim holds: Kustomizations, and HelmReleases whose chart comes from
	// it. Each applies a new commit as soon as Flux fetches it. Information only;
	// it never affects Ready.
	// +optional
	// +listType=atomic
	AlsoAppliedBy []meta.NamespacedObjectKindReference `json:"alsoAppliedBy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Control",type=string,JSONPath=`.status.conditions[?(@.type=="InControl")].reason`
// +kubebuilder:printcolumn:name="Kustomization",type=string,JSONPath=`.spec.kustomizationRef.name`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.status.operation.phase`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.operation.commit`,priority=1
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.kubeProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="RepairPending",type=boolean,JSONPath=`.status.repairPending`,priority=1
// +kubebuilder:printcolumn:name="PreviousSuspendState",type=string,JSONPath=`.status.previousSuspendState`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FluxClaim lets GitOps Reverser decide when a Flux Kustomization applies.
// The Kustomization stays suspended, and GitOps Reverser resumes it for each
// apply it starts, always after the edits pending on the branch are
// published. It starts one:
//
//   - after a commit that GitOps Reverser did not push;
//   - after a refused edit, on a GitTarget with spec.onRefusal "Reapply";
//   - after an apply reverted an edit made through the API;
//   - after objects were created through the API in its folders;
//   - once at startup, and when a GitTarget first names this FluxClaim;
//   - once spec.catchUpAfter passed since a push of GitOps Reverser's own;
//   - after a SaveRequest with spec.catchUp "AfterPush" pushed a commit.
//
// GitTargets name it with spec.applierClaimRef.
type FluxClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of FluxClaim.
	// +required
	Spec FluxClaimSpec `json:"spec"`

	// status defines the observed state of FluxClaim.
	// +optional
	Status FluxClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// FluxClaimList contains a list of FluxClaim.
type FluxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FluxClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FluxClaim{}, &FluxClaimList{})
}

// DefaultFluxCatchUpAfter is a FluxClaim's spec.catchUpAfter when it is omitted.
const DefaultFluxCatchUpAfter = time.Minute

var _ ApplierClaim = (*FluxClaim)(nil)

// ClaimKind is FluxClaim.
func (t *FluxClaim) ClaimKind() string { return ApplierClaimKindFlux }

// KubeProviderKey is spec.kubeProviderRef, its namespace defaulted to the applier claim's own.
func (t *FluxClaim) KubeProviderKey() types.NamespacedName {
	return kubeProviderKey(t.Spec.KubeProviderRef, t.Namespace)
}

// ApplierKind is Kustomization.
func (t *FluxClaim) ApplierKind() string { return ApplierKindKustomization }

// Applier is spec.kustomizationRef, its namespace defaulted to the applier claim's own.
func (t *FluxClaim) Applier() types.NamespacedName {
	return types.NamespacedName{
		Namespace: defaultedNamespace(t.Spec.KustomizationRef.Namespace, t.Namespace),
		Name:      t.Spec.KustomizationRef.Name,
	}
}

// SourceBinding is status.source's url and branch.
func (t *FluxClaim) SourceBinding() (string, string, bool) {
	if t.Status.Source == nil {
		return "", "", false
	}
	return t.Status.Source.URL, t.Status.Source.Branch, true
}

// SharedSource is the GitRepository in status.source.
func (t *FluxClaim) SharedSource() (types.NamespacedName, bool) {
	if t.Status.Source == nil {
		return types.NamespacedName{}, false
	}
	source := t.Status.Source.GitRepository
	return types.NamespacedName{Namespace: source.Namespace, Name: source.Name}, true
}

// CatchUpAfter is spec.catchUpAfter, or DefaultFluxCatchUpAfter when it is omitted.
func (t *FluxClaim) CatchUpAfter() time.Duration {
	return durationOr(t.Spec.CatchUpAfter, DefaultFluxCatchUpAfter)
}

// Operation is status.operation.
func (t *FluxClaim) Operation() *ApplyOperation { return t.Status.Operation }

// RepairPending is status.repairPending.
func (t *FluxClaim) RepairPending() bool { return t.Status.RepairPending }

// SetOperation replaces status.operation and status.repairPending.
func (t *FluxClaim) SetOperation(operation *ApplyOperation, repairPending bool) {
	t.Status.Operation, t.Status.RepairPending = operation, repairPending
}

// PreviousSuspendState is status.previousSuspendState.
func (t *FluxClaim) PreviousSuspendState() SuspendState { return t.Status.PreviousSuspendState }

// SetPreviousSuspendState replaces status.previousSuspendState.
func (t *FluxClaim) SetPreviousSuspendState(state SuspendState) {
	t.Status.PreviousSuspendState = state
}
