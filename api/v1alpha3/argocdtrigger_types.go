// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"strings"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ArgoCDTriggerFinalizer is the finalizer an ArgoCDTrigger carries while it holds its
// Application's automated sync off: from before it first switches it off until it has handed the
// switch back or over. Deleting the trigger, or the last eligible GitTarget naming it, gives the
// switch up, so the Application is as its own spec says again. A trigger that never held the
// switch (one whose Application an ApplicationSet generated, or whose switch another field manager
// applies) has none.
const ArgoCDTriggerFinalizer = "configbutler.ai/argocd-trigger"

// spec.clusterProviderRef was replaced by spec.kubeProviderRef, with the cluster-scoped kind it
// named. It stays in the schema only to be refused by name; the rule sits on spec so CRD validation
// ratcheting cannot wave a stored value through on the next edit.

// ArgoCDTriggerSpec names the Argo CD Application the trigger holds, and the cluster it is in.
// +kubebuilder:validation:XValidation:rule="!has(self.clusterProviderRef)",message="spec.clusterProviderRef was replaced by spec.kubeProviderRef, which names a namespaced KubeProvider"
type ArgoCDTriggerSpec struct {
	// Immutable for the reason FluxTriggerSpec's references are: the trigger holds what it names.

	// KubeProviderRef names the KubeProvider whose cluster holds the Application, such as a
	// management cluster running Argo CD for the source cluster. Namespace omitted means this
	// trigger's own namespace. The provider must admit this trigger's namespace: its own, or one its
	// spec.accessFrom admits. Its identity needs get, update and patch on the Application. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kubeProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kubeProviderRef is immutable; delete and recreate the ArgoCDTrigger to hold an Application elsewhere"
	KubeProviderRef meta.NamespacedObjectReference `json:"kubeProviderRef"`

	// RemovedClusterProviderRef is the replaced spelling of kubeProviderRef. It is never read;
	// setting it is refused.
	// +optional
	RemovedClusterProviderRef *meta.LocalObjectReference `json:"clusterProviderRef,omitempty"`

	// ApplicationRef is the Argo CD Application that applies the folders of the GitTargets naming
	// this trigger. It must have a single spec.source whose targetRevision names a branch. The
	// trigger holds its automated sync off: it applies spec.syncPolicy.automated.enabled: false,
	// which keeps automated.prune for the syncs GitOps Reverser starts, switches it off again when
	// somebody switches it on, and gives it up when it lets go. An Application an ApplicationSet
	// generated, or whose switch another field manager applies, must have automated sync off
	// already: the switch is theirs, and the trigger only checks it. GitOps Reverser starts each
	// sync by setting the Application's operation, as Argo CD's automated sync does, pinned to the
	// commit it checked, and never replaces an operation that is running. Its namespace defaults to
	// the trigger's namespace. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.applicationRef is immutable; delete and recreate the ArgoCDTrigger to hold another Application"
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.applicationRef.name must not be empty"
	ApplicationRef meta.NamespacedObjectReference `json:"applicationRef"`
}

// ArgoCDTriggerStatus is what the trigger knows about its Application, and the sync it started.
type ArgoCDTriggerStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are Configured (an eligible GitTarget names the trigger; the Application exists,
	// syncs a single source, has automated sync off or a switch the trigger may hold, may be
	// written, and no other ArgoCDTrigger holds it), Held (AutomatedSyncOff: automated sync is off,
	// held by GitOps Reverser or kept off by the switch's owner; Releasing: being handed back;
	// NotReferenced; NotConfigured), and the kstatus Ready/Reconciling/Stalled trio. A trigger acts only while an
	// eligible GitTarget references it: one in its namespace that names it, is admitted by its
	// KubeProviders, and writes the repository and branch the Application syncs. A trigger no
	// GitTarget names reads nothing in the Application's cluster and is Configured=Unknown,
	// Ready=True, both NotReferenced; one named only by GitTargets that are not eligible is
	// Configured NotBound or a refusal. Of the ArgoCDTriggers in line for one Application in one
	// cluster, one holds it: the one holding it already, then the one created first. The others are
	// Configured=False (HeldByAnotherTrigger) until it lets go, and the next in line takes it over.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Source is the Application's spec.source, as last read, published while an eligible GitTarget
	// references the trigger. A GitTarget naming this trigger must write the repository it syncs, on
	// the branch its targetRevision names.
	// +optional
	Source *ArgoCDTriggerSource `json:"source,omitempty"`

	// Operation is the sync GitOps Reverser started on the Application: the one in flight, or the
	// last one. Absent until the first.
	// +optional
	Operation *ApplyOperation `json:"operation,omitempty"`

	// RepairPending is true while a Repair sync is pending for the Application: a sync reverted an
	// API edit made after its flush, so the cluster runs an older value than Git until the
	// next sync.
	// +optional
	RepairPending bool `json:"repairPending,omitempty"`
}

// ArgoCDTriggerSource is the source an Application syncs.
type ArgoCDTriggerSource struct {
	// RepoURL is the Application's spec.source.repoURL.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`

	// TargetRevision is the Application's spec.source.targetRevision, as written: a branch name, or
	// refs/heads/ and the name. Empty is Argo CD's HEAD.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Held",type=string,JSONPath=`.status.conditions[?(@.type=="Held")].reason`
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.applicationRef.name`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.status.operation.phase`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.operation.commit`,priority=1
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.kubeProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="RepairPending",type=boolean,JSONPath=`.status.repairPending`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ArgoCDTrigger holds an Argo CD Application's automated sync off for GitOps Reverser, which starts
// every sync it runs, each after publishing the edits pending on the branch: after a commit
// GitOps Reverser did not push, after a refused edit, after a reverted edit, and to adopt what the
// API created or converge the Application (CatchUp). GitTargets name it with spec.applyTriggerRef.
type ArgoCDTrigger struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the Application the trigger holds
	// +required
	Spec ArgoCDTriggerSpec `json:"spec"`

	// status defines the observed state of ArgoCDTrigger
	// +optional
	Status ArgoCDTriggerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ArgoCDTriggerList contains a list of ArgoCDTrigger.
type ArgoCDTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ArgoCDTrigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArgoCDTrigger{}, &ArgoCDTriggerList{})
}

var _ ApplyTrigger = (*ArgoCDTrigger)(nil)

// TriggerKind is ArgoCDTrigger.
func (t *ArgoCDTrigger) TriggerKind() string { return ApplyTriggerKindArgoCD }

// KubeProviderKey is spec.kubeProviderRef, its namespace defaulted to the trigger's own.
func (t *ArgoCDTrigger) KubeProviderKey() types.NamespacedName {
	return kubeProviderKey(t.Spec.KubeProviderRef, t.Namespace)
}

// ApplierKind is Application.
func (t *ArgoCDTrigger) ApplierKind() string { return ApplierKindApplication }

// Applier is spec.applicationRef, its namespace defaulted to the trigger's own.
func (t *ArgoCDTrigger) Applier() types.NamespacedName {
	return types.NamespacedName{
		Namespace: defaultedNamespace(t.Spec.ApplicationRef.Namespace, t.Namespace),
		Name:      t.Spec.ApplicationRef.Name,
	}
}

// SourceBinding is status.source's repoURL, and the branch its targetRevision names ("HEAD" for an
// empty one, which follows no branch GitOps Reverser writes).
func (t *ArgoCDTrigger) SourceBinding() (string, string, bool) {
	if t.Status.Source == nil {
		return "", "", false
	}
	revision := strings.TrimPrefix(t.Status.Source.TargetRevision, "refs/heads/")
	if revision == "" {
		revision = "HEAD"
	}
	return t.Status.Source.RepoURL, revision, true
}

// SharedSource is none: every sync pins its own revision, so Applications reading one repository
// do not disturb each other's operations.
func (t *ArgoCDTrigger) SharedSource() (types.NamespacedName, bool) {
	return types.NamespacedName{}, false
}

// Operation is status.operation.
func (t *ArgoCDTrigger) Operation() *ApplyOperation { return t.Status.Operation }

// RepairPending is status.repairPending.
func (t *ArgoCDTrigger) RepairPending() bool { return t.Status.RepairPending }

// SetOperation replaces status.operation and status.repairPending.
func (t *ArgoCDTrigger) SetOperation(operation *ApplyOperation, repairPending bool) {
	t.Status.Operation, t.Status.RepairPending = operation, repairPending
}
