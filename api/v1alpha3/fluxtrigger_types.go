// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// FluxTriggerFinalizer is the finalizer a FluxTrigger carries while it holds its Kustomization
// suspended: from before its first adoption write until it has handed the Kustomization back or
// over. Deleting the trigger, or the last eligible GitTarget naming it, suspends the Kustomization
// once more, waits until nothing started before that suspend is running, and then hands it back to
// Flux: spec.suspend is released, so the Kustomization is resumed. A trigger that never held its
// Kustomization has none.
const FluxTriggerFinalizer = "configbutler.ai/flux-trigger"

// FluxTriggerSpec names the Flux Kustomization the trigger holds, and the cluster it is in.
type FluxTriggerSpec struct {
	// Why immutable: the trigger holds the Kustomization it names (it suspends it while idle and
	// hands it back when deleted), so pointing it elsewhere would leave the first one held by
	// nothing. Delete and recreate the trigger instead; its finalizer hands the old one back.

	// ClusterProviderRef names the ClusterProvider whose cluster holds the Kustomization, such as a
	// management cluster running Flux for the source cluster. A provider without spec.kubeConfig is
	// the operator's own cluster. The provider must admit this trigger's namespace
	// (spec.accessFrom). Its identity needs get and patch on the Kustomization and its
	// GitRepository. Omitted,
	// it is the ClusterProvider named "default", as for a GitTarget. Immutable.
	// +optional
	// +kubebuilder:default={name: "default"}
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.clusterProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.clusterProviderRef is immutable; delete and recreate the FluxTrigger to hold a Kustomization elsewhere"
	ClusterProviderRef *meta.LocalObjectReference `json:"clusterProviderRef,omitempty"`

	// KustomizationRef is the Flux Kustomization that applies the folders of the GitTargets naming
	// this trigger. Its spec.sourceRef must be a GitRepository that follows a branch. Its namespace
	// defaults to the trigger's namespace. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kustomizationRef is immutable; delete and recreate the FluxTrigger to hold another Kustomization"
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kustomizationRef.name must not be empty"
	KustomizationRef meta.NamespacedObjectReference `json:"kustomizationRef"`
}

// FluxTriggerStatus is what the trigger knows about its Kustomization, and the apply it started.
type FluxTriggerStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are Configured (an eligible GitTarget names the trigger; the Kustomization exists,
	// applies a GitRepository that follows a branch, may be patched, and no other FluxTrigger holds
	// it), Held (Suspended: suspended under GitOps Reverser with the last suspend handled; Applying:
	// resumed for an apply GitOps Reverser started; Suspending: a suspend is not handled yet;
	// Releasing: being handed back; NotReferenced; NotConfigured), and the kstatus
	// Ready/Reconciling/Stalled trio. A trigger acts only while an eligible GitTarget references it:
	// one in its namespace that names it, is admitted by its ClusterProviders, and writes the
	// repository and branch the GitRepository fetches. A trigger no GitTarget names reads nothing
	// in the Kustomization's cluster and is Configured=Unknown, Ready=True, both NotReferenced; one
	// named only by GitTargets that are not eligible is Configured NotBound or a refusal. Of the
	// FluxTriggers in line for one Kustomization in one cluster, one holds it: the one with an apply
	// in flight, then the one holding it already, then the one created first. The others are
	// Configured=False (HeldByAnotherTrigger) until it lets go, and the next in line takes it over.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Source is the GitRepository the Kustomization applies, as last read, published while an
	// eligible GitTarget references the trigger. A GitTarget naming this trigger must write the
	// repository and branch it fetches.
	// +optional
	Source *FluxTriggerSource `json:"source,omitempty"`

	// Operation is the apply GitOps Reverser started on the Kustomization: the one in flight, or
	// the last one. Absent until the first.
	// +optional
	Operation *ApplyOperation `json:"operation,omitempty"`

	// RepairPending is true while a Repair apply is pending for the Kustomization: an apply reverted
	// an API edit made after it was asked for, so the cluster runs an older value than Git until the
	// next apply.
	// +optional
	RepairPending bool `json:"repairPending,omitempty"`
}

// FluxTriggerSource is the GitRepository a Kustomization applies.
type FluxTriggerSource struct {
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
	// GitRepository for several Kustomizations is a normal setup, and a consumer no trigger names
	// applies a folder that has no promise of protection anyway.

	// OtherConsumers are the other objects in the cluster that apply this GitRepository and that no
	// FluxTrigger holds: Kustomizations, and HelmReleases whose chart comes from it. Each applies a
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
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="RepairPending",type=boolean,JSONPath=`.status.repairPending`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FluxTrigger holds a Flux Kustomization for GitOps Reverser, which starts every apply it makes:
// only on a branch with nothing live missing from the commit, after a commit GitOps Reverser did
// not push, after a refused edit, after a reverted edit, and to catch up with its own pushes.
// GitTargets name it with spec.applyTriggerRef.
type FluxTrigger struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the Kustomization the trigger holds
	// +required
	Spec FluxTriggerSpec `json:"spec"`

	// status defines the observed state of FluxTrigger
	// +optional
	Status FluxTriggerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// FluxTriggerList contains a list of FluxTrigger.
type FluxTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FluxTrigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FluxTrigger{}, &FluxTriggerList{})
}

var _ ApplyTrigger = (*FluxTrigger)(nil)

// TriggerKind is FluxTrigger.
func (t *FluxTrigger) TriggerKind() string { return ApplyTriggerKindFlux }

// ClusterProvider is spec.clusterProviderRef.name, or "default" when it is unset, as the schema
// defaults it: a literal built in a test or by an older client still names a concrete provider.
func (t *FluxTrigger) ClusterProvider() string { return clusterProviderName(t.Spec.ClusterProviderRef) }

// ApplierKind is Kustomization.
func (t *FluxTrigger) ApplierKind() string { return ApplierKindKustomization }

// Applier is spec.kustomizationRef, its namespace defaulted to the trigger's own.
func (t *FluxTrigger) Applier() types.NamespacedName {
	return types.NamespacedName{
		Namespace: defaultedNamespace(t.Spec.KustomizationRef.Namespace, t.Namespace),
		Name:      t.Spec.KustomizationRef.Name,
	}
}

// SourceBinding is status.source's url and branch.
func (t *FluxTrigger) SourceBinding() (string, string, bool) {
	if t.Status.Source == nil {
		return "", "", false
	}
	return t.Status.Source.URL, t.Status.Source.Branch, true
}

// SharedSource is the GitRepository in status.source.
func (t *FluxTrigger) SharedSource() (types.NamespacedName, bool) {
	if t.Status.Source == nil {
		return types.NamespacedName{}, false
	}
	source := t.Status.Source.GitRepository
	return types.NamespacedName{Namespace: source.Namespace, Name: source.Name}, true
}

// Operation is status.operation.
func (t *FluxTrigger) Operation() *ApplyOperation { return t.Status.Operation }

// RepairPending is status.repairPending.
func (t *FluxTrigger) RepairPending() bool { return t.Status.RepairPending }

// SetOperation replaces status.operation and status.repairPending.
func (t *FluxTrigger) SetOperation(operation *ApplyOperation, repairPending bool) {
	t.Status.Operation, t.Status.RepairPending = operation, repairPending
}
