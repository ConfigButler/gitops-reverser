// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// What every apply trigger kind shares: the reference a GitTarget names one by, why an apply is
// asked for, and the operation a trigger's status records. The kinds themselves are in
// fluxtrigger_types.go and argocdtrigger_types.go. docs/definitions.md fixes the words: the
// APPLIER is the Flux Kustomization or Argo CD Application, and the TRIGGER is this operator's
// resource that holds it.

// The apply trigger kinds GitTarget.spec.applyTriggerRef can name.
const (
	// ApplyTriggerKindFlux is a FluxTrigger, which holds a Flux Kustomization.
	ApplyTriggerKindFlux = "FluxTrigger"
	// ApplyTriggerKindArgoCD is an ArgoCDTrigger, which holds an Argo CD Application.
	ApplyTriggerKindArgoCD = "ArgoCDTrigger"
)

// The kinds of applier a trigger holds.
const (
	// ApplierKindKustomization is a Flux Kustomization (FluxTrigger.spec.kustomizationRef).
	ApplierKindKustomization = "Kustomization"
	// ApplierKindApplication is an Argo CD Application (ArgoCDTrigger.spec.applicationRef).
	ApplierKindApplication = "Application"
)

// Why a kind and a name, and no namespace: the kind keeps each trigger its own schema, the way
// Flux's sourceRef names a kind, and the namespace is the GitTarget's own because the trigger's
// ClusterProvider admits the namespace it is in (spec.accessFrom). A GitTarget that could name a
// trigger elsewhere would drive an applier that its own namespace was never admitted to.

// ApplyTriggerReference names an apply trigger in the GitTarget's own namespace.
type ApplyTriggerReference struct {
	// Kind is the trigger's kind: FluxTrigger or ArgoCDTrigger.
	// +required
	// +kubebuilder:validation:Enum=FluxTrigger;ArgoCDTrigger
	Kind string `json:"kind"`

	// Name is the trigger's name.
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
	// ApplyReasonCatchUp keeps the applier current with GitOps Reverser's own pushes.
	ApplyReasonCatchUp ApplyReason = "CatchUp"
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

// ApplyOperation is an apply GitOps Reverser started on the trigger's applier: the one in flight,
// or the last one, once it completed. The branch worker writes it on every transition.
type ApplyOperation struct {
	// Phase is where the operation stands. Requested, Running, Settling and Unresolved are in
	// flight: while one is, an edit to an object the applier applies can still be reverted by it,
	// and the applier is asked nothing else. Succeeded and Failed have completed.
	// +required
	Phase ApplyOperationPhase `json:"phase"`

	// Reason is why the apply was asked for. With several GitTargets on the trigger, it is the
	// strongest of their reasons.
	// +optional
	Reason ApplyReason `json:"reason,omitempty"`

	// Commit is the commit the applier was asked to apply. Empty when the remote did not carry the
	// branch.
	// +optional
	Commit string `json:"commit,omitempty"`

	// Token identifies the request in the applier's own status: the reconcile.fluxcd.io/requestedAt
	// value on a Flux object, or the "gitops-reverser request" entry in an Argo CD operation's info.
	// +optional
	Token string `json:"token,omitempty"`

	// FenceToken is the reconcile.fluxcd.io/requestedAt value the suspend after the apply carried.
	// Once the Kustomization reports it handled, nothing started before the suspend is still
	// running. Flux only.
	// +optional
	FenceToken string `json:"fenceToken,omitempty"`

	// AppliedRevision is the revision the applier reports it applied once the operation completed.
	// It can differ from commit when the branch moved between the check and the apply.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`

	// RequestedAt is when the applier was asked.
	// +required
	RequestedAt metav1.Time `json:"requestedAt"`

	// FinishedAt is when the operation was seen to complete.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message says why a Failed operation failed, or why an Unresolved one is overdue.
	// +optional
	Message string `json:"message,omitempty"`
}

// DefaultApplierQuietPeriod is FluxTrigger.spec.quietPeriod when it is omitted.
const DefaultApplierQuietPeriod = 30 * time.Second

// ApplyTrigger is what every trigger kind answers, so the controllers, the branch workers and the
// dispatcher handle a FluxTrigger and an ArgoCDTrigger alike.
// +kubebuilder:object:generate=false
type ApplyTrigger interface {
	metav1.Object
	runtime.Object

	// TriggerKind is the trigger's kind (ApplyTriggerKind*).
	TriggerKind() string
	// ClusterProvider is the name of the ClusterProvider whose cluster holds the applier.
	ClusterProvider() string
	// ApplierKind is the kind of applier the trigger holds (ApplierKind*).
	ApplierKind() string
	// Applier is the object the trigger holds, its namespace defaulted to the trigger's own.
	Applier() types.NamespacedName
	// CatchesUp reports whether the applier is owed a CatchUp after GitOps Reverser's own pushes.
	CatchesUp() bool
	// EffectiveQuietPeriod is how long the branch must be quiet before a CatchUp.
	EffectiveQuietPeriod() time.Duration
	// SourceBinding is the repository and branch the applier applies, as the trigger last read
	// them, and false when it has not read them.
	SourceBinding() (repositoryURL, branch string, known bool)
	// Operation is status.operation, and SetOperation replaces it and status.repairOwed.
	Operation() *ApplyOperation
	RepairOwed() bool
	SetOperation(operation *ApplyOperation, repairOwed bool)
	// StatusConditions returns the condition set, and SetObservedGeneration stamps the generation.
	StatusConditions() *[]metav1.Condition
	SetObservedGeneration(generation int64)
}

// NewApplyTrigger returns an empty trigger of kind, or nil for a kind this release does not know.
func NewApplyTrigger(kind string) ApplyTrigger {
	switch kind {
	case ApplyTriggerKindFlux:
		return &FluxTrigger{}
	case ApplyTriggerKindArgoCD:
		return &ArgoCDTrigger{}
	default:
		return nil
	}
}

// defaultedNamespace is namespace, or fallback when it is empty.
func defaultedNamespace(namespace, fallback string) string {
	if namespace == "" {
		return fallback
	}
	return namespace
}
