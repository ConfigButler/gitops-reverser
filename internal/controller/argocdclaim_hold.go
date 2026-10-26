// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
)

// An ArgoCDClaim holds its Application's automated sync off, as a FluxClaim holds its
// Kustomization suspended (fluxclaim_hold.go). The branch workers only start syncs
// (internal/applier/argocd.go); the reconciler does the lifecycle:
//
//   - Adoption: a Configured applier claim applies spec.syncPolicy.automated.enabled: false under GitOps
//     Reverser's field manager, with a Normal Event. Nothing else of automated changes, so prune
//     keeps applying to the syncs GitOps Reverser starts.
//   - Switched on elsewhere: a held Application found with automated sync on (Argo CD's UI,
//     `kubectl edit`) is switched off again, every time, with a Normal ClaimedResourceReconciled
//     Event. A sync Argo CD started meanwhile is not stopped: a request waits for it to end, and
//     never replaces it.
//   - Hand-back: the finalizer gives the switch up, so the Application is as its own spec says
//     again.
//
// The switch of an Application an ApplicationSet generated, or whose switch another field manager
// applies, is not the applier claim's (applier.AutomatedSync.Owner): the applier claim never writes it and
// carries no finalizer for it. Configured refuses such an Application while its automated sync is
// on, and the applier claim is InControl while the owner keeps it off.

// ReasonAutomatedSyncOff is InControl=True on an ArgoCDClaim whose Application has automated sync off:
// held off by GitOps Reverser, or kept off by the switch's owner (the message says which).
const ReasonAutomatedSyncOff = "AutomatedSyncOff"

// holdArgoCD returns an ArgoCDClaim's InControl condition. A Configured applier claim holds its
// Application's automated sync off. One that is not Configured holds nothing, and one in line
// behind another holder lets go of a finalizer it carries, handing nothing back.
func (r *ApplierClaimReconciler) holdArgoCD(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.ArgoCDClaim, check claimCheck,
) (conditionValue, error) {
	configured := check.configured
	switch {
	case configured.Status == metav1.ConditionTrue:
		return r.holdAutomatedSync(ctx, st, claim)
	case configured.Reason == applier.ReasonNotReferenced:
		return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
			Message: "nothing to hold: no GitTarget names this applier claim"}, nil
	case configured.Reason == applier.ReasonHeldByAnotherClaim && applier.Holding(claim):
		// The holder took it, or two adopted at once: the switch is held under the same field
		// manager either way, so there is nothing of this applier claim's to hand back.
		if err := r.setFinalizer(ctx, st, claim, false); err != nil {
			return conditionValue{}, err
		}
	case applier.Holding(claim):
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
			"Application %s is left with its automated sync held off under GitOps Reverser's field manager, as "+
				"this applier claim last held it: it is held again once the applier claim is Configured, and handed back "+
				"once no eligible GitTarget references the applier claim or the applier claim is deleted", claim.Applier())}, nil
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
		Message: "the Application's automated sync is held off once the applier claim is Configured"}, nil
}

// holdAutomatedSync holds a Configured ArgoCDClaim's Application's automated sync off, with the
// applier claim's finalizer put in place before the first write, and returns the InControl condition.
// A switch somebody else owns is left to them: Configured is True only while they keep it off.
func (r *ApplierClaimReconciler) holdAutomatedSync(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.ArgoCDClaim,
) (conditionValue, error) {
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	ref := claim.Applier()
	argo, err := r.argoClient(ctx, claim)
	if err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl,
			fmt.Sprintf("cannot reach the cluster of Application %s: %v", ref, err)), nil
	}
	state, err := argo.AutomatedSync(ctx, ref)
	if err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl,
			fmt.Sprintf("cannot read Application %s: %v", ref, err)), nil
	}
	if state.Owner != "" {
		return ownedSwitchCondition(ref, state), nil
	}
	// A switch held already, by an applier claim that adopts, is one a hand-back kept off (adopting).
	adopt := adopting(claim, claim.Status.Operation)
	if adopt && state.Held {
		if other, ok := r.heldElsewhere(ctx, claim); !ok {
			return other, nil
		}
	}
	if err := r.setFinalizer(ctx, st, claim, true); err != nil {
		return conditionValue{}, err
	}
	if !state.Held || state.On || adopt {
		if failed, ok := r.switchOff(ctx, st, claim, argo, state, adopt); !ok {
			return failed, nil
		}
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonAutomatedSyncOff, Message: fmt.Sprintf(
		"Application %s has automated sync held off by GitOps Reverser, which starts every sync", ref)}, nil
}

// switchOff switches an ArgoCDClaim's Application's automated sync off, having recorded how it stood
// when the applier claim adopts it, and reports false with the InControl condition when it could not.
func (r *ApplierClaimReconciler) switchOff(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.ArgoCDClaim, argo *applier.ArgoCD,
	state applier.AutomatedSync, adopt bool,
) (conditionValue, bool) {
	ref := claim.Applier()
	if err := r.recordPreviousSuspendState(ctx, st, claim, state.Held && !adopt, !state.On); err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, fmt.Sprintf(
			"cannot record how Application %s stood before it is held: %v", ref, err)), false
	}
	written, err := argo.SwitchOff(ctx, ref)
	switch {
	case err != nil:
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, err.Error()), false
	case !written:
		// Configured checks this first; an Argo CD downgraded since is caught here, before a write.
		return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonArgoCDVersionNotSupported,
			Message: fmt.Sprintf("Application %s is served without spec.syncPolicy.automated.enabled (Argo CD "+
				"before 3.1), so GitOps Reverser writes nothing", ref)}, false
	}
	switch {
	case !wasInControl(claim):
		r.event(claim, corev1.EventTypeNormal, EventReasonAdopted,
			"switched automated sync of Application %s off: from now on GitOps Reverser starts every sync", ref)
	case state.On:
		recordClaimedResourceReconciled(ctx, applierKindApplication, claim)
		r.event(claim, corev1.EventTypeNormal, EventReasonClaimedResourceReconciled,
			"automated sync of Application %s was switched on outside GitOps Reverser, which switched it off "+
				"again; a sync Argo CD started meanwhile was not checked against the branch", ref)
	}
	return conditionValue{}, true
}

// ownedSwitchCondition is the InControl condition of an applier claim whose Application's switch
// somebody else owns. Nothing is written: a share of the switch the applier claim took before the owner
// did is handed back by the finalizer it carries.
func ownedSwitchCondition(ref k8stypes.NamespacedName, state applier.AutomatedSync) conditionValue {
	if state.On {
		// Switched on since Configured read it: the next reconcile refuses it.
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
			"Application %s has automated sync on, switched by %s", ref, state.Owner)}
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonAutomatedSyncOff, Message: fmt.Sprintf(
		"Application %s has automated sync kept off by %s, which owns the switch; GitOps Reverser starts "+
			"every sync", ref, state.Owner)}
}

// releaseAutomatedSync takes an ArgoCDClaim's hand-back one step: it hands the Application's
// automated sync switch back as status.previousSuspendState recorded it, or as the ApplicationSet or field
// manager that owns the switch by then set it (applier.ArgoCD.ReleaseAutomatedSync). A successor
// (nextInLine) takes it over instead, and it is left held off for it: released, Argo CD would sync on
// its own at once, before the successor could switch it off again.
func (r *ApplierClaimReconciler) releaseAutomatedSync(
	ctx context.Context, claim *configbutleraiv1alpha3.ArgoCDClaim, successor configbutleraiv1alpha3.ApplierClaim,
) *pendingRelease {
	ref := claim.Applier()
	if successor != nil {
		r.event(claim, corev1.EventTypeNormal, EventReasonReleased,
			"left automated sync of Application %s held off for %s %s/%s, which holds it next", ref,
			successor.ClaimKind(), successor.GetNamespace(), successor.GetName())
		return nil
	}
	dynamicClient, err := r.releaseClient(ctx, claim)
	if err != nil {
		return &pendingRelease{
			err: err, left: fmt.Sprintf("its cluster cannot be reached (%v), so it is left as it was", err),
		}
	}
	if dynamicClient == nil {
		return nil
	}
	var wasOn *bool
	if previous := claim.Status.PreviousSuspendState; previous != "" {
		on := previous == configbutleraiv1alpha3.SuspendStateNotSuspended
		wasOn = &on
	}
	released, err := applier.NewArgoCD(dynamicClient).ReleaseAutomatedSync(ctx, ref, wasOn)
	switch {
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return nil
	case err != nil:
		return r.releaseFailed(claim, err, fmt.Sprintf("giving its automated sync switch up failed (%v); %s",
			err, leftHeld(claim)))
	}
	state := "its automated sync is off"
	if released.On {
		state = "its automated sync is on, and it syncs on its own"
	}
	if released.Kept != "" {
		state += ", " + released.Kept + "; GitOps Reverser's field manager keeps that value until its owner changes it"
	}
	r.event(claim, corev1.EventTypeNormal, EventReasonReleased, "handed Application %s back to Argo CD: %s",
		ref, state)
	return nil
}

// argoClient is the Argo CD client of the applier claim's cluster.
func (r *ApplierClaimReconciler) argoClient(
	ctx context.Context, claim *configbutleraiv1alpha3.ArgoCDClaim,
) (*applier.ArgoCD, error) {
	dynamicClient, err := r.ApplierClients(ctx, claim.KubeProviderKey().String())
	if err != nil {
		return nil, err
	}
	return applier.NewArgoCD(dynamicClient), nil
}
