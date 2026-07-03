// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

func TestGitProviderKstatusContract(t *testing.T) {
	tests := []struct {
		name       string
		conds      []map[string]interface{}
		wantStatus kstatus.Status
		wantMsg    string
	}{
		{
			name: "repository validation in progress",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", ReasonChecking, "Validating repository connectivity"),
				conditionMap(ConditionTypeReconciling, "True", ReasonChecking, "Validating repository connectivity"),
				conditionMap(ConditionTypeStalled, "False", ReasonChecking, "Reconciliation is making progress"),
			},
			wantStatus: kstatus.InProgressStatus,
		},
		{
			name: "repository ready",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "True", ConditionTypeReady, "Repository connectivity validated"),
				conditionMap(ConditionTypeReconciling, "False", ConditionTypeReady, "Reconciliation complete"),
				conditionMap(ConditionTypeStalled, "False", ConditionTypeReady, "GitProvider is not stalled"),
			},
			wantStatus: kstatus.CurrentStatus,
		},
		{
			name: "repository validation stalled",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", ReasonSecretNotFound, "Secret 'git-credentials' not found"),
				conditionMap(ConditionTypeReconciling, "False", ReasonSecretNotFound, "Reconciliation is stalled"),
				conditionMap(ConditionTypeStalled, "True", ReasonSecretNotFound, "Secret 'git-credentials' not found"),
			},
			wantStatus: kstatus.FailedStatus,
			wantMsg:    "git-credentials",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := kstatus.Compute(gitProviderStatusObject(tt.conds))
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantMsg != "" {
				assert.Contains(t, result.Message, tt.wantMsg)
			}
		})
	}
}

func gitProviderStatusObject(conditions []map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "GitProvider",
		"metadata": map[string]interface{}{
			"name":       "provider-a",
			"namespace":  "default",
			"generation": int64(7),
		},
		"status": map[string]interface{}{
			"observedGeneration": int64(7),
			"conditions":         conditions,
		},
	}}
}
