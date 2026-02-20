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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpsertCondition_DeduplicatesAndUpdatesFields(t *testing.T) {
	oldTransition := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	conditions := []metav1.Condition{
		{
			Type:               GitTargetConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "OldReason",
			Message:            "OldMessage",
			ObservedGeneration: 1,
			LastTransitionTime: oldTransition,
		},
		{
			Type:               GitTargetConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Duplicate",
			Message:            "Duplicate",
			ObservedGeneration: 1,
			LastTransitionTime: oldTransition,
		},
	}

	conditions = upsertCondition(
		conditions,
		GitTargetConditionReady,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Updated message",
		9,
	)

	require.Len(t, conditions, 1)
	require.Equal(t, GitTargetConditionReady, conditions[0].Type)
	require.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	require.Equal(t, GitTargetReasonOK, conditions[0].Reason)
	require.Equal(t, "Updated message", conditions[0].Message)
	require.Equal(t, int64(9), conditions[0].ObservedGeneration)
	require.Equal(t, oldTransition, conditions[0].LastTransitionTime)
}

func TestUpsertCondition_ChangesTransitionTimeWhenStatusChanges(t *testing.T) {
	oldTransition := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	conditions := []metav1.Condition{
		{
			Type:               GitTargetConditionValidated,
			Status:             metav1.ConditionFalse,
			Reason:             GitTargetReasonProviderNotFound,
			Message:            "missing provider",
			ObservedGeneration: 2,
			LastTransitionTime: oldTransition,
		},
	}

	conditions = upsertCondition(
		conditions,
		GitTargetConditionValidated,
		metav1.ConditionTrue,
		GitTargetReasonOK,
		"Provider and branch validation passed",
		3,
	)

	require.Len(t, conditions, 1)
	require.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	require.NotEqual(t, oldTransition, conditions[0].LastTransitionTime)
	require.Equal(t, int64(3), conditions[0].ObservedGeneration)
}
