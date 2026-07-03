// SPDX-License-Identifier: Apache-2.0

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func upsertCondition(
	conditions []metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
	observedGeneration int64,
) []metav1.Condition {
	now := metav1.Now()
	next := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: now,
	}

	var existing *metav1.Condition
	result := make([]metav1.Condition, 0, len(conditions))
	for i := range conditions {
		cond := conditions[i]
		if cond.Type == conditionType {
			if existing == nil {
				existing = &cond
			}
			continue
		}
		result = append(result, cond)
	}

	if existing != nil && existing.Status == next.Status && !existing.LastTransitionTime.IsZero() {
		next.LastTransitionTime = existing.LastTransitionTime
	}

	result = append(result, next)
	return result
}

func conditionByType(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := conditionByType(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}
