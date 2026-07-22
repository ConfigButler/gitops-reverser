// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
