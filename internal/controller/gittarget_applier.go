// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// GitTargetConditionApplierConfigured reports whether spec.applier names an object GitOps Reverser
// can ask to apply: it exists in the source cluster, applies this target's repository and branch,
// and may be patched. Present only while spec.applier is set.
const GitTargetConditionApplierConfigured = "ApplierConfigured"

// applierCheckTimeout bounds the ApplierConfigured check: two GETs and two access reviews against
// the source cluster. Its client has no request timeout, because it also carries the watches, and
// this controller runs one reconcile worker, so a source API server that accepts the connection and
// never answers would otherwise stop every GitTarget's reconcile, not only this one's.
const applierCheckTimeout = 10 * time.Second

// ApplyRequestLedger is what the GitTarget controller reads to publish status.applier: the last
// apply request delivered for a GitTarget. The applier dispatcher implements it.
type ApplyRequestLedger interface {
	LastRequest(ref types.ResourceReference) (configbutleraiv1alpha3.GitTargetApplierStatus, bool)
}

// observeApplier publishes status.applier and the ApplierConfigured condition, and returns the
// condition for the readiness trio. present is false when the target names no applier, in which
// case both are removed.
//
// The check reads the applier on this reconcile rather than watching it. Flux may not be installed
// in a source cluster at all, and an informer on a kind that is not served would be a failure to
// report rather than a check that answers "not found"; the cadence is the target's own.
func (r *GitTargetReconciler) observeApplier(
	ctx context.Context,
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	gitProvider *configbutleraiv1alpha3.GitProvider,
) (conditionValue, bool) {
	if target.Spec.Applier == nil {
		st.remove(GitTargetConditionApplierConfigured)
		target.Status.Applier = nil
		return conditionValue{}, false
	}

	if r.ApplyRequests != nil {
		if last, ok := r.ApplyRequests.LastRequest(types.NewResourceReference(target.Name, target.Namespace)); ok {
			target.Status.Applier = &last
		}
	}

	value := r.applierVerdict(ctx, target, gitProvider)
	st.setValue(GitTargetConditionApplierConfigured, value)
	return value, true
}

// applierVerdict checks the applier in the source cluster. A check that could not be made keeps the
// verdict already published, since a blip reading the source cluster says nothing about the
// applier; with nothing published it is Unknown.
func (r *GitTargetReconciler) applierVerdict(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	gitProvider *configbutleraiv1alpha3.GitProvider,
) conditionValue {
	ref := applierReference(target)
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	if r.ApplierClients == nil || gitProvider == nil {
		return conditionValue{
			Status:  metav1.ConditionUnknown,
			Reason:  GitTargetReasonNotChecked,
			Message: fmt.Sprintf("Kustomization %s has not been checked: no source-cluster client is wired", ref),
		}
	}
	client, err := r.ApplierClients(ctx, target.SourceCluster())
	if err != nil {
		return keptOrUnknown(target,
			fmt.Sprintf("Cannot reach the source cluster to check Kustomization %s: %v", ref, err))
	}
	newFlux := r.newFlux
	if newFlux == nil {
		newFlux = applier.NewFlux
	}
	verdict := newFlux(client).Validate(ctx, ref,
		applier.Binding{RepositoryURL: gitProvider.Spec.URL, Branch: target.Spec.Branch})
	switch {
	case verdict.OK:
		return conditionValue{
			Status:  metav1.ConditionTrue,
			Reason:  ReasonSucceeded,
			Message: fmt.Sprintf("Kustomization %s applies this repository and branch, and can be asked to apply", ref),
		}
	case verdict.Err != nil:
		return keptOrUnknown(target, verdict.Message)
	default:
		return conditionValue{Status: metav1.ConditionFalse, Reason: verdict.Reason, Message: verdict.Message}
	}
}

// keptOrUnknown is the ApplierConfigured value a check that could not be made publishes.
func keptOrUnknown(target *configbutleraiv1alpha3.GitTarget, message string) conditionValue {
	for _, cond := range target.Status.Conditions {
		if cond.Type == GitTargetConditionApplierConfigured {
			return conditionValue{Status: cond.Status, Reason: cond.Reason, Message: cond.Message}
		}
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonProgressing, Message: message}
}

// applierReference is spec.applier.kustomizationRef, its namespace defaulting to the GitTarget's.
func applierReference(target *configbutleraiv1alpha3.GitTarget) k8stypes.NamespacedName {
	ref := target.Spec.Applier.KustomizationRef
	namespace := ref.Namespace
	if namespace == "" {
		namespace = target.Namespace
	}
	return k8stypes.NamespacedName{Namespace: namespace, Name: ref.Name}
}

// applierReadiness contributes ApplierConfigured to the trio. A Kustomization that applies another
// repository, or that the operator may not patch, needs a human; one that is missing may be on its
// way (Flux objects are often created after the GitTarget), and an unknown verdict is still
// settling.
func applierReadiness(rd *readiness, value conditionValue, present bool) {
	if !present {
		return
	}
	rd.stalledIf(value.Status == metav1.ConditionFalse && value.Reason != applier.ReasonApplierNotFound,
		value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionFalse, metav1.ConditionFalse, value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionUnknown, metav1.ConditionUnknown, value.Reason, value.Message)
}
