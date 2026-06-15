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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// TestIsConditionTrue covers the condition helper the status pipeline uses.
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
			conditionType: GitTargetConditionSynced,
			want:          false,
		},
		{
			name: "condition present with status True returns true",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSynced, Status: metav1.ConditionTrue},
			},
			conditionType: GitTargetConditionSynced,
			want:          true,
		},
		{
			name: "condition present with status False returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSynced, Status: metav1.ConditionFalse},
			},
			conditionType: GitTargetConditionSynced,
			want:          false,
		},
		{
			name: "condition present with status Unknown returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionSynced, Status: metav1.ConditionUnknown},
			},
			conditionType: GitTargetConditionSynced,
			want:          false,
		},
		{
			name: "different condition type returns false",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionValidated, Status: metav1.ConditionTrue},
			},
			conditionType: GitTargetConditionSynced,
			want:          false,
		},
		{
			name: "target condition true alongside other conditions",
			conditions: []metav1.Condition{
				{Type: GitTargetConditionValidated, Status: metav1.ConditionTrue},
				{Type: GitTargetConditionSynced, Status: metav1.ConditionTrue},
			},
			conditionType: GitTargetConditionSynced,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isConditionTrue(tt.conditions, tt.conditionType))
		})
	}
}

// TestCheckForConflicts_ListErrorFailsClosed verifies the topology guard never
// silently accepts a GitTarget when it cannot list peer targets. The non-overlap
// invariant is a precondition for the destructive manifest writer, so cache/API
// failures must requeue reconciliation rather than pass validation.
func TestCheckForConflicts_ListErrorFailsClosed(t *testing.T) {
	client := newGitTargetListErrorClient(t)
	reconciler := &GitTargetReconciler{Client: client}
	target := &configbutleraiv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "target-a", Namespace: "default"},
		Spec: configbutleraiv1alpha1.GitTargetSpec{
			ProviderRef: configbutleraiv1alpha1.GitProviderReference{Name: "provider-a"},
			Branch:      "main",
			Path:        "apps",
		},
	}

	conflict, _, _, _, err := reconciler.checkForConflicts(context.Background(), target, target.Namespace)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list GitTargets for conflict validation")
	assert.False(t, conflict)
}

func newGitTargetListErrorClient(t *testing.T) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				context.Context,
				ctrlclient.WithWatch,
				ctrlclient.ObjectList,
				...ctrlclient.ListOption,
			) error {
				return errors.New("simulated GitTarget list failure")
			},
		}).
		Build()
}
