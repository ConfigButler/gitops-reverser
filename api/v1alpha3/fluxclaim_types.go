// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
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

// spec.clusterProviderRef was replaced by spec.kubeProviderRef, with the cluster-scoped kind it
// named. It stays in the schema only to be refused by name; the rule sits on spec so CRD validation
// ratcheting cannot wave a stored value through on the next edit.

// FluxClaimSpec names the Flux Kustomization the applier claim holds, and the cluster it is in.
// +kubebuilder:validation:XValidation:rule="!has(self.clusterProviderRef)",message="spec.clusterProviderRef was replaced by spec.kubeProviderRef, which names a namespaced KubeProvider"
type FluxClaimSpec struct {
	// Why immutable: the applier claim holds the Kustomization it names (it suspends it while idle and
	// hands it back when deleted), so pointing it elsewhere would leave the first one held by
	// nothing. Delete and recreate the applier claim instead; its finalizer hands the old one back.

	// KubeProviderRef names the KubeProvider whose cluster holds the Kustomization, such as a
	// management cluster running Flux for the source cluster. Namespace omitted means this
	// applier claim's own namespace. The provider must admit this applier claim's namespace: its own, or one its
	// spec.accessFrom admits. Its identity needs get and patch on the Kustomization and its GitRepository. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kubeProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kubeProviderRef is immutable; delete and recreate the FluxClaim to hold a Kustomization elsewhere"
	KubeProviderRef meta.NamespacedObjectReference `json:"kubeProviderRef"`

	// RemovedClusterProviderRef is the replaced spelling of kubeProviderRef. It is never read;
	// setting it is refused.
	// +optional
	RemovedClusterProviderRef *meta.LocalObjectReference `json:"clusterProviderRef,omitempty"`

	// KustomizationRef is the Flux Kustomization that applies the folders of the GitTargets naming
	// this applier claim. Its spec.sourceRef must be a GitRepository that follows a branch. Its namespace
	// defaults to the applier claim's namespace. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kustomizationRef is immutable; delete and recreate the FluxClaim to hold another Kustomization"
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kustomizationRef.name must not be empty"
	KustomizationRef meta.NamespacedObjectReference `json:"kustomizationRef"`
}

// FluxClaimStatus is what the applier claim knows about its Kustomization, and the apply it started.
type FluxClaimStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are Configured (an eligible GitTarget names the applier claim; the Kustomization exists,
	// applies a GitRepository that follows a branch, may be patched, and no other FluxClaim holds
	// it), Held (Suspended: suspended under GitOps Reverser with the last suspend handled; Applying:
	// resumed for an apply GitOps Reverser started; Suspending: a suspend is not handled yet;
	// Releasing: being handed back; NotReferenced; NotConfigured), and the kstatus
	// Ready/Reconciling/Stalled trio. An applier claim acts only while an eligible GitTarget references it:
	// one in its namespace that names it, is admitted by its KubeProviders, and writes the
	// repository and branch the GitRepository fetches. An applier claim no GitTarget names reads nothing
	// in the Kustomization's cluster and is Configured=Unknown, Ready=True, both NotReferenced; one
	// named only by GitTargets that are not eligible is Configured NotBound or a refusal. Of the
	// FluxClaims in line for one Kustomization in one cluster, one holds it: the one with an apply
	// in flight, then the one holding it already, then the one created first. The others are
	// Configured=False (HeldByAnotherClaim) until it lets go, and the next in line takes it over.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Source is the GitRepository the Kustomization applies, as last read, published while an
	// eligible GitTarget references the applier claim. A GitTarget naming this applier claim must write the
	// repository and branch it fetches.
	// +optional
	Source *FluxClaimSource `json:"source,omitempty"`

	// Operation is the apply GitOps Reverser started on the Kustomization: the one in flight, or
	// the last one. Absent until the first.
	// +optional
	Operation *ApplyOperation `json:"operation,omitempty"`

	// RepairPending is true while a Repair apply is pending for the Kustomization: an apply reverted
	// an API edit made after its flush, so the cluster runs an older value than Git until the
	// next apply.
	// +optional
	RepairPending bool `json:"repairPending,omitempty"`

	// PreviousSuspendState is how the Kustomization stood before the applier claim first suspended it,
	// which handing it back restores: one that was Suspended stays suspended, one that was
	// NotSuspended is resumed. Recorded with the first suspend, and cleared once the Kustomization is
	// handed back. Absent, a hand-back resumes the Kustomization.
	// +optional
	PreviousSuspendState SuspendState `json:"previousSuspendState,omitempty"`
}

// FluxClaimSource is the GitRepository a Kustomization applies.
type FluxClaimSource struct {
	// GitRepository is the Kustomization's spec.sourceRef, its namespace defaulted.
	// +required
	GitRepository meta.NamespacedObjectReference `json:"gitRepository"`

	// URL is the GitRepository's spec.url.
	// +optional
	URL string `json:"url,omitempty"`

	// Branch is the GitRepository's spec.ref.branch.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Why information only (decision 2 of 2026-10-17 in docs/design/apply-trigger.md): one
	// GitRepository for several Kustomizations is a normal setup, and a consumer no applier claim names
	// applies a folder that has no promise of protection anyway.

	// OtherConsumers are the other objects in the cluster that apply this GitRepository and that no
	// FluxClaim holds: Kustomizations, and HelmReleases whose chart comes from it. Each applies a
	// new artifact the moment it arrives. Information only: it never affects Ready.
	// +optional
	// +listType=atomic
	OtherConsumers []meta.NamespacedObjectKindReference `json:"otherConsumers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Held",type=string,JSONPath=`.status.conditions[?(@.type=="Held")].reason`
// +kubebuilder:printcolumn:name="Kustomization",type=string,JSONPath=`.spec.kustomizationRef.name`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.status.operation.phase`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.operation.commit`,priority=1
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.kubeProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="RepairPending",type=boolean,JSONPath=`.status.repairPending`,priority=1
// +kubebuilder:printcolumn:name="PreviousSuspendState",type=string,JSONPath=`.status.previousSuspendState`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FluxClaim holds a Flux Kustomization for GitOps Reverser, which starts every apply it makes,
// each after publishing the edits pending on the branch: after a commit GitOps Reverser did not
// push, after a refused edit, after a reverted edit, and to adopt what the API created or converge
// the Kustomization (CatchUp). GitTargets name it with spec.applierClaimRef.
type FluxClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the Kustomization the applier claim holds
	// +required
	Spec FluxClaimSpec `json:"spec"`

	// status defines the observed state of FluxClaim
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
