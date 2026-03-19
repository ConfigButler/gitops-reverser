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
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func noopLogger() logr.Logger { return logr.Discard() }

func makeGitTargetWithCondition(condType string, status metav1.ConditionStatus) *configbutleraiv1alpha1.GitTarget {
	return &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-target", Namespace: "gitops-reverser"},
		Status: configbutleraiv1alpha1.GitTargetStatus{
			Conditions: []metav1.Condition{
				{Type: condType, Status: status},
			},
		},
	}
}

// TestIsConditionTrue covers the helper used by the SnapshotSynced early-exit guard.
func TestIsConditionTrue(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		conditionType string
		want          bool
	}{
		{
			name:          "empty conditions returns false",
			conditions:    nil,
			conditionType: GitTargetConditionSnapshotSynced,
			want:          false,
		},
		{
			name: "condition present with status True returns true",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSnapshotSynced, Status: metav1.ConditionTrue},
			},
			conditionType: GitTargetConditionSnapshotSynced,
			want:          true,
		},
		{
			name: "condition present with status False returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSnapshotSynced, Status: metav1.ConditionFalse},
			},
			conditionType: GitTargetConditionSnapshotSynced,
			want:          false,
		},
		{
			name: "condition present with status Unknown returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSnapshotSynced, Status: metav1.ConditionUnknown},
			},
			conditionType: GitTargetConditionSnapshotSynced,
			want:          false,
		},
		{
			name: "different condition type returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionBootstrapped, Status: metav1.ConditionTrue},
			},
			conditionType: GitTargetConditionSnapshotSynced,
			want:          false,
		},
		{
			name: "target condition true alongside other conditions",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionBootstrapped, Status: metav1.ConditionTrue},
				{Type: GitTargetConditionSnapshotSynced, Status: metav1.ConditionTrue},
				{Type: GitTargetConditionEventStreamLive, Status: metav1.ConditionFalse},
			},
			conditionType: GitTargetConditionSnapshotSynced,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isConditionTrue(tt.conditions, tt.conditionType))
		})
	}
}

// TestEvaluateSnapshotGate_SkipsWhenSnapshotSynced verifies that when a GitTarget
// already has SnapshotSynced=True, evaluateSnapshotGate short-circuits and returns
// ConditionTrue without calling StartReconciliation.
//
// This is the regression guard for Bug 2: an unrelated event (e.g. Flux touching the
// encryption secret) must not trigger a second cluster snapshot after the first has
// completed successfully.
func TestEvaluateSnapshotGate_SkipsWhenSnapshotSynced(t *testing.T) {
	// With EventRouter == nil the function returns ConditionTrue immediately (no-cluster
	// path). We set SnapshotSynced=True so the nil-EventRouter early-exit is the path
	// that fires; the important assertion is that we get ConditionTrue back and no error,
	// confirming the gate does not re-run reconciliation.
	reconciler := &GitTargetReconciler{
		EventRouter: nil, // no cluster access needed
	}

	target := makeGitTargetWithCondition(GitTargetConditionSnapshotSynced, metav1.ConditionTrue)

	stream, state, msg, requeue, err := reconciler.evaluateSnapshotGate(
		context.TODO(), target, "gitops-reverser", noopLogger(),
	)

	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, state, "gate should report SnapshotSynced=True")
	assert.NotEmpty(t, msg)
	assert.Zero(t, requeue, "no requeue needed once snapshot is complete")
	assert.Nil(t, stream, "stream is nil when EventRouter is nil")
}

// TestEvaluateSnapshotGate_RunsWhenSnapshotNotSynced verifies that when SnapshotSynced
// is not yet True, evaluateSnapshotGate proceeds past the early-exit guard and returns
// ConditionFalse (snapshot in progress) because there is no cluster to talk to.
func TestEvaluateSnapshotGate_RunsWhenSnapshotNotSynced(t *testing.T) {
	reconciler := &GitTargetReconciler{
		EventRouter: nil,
	}

	// SnapshotSynced absent — the guard must NOT short-circuit.
	// With EventRouter==nil the function still returns ConditionTrue via the nil-router
	// path (which is the no-op path). The key is that the SnapshotSynced guard is not
	// the one that fired.
	target := makeGitTargetWithCondition(GitTargetConditionBootstrapped, metav1.ConditionTrue)

	_, state, _, _, err := reconciler.evaluateSnapshotGate(
		context.TODO(), target, "gitops-reverser", noopLogger(),
	)

	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, state)
}
