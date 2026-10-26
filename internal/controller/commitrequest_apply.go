// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// A CommitRequest with catchUp: AfterPush keeps being reconciled once it is Ready, to
// report in its Applied condition how the apply it asked for went (git.SaveApply). Ready is the
// save's alone and never changes here.

// crReasonApplyLost is the Applied reason for an apply the worker knows nothing of: GitOps Reverser
// restarted before it learnt how it went, or the record aged out.
const crReasonApplyLost = "Lost"

// applyLostAfter is how long, from Ready, a request may find its worker knowing nothing of its apply
// before Applied says it is lost. The worker records the apply as the request resolves, so only a
// restart, or a record past its TTL, leaves it unknown.
const applyLostAfter = time.Minute

// followsApply reports whether a finished request still reports its apply: it asked for one, its
// save ended Ready, and Applied is not settled yet.
func followsApply(cr *configbutleraiv1alpha3.CommitRequest) bool {
	return cr.Spec.CatchUp == configbutleraiv1alpha3.CatchUpAfterPush &&
		conditionIsTrue(cr.Status.Conditions, ConditionTypeReady) && !applyReported(cr)
}

// applyReported reports whether Applied is settled: True, False, or Unknown because it was lost.
func applyReported(cr *configbutleraiv1alpha3.CommitRequest) bool {
	c := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeApplied)
	return c != nil && (c.Status != metav1.ConditionUnknown || c.Reason == crReasonApplyLost)
}

// followApply reads where the request's apply stands, writes it to Applied, and comes back until it
// is settled; then the request expires as any finished one does.
func (r *CommitRequestReconciler) followApply(
	ctx context.Context,
	cr *configbutleraiv1alpha3.CommitRequest,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("CommitRequestReconciler")
	attach := git.AttachCommitRequest{
		Namespace: cr.Namespace, Name: cr.Name, UID: string(cr.UID),
		GitTargetName: cr.Spec.GitTargetRef.Name, GitTargetNamespace: cr.Namespace,
	}
	apply, known, err := r.Finalizer.LookupCommitRequestApply(ctx, attach)
	if err != nil {
		log.V(1).Info("Cannot read the CommitRequest's apply yet; will retry",
			"name", client.ObjectKeyFromObject(cr), "err", err.Error())
		return ctrl.Result{RequeueAfter: commitRequestPollInterval}, nil
	}
	if !known {
		apply = unknownApply(cr)
	}
	before := cr.Status.DeepCopy()
	setAppliedCondition(cr, apply)
	if !equality.Semantic.DeepEqual(*before, cr.Status) {
		if err := r.Status().Update(ctx, cr); err != nil {
			// A conflict reads the request again; anything else retries the same way.
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		log.Info("CommitRequest apply reported", "name", client.ObjectKeyFromObject(cr),
			"reason", apply.Reason, "appliedRevision", cr.Status.AppliedRevision, "message", apply.Message)
	}
	if applyReported(cr) {
		return r.expireFinished(ctx, cr)
	}
	return ctrl.Result{RequeueAfter: commitRequestPollInterval}, nil
}

// unknownApply is the apply of a request its worker knows nothing of: still being taken on, shortly
// after Ready, and lost after applyLostAfter.
func unknownApply(cr *configbutleraiv1alpha3.CommitRequest) git.SaveApply {
	if ready := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady); ready != nil &&
		time.Since(ready.LastTransitionTime.Time) < applyLostAfter {
		return git.SaveApply{Reason: git.SaveApplyApplying, Message: "waiting for the branch worker to take the apply"}
	}
	return git.SaveApply{Reason: crReasonApplyLost,
		Message: "GitOps Reverser does not know how the apply went: it restarted before it learnt, or kept no record"}
}

// setAppliedCondition writes apply to Applied and status.appliedRevision.
func setAppliedCondition(cr *configbutleraiv1alpha3.CommitRequest, apply git.SaveApply) {
	status := metav1.ConditionUnknown
	switch {
	case apply.Applied:
		status = metav1.ConditionTrue
		cr.Status.AppliedRevision = apply.AppliedRevision
	case apply.Done:
		status = metav1.ConditionFalse
	}
	setCommitRequestCondition(cr, ConditionTypeApplied, status, apply.Reason, apply.Message)
}
