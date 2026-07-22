// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestUpsertCondition_UpdatesInPlaceAndKeepsOrder pins the property the hand-rolled setter did not
// have: a touched condition keeps its POSITION. The old implementation filtered the target type out
// and re-appended it, so every reconcile that touched Ready shuffled the list — a diff in
// `kubectl get -o yaml` and, for a cluster mirroring its own config objects into Git, a stream of
// commits that reorder conditions and change nothing.
func TestUpsertCondition_UpdatesInPlaceAndKeepsOrder(t *testing.T) {
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
			Type:               GitTargetConditionStreamsRunning,
			Status:             metav1.ConditionTrue,
			Reason:             "AllStreamsReady",
			Message:            "1/1 streams running",
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

	require.Len(t, conditions, 2)
	require.Equal(t, GitTargetConditionReady, conditions[0].Type, "the touched condition must not migrate")
	require.Equal(t, GitTargetConditionStreamsRunning, conditions[1].Type)
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
