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

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// TestDeriveSyncedCondition covers the §3.3 data-plane derivation for a Ready GitTarget: each
// phase, the no-flap-on-re-anchor guarantee, and the case precedence (Degraded > Initializing >
// Synced). The summary is the serviceability roll-up, so the inputs mirror what
// MaterializationSummaryForGitTarget produces.
func TestDeriveSyncedCondition(t *testing.T) {
	tests := []struct {
		name       string
		sum        watch.GitTargetMaterializationSummary
		wantPhase  string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "no claims is trivially synced",
			sum:        watch.GitTargetMaterializationSummary{},
			wantPhase:  GitTargetPhaseSynced,
			wantStatus: metav1.ConditionTrue,
			wantReason: GitTargetSyncedReasonOK,
		},
		{
			name:       "all claimed types serviceable",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 3, Synced: 3},
			wantPhase:  GitTargetPhaseSynced,
			wantStatus: metav1.ConditionTrue,
			wantReason: GitTargetSyncedReasonOK,
		},
		{
			name:       "first checkpoint still building",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 2, Synced: 1, Pending: 1},
			wantPhase:  GitTargetPhaseInitializing,
			wantStatus: metav1.ConditionFalse,
			wantReason: GitTargetSyncedReasonInitializing,
		},
		{
			name:       "a claimed type is not followable",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 2, Synced: 1, NotFollowable: 1},
			wantPhase:  GitTargetPhaseDegraded,
			wantStatus: metav1.ConditionFalse,
			wantReason: GitTargetSyncedReasonNotFollowable,
		},
		{
			name:       "a first sync is failing with no checkpoint",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 1, Failing: 1, FailingNoCheckpoint: 1},
			wantPhase:  GitTargetPhaseDegraded,
			wantStatus: metav1.ConditionFalse,
			wantReason: GitTargetSyncedReasonSyncFailing,
		},
		{
			// The serviceability fix: a Resyncing/Failing-with-prior type still counts as Synced,
			// so a periodic re-anchor (Synced→Resyncing→Synced) keeps the condition True and never
			// flaps. Modeled here as a serviceable type that is also Failing (a re-anchor that
			// errored): still serviceable, still Synced.
			name:       "re-anchor failing-with-checkpoint does not flap Synced",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 1, Synced: 1, Failing: 1},
			wantPhase:  GitTargetPhaseSynced,
			wantStatus: metav1.ConditionTrue,
			wantReason: GitTargetSyncedReasonOK,
		},
		{
			// Precedence: not-followable outranks an in-flight first sync.
			name:       "not-followable outranks pending",
			sum:        watch.GitTargetMaterializationSummary{Claimed: 3, Synced: 1, Pending: 1, NotFollowable: 1},
			wantPhase:  GitTargetPhaseDegraded,
			wantStatus: metav1.ConditionFalse,
			wantReason: GitTargetSyncedReasonNotFollowable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := deriveSyncedCondition(tt.sum)
			assert.Equal(t, tt.wantPhase, d.Phase)
			assert.Equal(t, tt.wantStatus, d.Status)
			assert.Equal(t, tt.wantReason, d.Reason)
			assert.NotEmpty(t, d.Message)
		})
	}
}

// TestApplySyncedConditionAndPhase_SetsConditionAndPhase proves the apply step writes both the
// Synced condition and the informational phase onto the GitTarget for a Ready (serviceable)
// summary.
func TestApplySyncedConditionAndPhase_SetsConditionAndPhase(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha1.GitTarget{}

	r.applySyncedConditionAndPhase(target, watch.GitTargetMaterializationSummary{Claimed: 1, Synced: 1})

	require.Equal(t, GitTargetPhaseSynced, target.Status.Phase)
	require.True(t, isConditionTrue(target.Status.Conditions, GitTargetConditionSynced))
}

// TestSetBlockedDataPlane_MarksUnknownAndPending proves that when a control-plane gate blocks the
// reconcile, the data-plane axis is left honest: Synced=Unknown and phase=Pending, never a stale
// True/False.
func TestSetBlockedDataPlane_MarksUnknownAndPending(t *testing.T) {
	r := &GitTargetReconciler{}
	target := &configbutleraiv1alpha1.GitTarget{}

	r.setBlockedDataPlane(target)

	require.Equal(t, GitTargetPhasePending, target.Status.Phase)
	var synced *metav1.Condition
	for i := range target.Status.Conditions {
		if target.Status.Conditions[i].Type == GitTargetConditionSynced {
			synced = &target.Status.Conditions[i]
		}
	}
	require.NotNil(t, synced)
	assert.Equal(t, metav1.ConditionUnknown, synced.Status)
	assert.Equal(t, GitTargetReasonBlocked, synced.Reason)
}
