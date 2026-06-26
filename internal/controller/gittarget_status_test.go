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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestDeriveStreamsReadyCondition(t *testing.T) {
	tests := []struct {
		name       string
		streams    watch.StreamSummary
		wantPhase  string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "all streams ready",
			streams: watch.StreamSummary{
				Total:   3,
				Ready:   3,
				Reason:  watch.StreamReasonAllStreamsReady,
				Message: "3/3 streams ready",
			},
			wantPhase:  GitTargetPhaseSynced,
			wantStatus: metav1.ConditionTrue,
			wantReason: watch.StreamReasonAllStreamsReady,
		},
		{
			name: "stream replaying",
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Replaying: 1, Reason: watch.StreamReasonReplaying,
				Message: "1/2 streams ready; 1 replaying",
			},
			wantPhase:  GitTargetPhaseInitializing,
			wantStatus: metav1.ConditionFalse,
			wantReason: watch.StreamReasonReplaying,
		},
		{
			name: "stream blocked",
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Blocked: 1, Reason: watch.StreamReasonWatchError,
				Message: "1/2 streams ready; 1 blocked",
			},
			wantPhase:  GitTargetPhaseDegraded,
			wantStatus: metav1.ConditionFalse,
			wantReason: watch.StreamReasonWatchError,
		},
		{
			name: "no resolved types",
			streams: watch.StreamSummary{
				Reason:  watch.StreamReasonNoResolvedTypes,
				Message: "0/0 streams ready; no resolved resource types",
			},
			wantPhase:  GitTargetPhaseInitializing,
			wantStatus: metav1.ConditionFalse,
			wantReason: watch.StreamReasonNoResolvedTypes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := deriveStreamsReadyCondition(tt.streams)
			assert.Equal(t, tt.wantPhase, d.Phase)
			assert.Equal(t, tt.wantStatus, d.Status)
			assert.Equal(t, tt.wantReason, d.Reason)
			assert.NotEmpty(t, d.Message)
		})
	}
}

func TestApplyStreamsReadyConditionAndPhase_SetsConditionAndPhase(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha2.GitTarget{}

	r.applyStreamsReadyConditionAndPhase(target, watch.StreamSummary{
		Total: 1, Ready: 1, Reason: watch.StreamReasonAllStreamsReady, Message: "1/1 streams ready",
	})

	require.Equal(t, GitTargetPhaseSynced, target.Status.Phase)
	require.True(t, isConditionTrue(target.Status.Conditions, GitTargetConditionStreamsReady))
}

func TestSetBlockedDataPlane_MarksUnknownAndPending(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha2.GitTarget{}

	r.setBlockedDataPlane(target)

	require.Equal(t, GitTargetPhasePending, target.Status.Phase)
	var streamsReady *metav1.Condition
	for i := range target.Status.Conditions {
		if target.Status.Conditions[i].Type == GitTargetConditionStreamsReady {
			streamsReady = &target.Status.Conditions[i]
		}
	}
	require.NotNil(t, streamsReady)
	assert.Equal(t, metav1.ConditionUnknown, streamsReady.Status)
	assert.Equal(t, GitTargetStreamsReadyReasonNotReady, streamsReady.Reason)
}
