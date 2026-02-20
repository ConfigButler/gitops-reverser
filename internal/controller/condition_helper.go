/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
