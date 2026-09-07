// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func gitTargetReadyCondition(target configbutleraiv1alpha3.GitTarget) conditionValue {
	if ready := findCondition(target.Status.Conditions, GitTargetConditionReady); ready != nil {
		if ready.Status == metav1.ConditionTrue {
			return conditionValue{
				Status:  metav1.ConditionTrue,
				Reason:  GitTargetReasonOK,
				Message: fmt.Sprintf("GitTarget %s/%s is ready", target.Namespace, target.Name),
			}
		}
		if stalled := findCondition(target.Status.Conditions, GitTargetConditionStalled); stalled != nil &&
			stalled.Status == metav1.ConditionTrue {
			reason, message := conditionReasonMessage(stalled, ReasonStalled, "GitTarget is stalled")
			return conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message}
		}
		reason, message := conditionReasonMessage(ready, ReasonProgressing, "GitTarget is not ready yet")
		return conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	if reconciling := findCondition(target.Status.Conditions, GitTargetConditionReconciling); reconciling != nil &&
		reconciling.Status == metav1.ConditionTrue {
		reason, message := conditionReasonMessage(reconciling, ReasonProgressing, "GitTarget is reconciling")
		return conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	return conditionValue{
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonProgressing,
		Message: fmt.Sprintf("Waiting for GitTarget %s/%s to publish Ready", target.Namespace, target.Name),
	}
}

// gitTargetReadyProjectionChanged is the GitTarget watch predicate for the WatchRule and
// ClusterWatchRule controllers: fire on create/delete, and on an update when the SPEC changed
// (generation) OR the readiness those rules MIRROR changed.
//
// GenerationChangedPredicate alone is wrong on this edge, for the same reason it cannot carry a
// ClusterProvider change to a GitTarget: a GitTarget that heals publishes its recovery as a
// status-only update, which that predicate deliberately drops. The rule's mirrored GitTargetReady
// then stayed frozen on the failing snapshot until the rule's own periodic requeue — leaving an
// object that contradicts itself for minutes, with StreamsRunning already back to True because the
// stream-state channel told it so directly. That made the mirrored condition unusable for alerting.
//
// Only Status and Reason are compared. Both are low-cardinality and are what a printer column or an
// alert keys on, whereas the mirrored Message is free-form and moves with transient stream detail;
// including it would re-enqueue every rule of a target on churn that changes no verdict. A
// message-only drift self-corrects on the next Status or Reason move.
func gitTargetReadyProjectionChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldTarget, ok1 := e.ObjectOld.(*configbutleraiv1alpha3.GitTarget)
			newTarget, ok2 := e.ObjectNew.(*configbutleraiv1alpha3.GitTarget)
			if !ok1 || !ok2 {
				return true
			}
			if oldTarget.Generation != newTarget.Generation {
				return true
			}
			before := gitTargetReadyCondition(*oldTarget)
			after := gitTargetReadyCondition(*newTarget)
			return before.Status != after.Status || before.Reason != after.Reason
		},
	}
}
