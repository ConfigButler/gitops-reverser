// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
)

// An ArgoCDTrigger holds its Application's automated sync off, as a FluxTrigger holds its
// Kustomization suspended (fluxtrigger_hold.go). The branch workers only start syncs
// (internal/applier/argocd.go); the reconciler does the lifecycle:
//
//   - Adoption: a Configured trigger applies spec.syncPolicy.automated.enabled: false under GitOps
//     Reverser's field manager, with a Normal Event. Nothing else of automated changes, so prune
//     keeps applying to the syncs GitOps Reverser starts.
//   - Switched on elsewhere: a held Application found with automated sync on (Argo CD's UI,
//     `kubectl edit`) is switched off again, every time, with a Normal Event. A sync Argo CD
//     started meanwhile is not stopped: a request waits for it to end, and never replaces it.
//   - Hand-back: the finalizer gives the switch up, so the Application is as its own spec says
//     again.
//
// The switch of an Application an ApplicationSet generated, or whose switch another field manager
// applies, is not the trigger's (applier.AutomatedSync.Owner): the trigger never writes it and
// carries no finalizer for it. Configured refuses such an Application while its automated sync is
// on, and the trigger is Held while the owner keeps it off.

// Held reasons of an ArgoCDTrigger.
const (
	// ReasonAutomatedSyncOff is Held=True: the Application has automated sync off, held off by
	// GitOps Reverser, or kept off by the switch's owner (the message says which).
	ReasonAutomatedSyncOff = "AutomatedSyncOff"
	// ReasonAutomatedSyncOn is Held=False: automated sync is still on after GitOps Reverser switched
	// it off, which is an Argo CD before 3.1, whose API server drops spec.syncPolicy.automated.enabled.
	ReasonAutomatedSyncOn = "AutomatedSyncOn"
)

// EventReasonAutomatedSyncSwitchedOn is a held Application found with automated sync switched on by
// somebody else, and switched off again.
const EventReasonAutomatedSyncSwitchedOn = "AutomatedSyncSwitchedOn"

// reconcileArgoCD is Reconcile for an ArgoCDTrigger: one being deleted, or that no eligible
// GitTarget references any more, hands its Application's automated sync back; any other holds it
// off while it is Configured.
func (r *ApplyTriggerReconciler) reconcileArgoCD(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.ArgoCDTrigger,
) (ctrl.Result, error) {
	if !trigger.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(trigger, configbutleraiv1alpha3.ArgoCDTriggerFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.retire(ctx, st, trigger, nil)
	}
	check := r.configuredVerdict(ctx, trigger)
	publishCheck(st, trigger, check)
	if check.retiring(trigger) {
		return r.retire(ctx, st, trigger, &check)
	}
	held, err := r.holdArgoCD(ctx, st, trigger, check)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.setValue(ApplyTriggerConditionHeld, held)
	return r.commitTrigger(ctx, st, r.triggerReadiness(trigger, check.configured, &held), &held,
		heldTriggerSteadyInterval)
}

// holdArgoCD returns an ArgoCDTrigger's Held condition. A Configured trigger holds its
// Application's automated sync off. One that is not Configured holds nothing, and one in line
// behind another holder lets go of a finalizer it carries, handing nothing back.
func (r *ApplyTriggerReconciler) holdArgoCD(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.ArgoCDTrigger, check triggerCheck,
) (conditionValue, error) {
	configured := check.configured
	switch {
	case configured.Status == metav1.ConditionTrue:
		return r.holdAutomatedSync(ctx, st, trigger)
	case configured.Reason == applier.ReasonNotReferenced:
		return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
			Message: "nothing to hold: no GitTarget names this trigger"}, nil
	case configured.Reason == applier.ReasonHeldByAnotherTrigger && applier.Holding(trigger):
		// The holder took it, or two adopted at once: the switch is held under the same field
		// manager either way, so there is nothing of this trigger's to hand back.
		if err := r.setFinalizer(ctx, st, trigger, false); err != nil {
			return conditionValue{}, err
		}
	case applier.Holding(trigger):
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
			"Application %s is left with its automated sync held off under GitOps Reverser's field manager, as "+
				"this trigger last held it: it is held again once the trigger is Configured, and handed back "+
				"once no eligible GitTarget references the trigger or the trigger is deleted", trigger.Applier())}, nil
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
		Message: "the Application's automated sync is held off once the trigger is Configured"}, nil
}

// holdAutomatedSync holds a Configured ArgoCDTrigger's Application's automated sync off, with the
// trigger's finalizer put in place before the first write, and returns the Held condition. A switch
// somebody else owns is left to them: Configured is True only while they keep it off.
func (r *ApplyTriggerReconciler) holdAutomatedSync(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.ArgoCDTrigger,
) (conditionValue, error) {
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	ref := trigger.Applier()
	argo, err := r.argoClient(ctx, trigger)
	if err != nil {
		return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld,
			fmt.Sprintf("cannot reach the cluster of Application %s: %v", ref, err)), nil
	}
	state, err := argo.AutomatedSync(ctx, ref)
	if err != nil {
		return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld,
			fmt.Sprintf("cannot read Application %s: %v", ref, err)), nil
	}
	if state.Owner != "" {
		// A share of the switch this trigger took before the owner applied it is given up on
		// hand-back, by the finalizer it carries; nothing is written meanwhile.
		if state.On {
			// Switched on since Configured read it: the next reconcile refuses it.
			return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonNotConfigured, Message: fmt.Sprintf(
				"Application %s has automated sync on, switched by %s", ref, state.Owner)}, nil
		}
		return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonAutomatedSyncOff, Message: fmt.Sprintf(
			"Application %s has automated sync kept off by %s, which owns the switch; GitOps Reverser starts "+
				"every sync", ref, state.Owner)}, nil
	}
	if err := r.setFinalizer(ctx, st, trigger, true); err != nil {
		return conditionValue{}, err
	}
	if !state.Held || state.On {
		stillOn, err := argo.SwitchOff(ctx, ref)
		switch {
		case err != nil:
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, err.Error()), nil
		case stillOn:
			return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonAutomatedSyncOn, Message: fmt.Sprintf(
				"Application %s still has automated sync on after GitOps Reverser switched it off: Argo CD before "+
					"3.1 drops spec.syncPolicy.automated.enabled; upgrade Argo CD, or remove spec.syncPolicy.automated "+
					"(its syncs then do not prune)", ref)}, nil
		}
		switch {
		case !heldBefore(trigger):
			r.event(trigger, corev1.EventTypeNormal, EventReasonAdopted,
				"switched automated sync of Application %s off: from now on GitOps Reverser starts every sync", ref)
		case state.On:
			r.event(trigger, corev1.EventTypeNormal, EventReasonAutomatedSyncSwitchedOn,
				"automated sync of Application %s was switched on outside GitOps Reverser, which switched it off "+
					"again; a sync Argo CD started meanwhile was not checked against the branch", ref)
		}
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonAutomatedSyncOff, Message: fmt.Sprintf(
		"Application %s has automated sync held off by GitOps Reverser, which starts every sync", ref)}, nil
}

// releaseAutomatedSync takes an ArgoCDTrigger's hand-back one step: it gives the Application's
// automated sync switch up, unless a successor (nextInLine) takes it over, for which it is left held
// off: released, Argo CD would sync on its own at once, before the successor could switch it off
// again.
func (r *ApplyTriggerReconciler) releaseAutomatedSync(
	ctx context.Context, trigger *configbutleraiv1alpha3.ArgoCDTrigger, successor configbutleraiv1alpha3.ApplyTrigger,
) *pendingRelease {
	ref := trigger.Applier()
	if successor != nil {
		r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
			"left automated sync of Application %s held off for %s %s/%s, which holds it next", ref,
			successor.TriggerKind(), successor.GetNamespace(), successor.GetName())
		return nil
	}
	dynamicClient, err := r.releaseClient(ctx, trigger)
	if err != nil {
		return &pendingRelease{
			err: err, left: fmt.Sprintf("its cluster cannot be reached (%v), so it is left as it was", err),
		}
	}
	if dynamicClient == nil {
		return nil
	}
	on, err := applier.NewArgoCD(dynamicClient).ReleaseAutomatedSync(ctx, ref)
	switch {
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return nil
	case err != nil:
		return r.releaseFailed(trigger, err, fmt.Sprintf("giving its automated sync switch up failed (%v); %s",
			err, leftHeld(trigger)))
	}
	state := "it stays manual: its own spec has automated sync off"
	if on {
		state = "its automated sync is on again, and it syncs on its own"
	}
	r.event(trigger, corev1.EventTypeNormal, EventReasonReleased, "handed Application %s back to Argo CD: %s",
		ref, state)
	return nil
}

// argoClient is the Argo CD client of the trigger's cluster.
func (r *ApplyTriggerReconciler) argoClient(
	ctx context.Context, trigger *configbutleraiv1alpha3.ArgoCDTrigger,
) (*applier.ArgoCD, error) {
	dynamicClient, err := r.ApplierClients(ctx, trigger.KubeProviderKey().String())
	if err != nil {
		return nil, err
	}
	return applier.NewArgoCD(dynamicClient), nil
}
