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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

func TestGitTargetKstatusContract(t *testing.T) {
	tests := []struct {
		name       string
		conds      []map[string]interface{}
		wantStatus kstatus.Status
		wantMsg    string
	}{
		{
			name: "initial sync in progress",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", ReasonProgressing, "target watch replay in progress"),
				conditionMap(ConditionTypeReconciling, "True", "Replaying", "target watch replay in progress"),
				conditionMap(ConditionTypeStalled, "False", ReasonProgressing, "GitTarget is not stalled"),
			},
			wantStatus: kstatus.InProgressStatus,
		},
		{
			name: "fully mirrored",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "True", GitTargetReasonOK, "GitTarget is fully reconciled"),
				conditionMap(ConditionTypeReconciling, "False", GitTargetReasonOK, "Reconciliation complete"),
				conditionMap(ConditionTypeStalled, "False", GitTargetReasonOK, "GitTarget is not stalled"),
			},
			wantStatus: kstatus.CurrentStatus,
		},
		{
			name: "Git path refused",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", GitTargetReasonUnsupportedContent,
					"Git path refused at kustomization.yaml: uses patches"),
				conditionMap(ConditionTypeReconciling, "False", GitTargetReasonUnsupportedContent,
					"Reconciliation is stalled"),
				conditionMap(ConditionTypeStalled, "True", GitTargetReasonUnsupportedContent,
					"Git path refused at kustomization.yaml: uses patches"),
				conditionMap(ConditionTypeGitPathAccepted, "False", GitTargetReasonUnsupportedContent,
					"Git path refused at kustomization.yaml: uses patches"),
			},
			wantStatus: kstatus.FailedStatus,
			wantMsg:    "kustomization.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := kstatus.Compute(gitTargetStatusObject(tt.conds))
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantMsg != "" {
				assert.Contains(t, result.Message, tt.wantMsg)
			}
		})
	}
}

func gitTargetStatusObject(conditions []map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "configbutler.ai/v1alpha2",
		"kind":       "GitTarget",
		"metadata": map[string]interface{}{
			"name":       "target-a",
			"namespace":  "default",
			"generation": int64(7),
		},
		"status": map[string]interface{}{
			"observedGeneration": int64(7),
			"conditions":         conditions,
		},
	}}
}

func conditionMap(conditionType, status, reason, message string) map[string]interface{} {
	return map[string]interface{}{
		"type":               conditionType,
		"status":             status,
		"reason":             reason,
		"message":            message,
		"observedGeneration": int64(7),
	}
}
