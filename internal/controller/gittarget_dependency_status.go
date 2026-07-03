// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

type gitTargetReadyStatus struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

func gitTargetReadyCondition(target configbutleraiv1alpha3.GitTarget) gitTargetReadyStatus {
	if ready := conditionByType(target.Status.Conditions, GitTargetConditionReady); ready != nil {
		if ready.Status == metav1.ConditionTrue {
			return gitTargetReadyStatus{
				Status:  metav1.ConditionTrue,
				Reason:  GitTargetReasonOK,
				Message: fmt.Sprintf("GitTarget %s/%s is ready", target.Namespace, target.Name),
			}
		}
		if stalled := conditionByType(target.Status.Conditions, GitTargetConditionStalled); stalled != nil &&
			stalled.Status == metav1.ConditionTrue {
			reason, message := conditionReasonMessage(stalled, ReasonStalled, "GitTarget is stalled")
			return gitTargetReadyStatus{Status: metav1.ConditionFalse, Reason: reason, Message: message}
		}
		reason, message := conditionReasonMessage(ready, ReasonProgressing, "GitTarget is not ready yet")
		return gitTargetReadyStatus{Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	if reconciling := conditionByType(target.Status.Conditions, GitTargetConditionReconciling); reconciling != nil &&
		reconciling.Status == metav1.ConditionTrue {
		reason, message := conditionReasonMessage(reconciling, ReasonProgressing, "GitTarget is reconciling")
		return gitTargetReadyStatus{Status: metav1.ConditionFalse, Reason: reason, Message: message}
	}
	return gitTargetReadyStatus{
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonProgressing,
		Message: fmt.Sprintf("Waiting for GitTarget %s/%s to publish Ready", target.Namespace, target.Name),
	}
}
