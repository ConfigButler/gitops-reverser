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

func TestDeriveGitTargetDataPlaneStatus(t *testing.T) {
	tests := []struct {
		name              string
		streams           watch.StreamSummary
		folder            watch.FolderAcceptanceStatus
		wantPhase         string
		wantReady         metav1.ConditionStatus
		wantReconciling   metav1.ConditionStatus
		wantStalled       metav1.ConditionStatus
		wantFolder        metav1.ConditionStatus
		wantStreams       metav1.ConditionStatus
		wantStalledReason string
	}{
		{
			name: "all streams running and folder accepted",
			streams: watch.StreamSummary{
				Total: 3, Ready: 3, Reason: watch.StreamReasonAllStreamsReady, Message: "3/3 streams running",
			},
			folder:          watch.FolderAcceptanceStatus{Accepted: true},
			wantPhase:       GitTargetPhaseSynced,
			wantReady:       metav1.ConditionTrue,
			wantReconciling: metav1.ConditionFalse,
			wantStalled:     metav1.ConditionFalse,
			wantFolder:      metav1.ConditionTrue,
			wantStreams:     metav1.ConditionTrue,
		},
		{
			name: "stream replaying",
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Replaying: 1, Reason: watch.StreamReasonReplaying,
				Message: "1/2 streams running; 1 replaying",
			},
			folder:          watch.FolderAcceptanceStatus{Accepted: true},
			wantPhase:       GitTargetPhaseInitializing,
			wantReady:       metav1.ConditionFalse,
			wantReconciling: metav1.ConditionTrue,
			wantStalled:     metav1.ConditionFalse,
			wantFolder:      metav1.ConditionTrue,
			wantStreams:     metav1.ConditionFalse,
		},
		{
			name: "stream blocked",
			streams: watch.StreamSummary{
				Total: 2, Ready: 1, Blocked: 1, Reason: watch.StreamReasonWatchError,
				Message: "1/2 streams running; 1 blocked",
			},
			folder:            watch.FolderAcceptanceStatus{Accepted: true},
			wantPhase:         GitTargetPhaseDegraded,
			wantReady:         metav1.ConditionFalse,
			wantReconciling:   metav1.ConditionFalse,
			wantStalled:       metav1.ConditionTrue,
			wantFolder:        metav1.ConditionTrue,
			wantStreams:       metav1.ConditionFalse,
			wantStalledReason: watch.StreamReasonWatchError,
		},
		{
			name: "folder refused",
			streams: watch.StreamSummary{
				Total: 1, Ready: 1, Reason: watch.StreamReasonAllStreamsReady, Message: "1/1 streams running",
			},
			folder: watch.FolderAcceptanceStatus{
				Accepted: false,
				Reason:   GitTargetReasonUnsupportedContent,
				Message:  "git folder refused at kustomization.yaml: uses patches",
			},
			wantPhase:         GitTargetPhaseDegraded,
			wantReady:         metav1.ConditionFalse,
			wantReconciling:   metav1.ConditionFalse,
			wantStalled:       metav1.ConditionTrue,
			wantFolder:        metav1.ConditionFalse,
			wantStreams:       metav1.ConditionTrue,
			wantStalledReason: GitTargetReasonUnsupportedContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := deriveGitTargetDataPlaneStatus(tt.streams, tt.folder)
			assert.Equal(t, tt.wantPhase, d.Phase)
			assert.Equal(t, tt.wantReady, d.ReadyStatus)
			assert.Equal(t, tt.wantReconciling, d.ReconcilingStatus)
			assert.Equal(t, tt.wantStalled, d.StalledStatus)
			assert.Equal(t, tt.wantFolder, d.FolderStatus)
			assert.Equal(t, tt.wantStreams, d.StreamsStatus)
			if tt.wantStalledReason != "" {
				assert.Equal(t, tt.wantStalledReason, d.StalledReason)
			}
			assert.NotEmpty(t, d.ReadyMessage)
		})
	}
}

func TestApplyDataPlaneConditionsAndPhase_SetsKstatusTrio(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha2.GitTarget{}

	r.applyDataPlaneConditionsAndPhase(target, watch.StreamSummary{
		Total: 1, Ready: 1, Reason: watch.StreamReasonAllStreamsReady, Message: "1/1 streams running",
	}, watch.FolderAcceptanceStatus{Accepted: true})

	require.Equal(t, GitTargetPhaseSynced, target.Status.Phase)
	require.True(t, isConditionTrue(target.Status.Conditions, GitTargetConditionReady))
	require.True(t, isConditionTrue(target.Status.Conditions, GitTargetConditionStreamsRunning))
	require.True(t, isConditionTrue(target.Status.Conditions, GitTargetConditionFolderAccepted))
	require.False(t, isConditionTrue(target.Status.Conditions, GitTargetConditionReconciling))
	require.False(t, isConditionTrue(target.Status.Conditions, GitTargetConditionStalled))
}

func TestSetBlockedDataPlane_MarksUnknownAndPending(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha2.GitTarget{}

	r.setBlockedDataPlane(target)

	require.Equal(t, GitTargetPhasePending, target.Status.Phase)
	streamsRunning := conditionByType(target.Status.Conditions, GitTargetConditionStreamsRunning)
	require.NotNil(t, streamsRunning)
	assert.Equal(t, metav1.ConditionUnknown, streamsRunning.Status)
	assert.Equal(t, GitTargetStreamsRunningReasonNotReady, streamsRunning.Reason)
}
