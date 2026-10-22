// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// TestKubeProviderKstatusContract pins what a kstatus reader — `kubectl wait`, Flux health
// checks, an Argo sync wave — makes of a KubeProvider's conditions.
//
// The case that matters most is the last one: a provider whose audit route has never carried a
// fact is Current. AuditFactsReceived is a report about the commit AUTHOR, and the object it hangs
// on mirrors perfectly without it, so folding it into Ready would tell every one of those readers
// the object is broken and block a rollout on a cluster that is working.
func TestKubeProviderKstatusContract(t *testing.T) {
	tests := []struct {
		name       string
		conds      []map[string]interface{}
		wantStatus kstatus.Status
		wantMsg    string
	}{
		{
			name: "inCluster provider validated",
			conds: []map[string]interface{}{
				conditionMap(
					ConditionTypeReady,
					"True",
					ReasonSucceeded,
					"spec.inCluster: the operator's own identity",
				),
				conditionMap(ConditionTypeReconciling, "False", ReasonSucceeded, "Reconciliation complete"),
				conditionMap(ConditionTypeStalled, "False", ReasonSucceeded, "KubeProvider is not stalled"),
				conditionMap(KubeProviderConditionValidated, "True", ReasonInCluster, "the operator's own cluster"),
			},
			wantStatus: kstatus.CurrentStatus,
		},
		{
			name: "unsafe kubeconfig stalls the provider",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", ReasonKubeConfigInvalid, "exec auth is not permitted"),
				conditionMap(ConditionTypeReconciling, "False", ReasonKubeConfigInvalid, "Reconciliation is stalled"),
				conditionMap(ConditionTypeStalled, "True", ReasonKubeConfigInvalid, "exec auth is not permitted"),
				conditionMap(KubeProviderConditionValidated, "False", ReasonKubeConfigInvalid, "exec auth"),
			},
			wantStatus: kstatus.FailedStatus,
			wantMsg:    "exec auth",
		},
		{
			// A provider another namespace's route holds is not used, though its credential is fine:
			// Validated stays True and the stall alone is what a kstatus reader sees.
			name: "an audit route held elsewhere stalls a validated provider",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", "AuditRouteConflict", "held by namespace team-b"),
				conditionMap(ConditionTypeReconciling, "False", "AuditRouteConflict", "Reconciliation is stalled"),
				conditionMap(ConditionTypeStalled, "True", "AuditRouteConflict", "held by namespace team-b"),
				conditionMap(KubeProviderConditionValidated, "True", ReasonInCluster, "the operator's own cluster"),
			},
			wantStatus: kstatus.FailedStatus,
			wantMsg:    "held by namespace team-b",
		},
		{
			name: "a route that has never carried a fact is still Current",
			conds: []map[string]interface{}{
				conditionMap(
					ConditionTypeReady,
					"True",
					ReasonSucceeded,
					"spec.inCluster: the operator's own identity",
				),
				conditionMap(ConditionTypeReconciling, "False", ReasonSucceeded, "Reconciliation complete"),
				conditionMap(ConditionTypeStalled, "False", ReasonSucceeded, "KubeProvider is not stalled"),
				conditionMap(KubeProviderConditionValidated, "True", ReasonInCluster, "the operator's own cluster"),
				conditionMap(KubeProviderConditionAuditFactsReceived, "Unknown", ReasonRouteUnused,
					`audit requests are arriving, but nothing has been published for route "team-a.srcns-delegating"`),
			},
			wantStatus: kstatus.CurrentStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := kstatus.Compute(kubeProviderStatusObject(tt.conds))
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantMsg != "" {
				assert.Contains(t, result.Message, tt.wantMsg)
			}
		})
	}
}

func kubeProviderStatusObject(conditions []map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "KubeProvider",
		"metadata": map[string]interface{}{
			"name":       "srcns-delegating",
			"namespace":  "team-a",
			"generation": int64(3),
		},
		"status": map[string]interface{}{
			"observedGeneration": int64(3),
			"conditions":         conditions,
		},
	}}
}
