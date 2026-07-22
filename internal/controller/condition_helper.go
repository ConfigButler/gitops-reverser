// SPDX-License-Identifier: Apache-2.0

package controller

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// upsertCondition sets one condition by type, preserving LastTransitionTime while Status is
// unchanged.
//
// It delegates to apimachinery's own setter rather than rebuilding the slice. The hand-rolled
// version filtered the target type out and re-appended it at the tail, so touching Ready migrated
// it to the end and every other condition shuffled up — a diff on every reconcile in
// `kubectl get -o yaml`, and, for a cluster whose own config objects are mirrored into Git by this
// operator, a stream of commits that reorder conditions and change nothing.
func upsertCondition(
	conditions []metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
	observedGeneration int64,
) []metav1.Condition {
	apimeta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
	return conditions
}

// findCondition returns the named condition, or nil.
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	return apimeta.FindStatusCondition(conditions, conditionType)
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionTrue(conditions, conditionType)
}

func conditionIsFalse(conditions []metav1.Condition, conditionType string) bool {
	return apimeta.IsStatusConditionFalse(conditions, conditionType)
}
