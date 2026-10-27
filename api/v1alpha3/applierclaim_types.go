// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// What every applier claim kind shares: the reference a GitTarget names one by, why an apply is
// asked for, and the operation an applier claim's status records. The kinds themselves are in
// fluxclaim_types.go and argocdclaim_types.go. docs/definitions.md fixes the words: the
// APPLIER is the Flux Kustomization or Argo CD Application, and the TRIGGER is this operator's
// resource that holds it.

// The applier claim kinds GitTarget.spec.applierClaimRef can name.
const (
	// ApplierClaimKindFlux is a FluxClaim, which holds a Flux Kustomization.
	ApplierClaimKindFlux = "FluxClaim"
	// ApplierClaimKindArgoCD is an ArgoCDClaim, which holds an Argo CD Application.
	ApplierClaimKindArgoCD = "ArgoCDClaim"
)

// The kinds of applier an applier claim holds.
const (
	// ApplierKindKustomization is a Flux Kustomization (FluxClaim.spec.kustomizationRef).
	ApplierKindKustomization = "Kustomization"
	// ApplierKindApplication is an Argo CD Application (ArgoCDClaim.spec.applicationRef).
	ApplierKindApplication = "Application"
)

// Why a kind and a name, and no namespace: the kind keeps each applier claim its own schema, the way
// Flux's sourceRef names a kind, and the namespace is the GitTarget's own because the applier claim's
// KubeProvider admits the namespace it is in. A GitTarget that could name an
// applier claim elsewhere would drive an applier that its own namespace was never admitted to.

// ApplierClaimReference names an applier claim in the GitTarget's own
// namespace.
type ApplierClaimReference struct {
	// Kind is the applier claim's kind: "FluxClaim" or "ArgoCDClaim".
	// +required
	// +kubebuilder:validation:Enum=FluxClaim;ArgoCDClaim
	Kind string `json:"kind"`

	// Name is the applier claim's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ApplyReason is why GitOps Reverser asked an applier to apply.
// +kubebuilder:validation:Enum=ForeignCommit;Refusal;Repair;CatchUp
type ApplyReason string

const (
	// ApplyReasonForeignCommit is a commit GitOps Reverser did not push.
	ApplyReasonForeignCommit ApplyReason = "ForeignCommit"
	// ApplyReasonRefusal is a refused edit on a target with spec.onRefusal: Reapply.
	ApplyReasonRefusal ApplyReason = "Refusal"
	// ApplyReasonRepair puts back an API edit that an earlier apply reverted on the cluster after
	// the edit was made. Git kept the edit; the cluster runs the older value until the repair.
	ApplyReasonRepair ApplyReason = "Repair"
	// ApplyReasonCatchUp adopts what the API created in the applier's folders, so the applier owns
	// and can later prune it, and converges the applier once at startup and when a GitTarget first
	// names its applier claim.
	ApplyReasonCatchUp ApplyReason = "CatchUp"
)

// SuspendState is whether an applier applied on its own. An Argo CD
// Application with automated sync off counts as Suspended.
// +kubebuilder:validation:Enum=Suspended;NotSuspended
type SuspendState string

const (
	// SuspendStateSuspended is a Kustomization with spec.suspend: true, or an Application with
	// automated sync off.
	SuspendStateSuspended SuspendState = "Suspended"
	// SuspendStateNotSuspended is a Kustomization that applied on its own, or an Application with
	// automated sync on.
	SuspendStateNotSuspended SuspendState = "NotSuspended"
)

// ApplyOperationPhase is where an apply operation stands.
// +kubebuilder:validation:Enum=Requested;Running;Settling;Unresolved;Succeeded;Failed
type ApplyOperationPhase string

const (
	// ApplyOperationRequested is an operation GitOps Reverser has asked the applier for.
	ApplyOperationRequested ApplyOperationPhase = "Requested"
	// ApplyOperationRunning is an operation the applier is seen working on.
	ApplyOperationRunning ApplyOperationPhase = "Running"
	// ApplyOperationSettling is a Flux operation that has applied, and whose Kustomization is not
	// yet seen suspended again.
	ApplyOperationSettling ApplyOperationPhase = "Settling"
	// ApplyOperationUnresolved is an operation that has not completed in the time it was given. It
	// is still followed, and no timer ends it.
	ApplyOperationUnresolved ApplyOperationPhase = "Unresolved"
	// ApplyOperationSucceeded is an operation that completed and applied.
	ApplyOperationSucceeded ApplyOperationPhase = "Succeeded"
	// ApplyOperationFailed is an operation that completed without applying the commit. It may have
	// applied part of it.
	ApplyOperationFailed ApplyOperationPhase = "Failed"
)

// Completed reports whether the phase ends its operation.
func (p ApplyOperationPhase) Completed() bool {
	return p == ApplyOperationSucceeded || p == ApplyOperationFailed
}

// ApplyOperation is an apply GitOps Reverser started on the applier claim's
// applier: the one in flight, or the last one, once it completed. The branch
// worker writes it on every transition.
type ApplyOperation struct {
	// Phase is where the operation stands. Requested, Running, Settling (Flux
	// only: applied, and the Kustomization not yet suspended again) and Unresolved
	// (overdue, still followed) are in flight: the applier can still revert an
	// edit to an object it applies, and is asked for nothing else. Succeeded and
	// Failed are complete; Failed may have applied part of the commit.
	// +required
	Phase ApplyOperationPhase `json:"phase"`

	// Reason is why the apply was asked for: ForeignCommit (a commit GitOps
	// Reverser did not push), Refusal (a refused edit under spec.onRefusal:
	// "Reapply"), Repair (an earlier apply reverted a newer API edit), or CatchUp
	// (adopting objects the API created, and once at startup or when a GitTarget
	// first names the claim). With several GitTargets on the claim, it is the
	// strongest of their reasons.
	// +optional
	Reason ApplyReason `json:"reason,omitempty"`

	// Commit is the commit the applier was asked to apply. Empty when the remote
	// did not carry the branch.
	// +optional
	Commit string `json:"commit,omitempty"`

	// Token identifies the request in the applier's own status: the
	// reconcile.fluxcd.io/requestedAt value on a Flux object, or the
	// "gitops-reverser request" entry in an Argo CD operation's info.
	// +optional
	Token string `json:"token,omitempty"`

	// SuspendToken is the reconcile.fluxcd.io/requestedAt value of the suspend
	// that follows the apply. Once the Kustomization reports it handled, nothing
	// started before the suspend is still running. Flux only.
	// +optional
	SuspendToken string `json:"suspendToken,omitempty"`

	// AppliedRevision is the revision the applier reports applying when the
	// operation completes. It can differ from commit when the branch moved between
	// the check and the apply.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`

	// RequestedAt is when the applier was asked.
	// +required
	RequestedAt metav1.Time `json:"requestedAt"`

	// FinishedAt is when the operation was seen to complete.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message says why a Failed operation failed, or why an Unresolved one is
	// overdue.
	// +optional
	Message string `json:"message,omitempty"`
}

// ApplierClaim is what every applier claim kind answers, so the controllers,
// the branch workers and the dispatcher handle a FluxClaim and an ArgoCDClaim
// alike.
// +kubebuilder:object:generate=false
type ApplierClaim interface {
	metav1.Object
	runtime.Object

	// ClaimKind is the applier claim's kind (ApplierClaimKind*).
	ClaimKind() string
	// KubeProviderKey is the KubeProvider whose cluster holds the applier.
	KubeProviderKey() types.NamespacedName
	// ApplierKind is the kind of applier the applier claim holds (ApplierKind*).
	ApplierKind() string
	// Applier is the object the applier claim holds, its namespace defaulted to the applier claim's own.
	Applier() types.NamespacedName
	// SourceBinding is the repository and branch the applier applies, as the applier claim last read
	// them, and false when it has not read them.
	SourceBinding() (repositoryURL, branch string, known bool)
	// SharedSource is the source object the applier reads, which other appliers may read too, and
	// false when the applier has none it shares this way or the applier claim has not read it. Appliers
	// sharing one have one operation at a time.
	SharedSource() (source types.NamespacedName, known bool)
	// CatchUpAfter is spec.catchUpAfter, or the kind's default when it is omitted: how long
	// the applier may stay behind the branch after a push of GitOps Reverser's own. Zero never
	// converges for that reason.
	CatchUpAfter() time.Duration
	// Operation is status.operation, and SetOperation replaces it and status.repairPending.
	Operation() *ApplyOperation
	RepairPending() bool
	SetOperation(operation *ApplyOperation, repairPending bool)
	// PreviousSuspendState is status.previousSuspendState, and SetPreviousSuspendState replaces it.
	PreviousSuspendState() SuspendState
	SetPreviousSuspendState(state SuspendState)
	// StatusConditions returns the condition set, and SetObservedGeneration stamps the generation.
	StatusConditions() *[]metav1.Condition
	SetObservedGeneration(generation int64)
}

// NewApplierClaim returns an empty applier claim of kind, or nil for a kind this release does not know.
func NewApplierClaim(kind string) ApplierClaim {
	switch kind {
	case ApplierClaimKindFlux:
		return &FluxClaim{}
	case ApplierClaimKindArgoCD:
		return &ArgoCDClaim{}
	default:
		return nil
	}
}

// durationOr is d, or fallback when it is omitted.
func durationOr(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil {
		return fallback
	}
	return d.Duration
}

// defaultedNamespace is namespace, or fallback when it is empty.
func defaultedNamespace(namespace, fallback string) string {
	if namespace == "" {
		return fallback
	}
	return namespace
}

// kubeProviderKey is ref, its namespace defaulted to the referrer's own, or the zero key when ref
// names nothing.
func kubeProviderKey(ref meta.NamespacedObjectReference, referrerNamespace string) types.NamespacedName {
	if ref.Name == "" {
		return types.NamespacedName{}
	}
	return types.NamespacedName{Namespace: defaultedNamespace(ref.Namespace, referrerNamespace), Name: ref.Name}
}
