// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"strings"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ArgoCDClaimFinalizer is the finalizer an ArgoCDClaim carries while it holds its
// Application's automated sync off: from before it first switches it off until it has handed the
// switch back or over. Deleting the applier claim, or the last eligible GitTarget naming it, hands the
// switch back as status.previousSuspendState recorded it. An applier claim that never held the
// switch (one whose Application an ApplicationSet generated, or whose switch another field manager
// applies) has none.
const ArgoCDClaimFinalizer = "configbutler.ai/argocd-claim"

// ArgoCDClaimSpec names the Argo CD Application the applier claim holds, and
// the cluster it is in.
type ArgoCDClaimSpec struct {
	// Immutable for the reason FluxClaimSpec's references are: the applier claim holds what it names.

	// KubeProviderRef names the KubeProvider of the cluster that runs the
	// Application, such as a management cluster running Argo CD. An omitted
	// namespace means this ArgoCDClaim's namespace. The KubeProvider's
	// spec.allowedApplierClaims must admit this namespace, with the Application's
	// namespace in applierNamespaces, and its identity needs get, update and patch
	// on the Application. Immutable: delete and recreate the ArgoCDClaim to change
	// it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.kubeProviderRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.kubeProviderRef is immutable; delete and recreate the ArgoCDClaim to hold an Application elsewhere"
	KubeProviderRef meta.NamespacedObjectReference `json:"kubeProviderRef"`

	// ApplicationRef names the Argo CD Application that syncs the folders of the
	// GitTargets naming this ArgoCDClaim. It must have a single spec.source whose
	// targetRevision names a branch.
	//
	// The ArgoCDClaim keeps its automated sync off by applying
	// spec.syncPolicy.automated.enabled: false, switches it off again when
	// somebody switches it on, and gives the switch up when it lets go;
	// automated.prune still applies to the syncs GitOps Reverser starts. An
	// Application that an ApplicationSet generates, or whose switch another field
	// manager applies, must have automated sync off already. Each sync is pinned
	// to the commit GitOps Reverser checked, and a running operation is never
	// replaced.
	//
	// An omitted namespace means this ArgoCDClaim's namespace. Immutable: delete
	// and recreate the ArgoCDClaim to change it.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.applicationRef is immutable; delete and recreate the ArgoCDClaim to hold another Application"
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.applicationRef.name must not be empty"
	ApplicationRef meta.NamespacedObjectReference `json:"applicationRef"`

	// Why off by default, where a FluxClaim's is on: a sync runs the Application's hooks, which
	// cost more than a no-op apply, and Argo CD's own refresh already compares the live state
	// against the tip without a sync. Only the last sync's revision lags.

	// CatchUpAfter is how long the Application stays behind the branch after
	// a push of GitOps Reverser's own before a sync of the tip is asked for,
	// as a Go duration ("5m"). Such a push changes nothing on the cluster,
	// which already holds the edit, but leaves the Application's last sync at
	// the commit before it. The interval starts when the Application is first
	// seen behind, so a burst of saves costs one sync, hooks included, and one
	// at the tip costs none. The sync can still wait for startup, a retry
	// backoff or edits being published. "0s" never asks for this reason, and
	// drops a sync it asked for that has not started. Otherwise at least "10s"
	// and at most "24h". Defaults to "0s".
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$"
	// +kubebuilder:default="0s"
	// +kubebuilder:validation:XValidation:rule="duration(self) == duration('0s') || (duration(self) >= duration('10s') && duration(self) <= duration('24h'))",message="spec.catchUpAfter must be 0s, or a Go duration from 10s to 24h"
	CatchUpAfter *metav1.Duration `json:"catchUpAfter,omitempty"`
}

// ArgoCDClaimStatus is what the applier claim knows about its Application, and
// the sync it started.
type ArgoCDClaimStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this ArgoCDClaim's state. Ready,
	// Reconciling and Stalled follow kstatus.
	//
	// Configured is True when an eligible GitTarget names this ArgoCDClaim, and
	// the Application exists, syncs a single source, has automated sync off or a
	// switch this ArgoCDClaim may hold, may be written, and no other ArgoCDClaim
	// holds it. An eligible GitTarget is in this namespace, is admitted by its
	// KubeProviders, and writes the branch the Application syncs. Of several
	// ArgoCDClaims for one Application, the one holding it already holds it, then
	// the oldest; the others are Configured=False, reason HeldByAnotherClaim.
	//
	// InControl says whether GitOps Reverser decides when the Application
	// syncs:
	//
	// - True, reason AutomatedSyncOff: automated sync is off and GitOps
	//   Reverser starts every sync, also when another field manager owns the
	//   switch and keeps it off.
	// - False, reason Releasing or Revoking: the ArgoCDClaim is handing the
	//   Application back.
	// - False, reason NotReferenced: no GitTarget names this ArgoCDClaim.
	// - False, reason NotConfigured: another field manager owns the switch and
	//   has turned automated sync on.
	// - False, reason ArgoCDVersionNotSupported: the Application has no
	//   spec.syncPolicy.automated.enabled field (Argo CD before 3.1).
	// - Unknown, reason NotConfigured: Configured is not True, or another
	//   ArgoCDClaim holds the Application.
	//
	// A failed read of the Application keeps the previous InControl value. An
	// ArgoCDClaim no GitTarget names reads nothing and is Ready=True, reason
	// NotReferenced.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Source is the Application's spec.source, as last read. It is published while
	// an eligible GitTarget names this ArgoCDClaim, and that GitTarget must write
	// the repository it syncs, on the branch its targetRevision names.
	// +optional
	Source *ArgoCDClaimSource `json:"source,omitempty"`

	// Operation is the sync GitOps Reverser started on the Application: the one in
	// flight, or the last one. Absent until the first.
	// +optional
	Operation *ApplyOperation `json:"operation,omitempty"`

	// RepairPending is true while a Repair sync is pending: a sync ran a commit
	// older than an edit made through the API and reverted it on the cluster. Git
	// holds the edit, and the cluster runs the older value until the next sync.
	// +optional
	RepairPending bool `json:"repairPending,omitempty"`

	// PreviousSuspendState is how the Application stood before this ArgoCDClaim
	// first switched its automated sync off, and what handing it back restores:
	// NotSuspended (automated sync on) or Suspended (off). It is cleared once the
	// Application is handed back. Absent, handing it back leaves what the
	// Application's own spec says. A switch that an ApplicationSet or another
	// field manager owns keeps that owner's value either way.
	// +optional
	PreviousSuspendState SuspendState `json:"previousSuspendState,omitempty"`
}

// ArgoCDClaimSource is the source an Application syncs.
type ArgoCDClaimSource struct {
	// RepoURL is the Application's spec.source.repoURL.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`

	// TargetRevision is the Application's spec.source.targetRevision, as written:
	// a branch name, or refs/heads/ and the name. Empty means Argo CD's HEAD.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Control",type=string,JSONPath=`.status.conditions[?(@.type=="InControl")].reason`
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.applicationRef.name`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.status.operation.phase`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.status.operation.commit`,priority=1
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.kubeProviderRef.name`,priority=1
// +kubebuilder:printcolumn:name="RepairPending",type=boolean,JSONPath=`.status.repairPending`,priority=1
// +kubebuilder:printcolumn:name="PreviousSuspendState",type=string,JSONPath=`.status.previousSuspendState`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ArgoCDClaim lets GitOps Reverser decide when an Argo CD Application syncs.
// The Application's automated sync stays off, and GitOps Reverser starts each
// sync, always after the edits pending on the branch are published. It starts
// one:
//
//   - after a commit that GitOps Reverser did not push;
//   - after a refused edit, on a GitTarget with spec.onRefusal "Reapply";
//   - after a sync reverted an edit made through the API;
//   - after objects were created through the API in its folders;
//   - once at startup, and when a GitTarget first names this ArgoCDClaim;
//   - once spec.catchUpAfter passed since a push of GitOps Reverser's own;
//   - after a CommitRequest with spec.catchUp "AfterPush" pushed a commit.
//
// GitTargets name it with spec.applierClaimRef.
type ArgoCDClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ArgoCDClaim.
	// +required
	Spec ArgoCDClaimSpec `json:"spec"`

	// status defines the observed state of ArgoCDClaim.
	// +optional
	Status ArgoCDClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ArgoCDClaimList contains a list of ArgoCDClaim.
type ArgoCDClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ArgoCDClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArgoCDClaim{}, &ArgoCDClaimList{})
}

// DefaultArgoCDCatchUpAfter is an ArgoCDClaim's spec.catchUpAfter when it is omitted: off.
const DefaultArgoCDCatchUpAfter = 0

var _ ApplierClaim = (*ArgoCDClaim)(nil)

// ClaimKind is ArgoCDClaim.
func (t *ArgoCDClaim) ClaimKind() string { return ApplierClaimKindArgoCD }

// KubeProviderKey is spec.kubeProviderRef, its namespace defaulted to the applier claim's own.
func (t *ArgoCDClaim) KubeProviderKey() types.NamespacedName {
	return kubeProviderKey(t.Spec.KubeProviderRef, t.Namespace)
}

// ApplierKind is Application.
func (t *ArgoCDClaim) ApplierKind() string { return ApplierKindApplication }

// Applier is spec.applicationRef, its namespace defaulted to the applier claim's own.
func (t *ArgoCDClaim) Applier() types.NamespacedName {
	return types.NamespacedName{
		Namespace: defaultedNamespace(t.Spec.ApplicationRef.Namespace, t.Namespace),
		Name:      t.Spec.ApplicationRef.Name,
	}
}

// SourceBinding is status.source's repoURL, and the branch its targetRevision names ("HEAD" for an
// empty one, which follows no branch GitOps Reverser writes).
func (t *ArgoCDClaim) SourceBinding() (string, string, bool) {
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
func (t *ArgoCDClaim) SharedSource() (types.NamespacedName, bool) {
	return types.NamespacedName{}, false
}

// CatchUpAfter is spec.catchUpAfter, or DefaultArgoCDCatchUpAfter when it is omitted.
func (t *ArgoCDClaim) CatchUpAfter() time.Duration {
	return durationOr(t.Spec.CatchUpAfter, DefaultArgoCDCatchUpAfter)
}

// Operation is status.operation.
func (t *ArgoCDClaim) Operation() *ApplyOperation { return t.Status.Operation }

// RepairPending is status.repairPending.
func (t *ArgoCDClaim) RepairPending() bool { return t.Status.RepairPending }

// SetOperation replaces status.operation and status.repairPending.
func (t *ArgoCDClaim) SetOperation(operation *ApplyOperation, repairPending bool) {
	t.Status.Operation, t.Status.RepairPending = operation, repairPending
}

// PreviousSuspendState is status.previousSuspendState.
func (t *ArgoCDClaim) PreviousSuspendState() SuspendState { return t.Status.PreviousSuspendState }

// SetPreviousSuspendState replaces status.previousSuspendState.
func (t *ArgoCDClaim) SetPreviousSuspendState(state SuspendState) {
	t.Status.PreviousSuspendState = state
}
