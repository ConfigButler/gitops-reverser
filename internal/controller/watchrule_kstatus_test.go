// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kstatus "sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// TestWatchRuleSourceNamespaceKstatusContract walks every row of the PR 4 status table through the
// REAL kstatus helper, because the whole point of the contract is that
// sigs.k8s.io/cli-utils clients see the same Current/InProgress/Failed results as for the other
// CRDs — no phase, no state string, no second readiness model.
//
// The source-namespace gate has only two outcomes now, so this table shrank with it: an item is
// either authorized or refused, and a refusal is terminal. The rows that used to distinguish "the
// policy cannot be read YET" from "the policy can never be read" are gone, because there is no
// longer a policy in another cluster to read — every input is a control-plane object the reconcile
// already has.
func TestWatchRuleSourceNamespaceKstatusContract(t *testing.T) {
	tests := []struct {
		name       string
		conds      []map[string]interface{}
		wantStatus kstatus.Status
		wantMsg    string
	}{{
		name: "not yet evaluated: blocked behind an earlier gate",
		conds: []map[string]interface{}{
			conditionMap(ConditionTypeSourceNamespaceAuthorized, "Unknown",
				ReasonProgressing, "Blocked by validation; source namespace not evaluated"),
			conditionMap(ConditionTypeReady, "False",
				ReasonProgressing, "Blocked by validation"),
			conditionMap(ConditionTypeReconciling, "True",
				ReasonProgressing, "Blocked by validation"),
			conditionMap(ConditionTypeStalled, "False",
				ReasonProgressing, "WatchRule is not stalled"),
		},
		wantStatus: kstatus.InProgressStatus,
	}, {
		name: "authorized, but streams still replaying",
		conds: []map[string]interface{}{
			conditionMap(ConditionTypeSourceNamespaceAuthorized, "True",
				WatchRuleReasonSourceNamespaceAllowed, "admitted"),
			conditionMap(ConditionTypeReady, "False", ReasonProgressing, "Waiting for streams to run"),
			conditionMap(ConditionTypeReconciling, "True", ReasonProgressing, "Waiting for streams to run"),
			conditionMap(ConditionTypeStalled, "False", ReasonProgressing, "WatchRule is not stalled"),
		},
		wantStatus: kstatus.InProgressStatus,
	}, {
		name: "authorized and every prerequisite healthy",
		conds: []map[string]interface{}{
			conditionMap(ConditionTypeSourceNamespaceAuthorized, "True",
				WatchRuleReasonSourceNamespaceAllowed, "admitted"),
			conditionMap(ConditionTypeReady, "True", WatchRuleReasonReady, "WatchRule is ready"),
			conditionMap(ConditionTypeReconciling, "False", WatchRuleReasonReady, "Reconciliation complete"),
			conditionMap(ConditionTypeStalled, "False", WatchRuleReasonReady, "WatchRule is not stalled"),
		},
		wantStatus: kstatus.CurrentStatus,
	}, {
		name: "delegation disabled: the refusal is terminal",
		conds: []map[string]interface{}{
			conditionMap(ConditionTypeSourceNamespaceAuthorized, "False",
				WatchRuleReasonSourceNamespaceNotAllowed,
				"source namespace \"repo-config\" is not admitted"),
			conditionMap(ConditionTypeReady, "False",
				WatchRuleReasonSourceNamespaceNotAllowed,
				"source namespace \"repo-config\" is not admitted"),
			conditionMap(ConditionTypeReconciling, "False",
				WatchRuleReasonSourceNamespaceNotAllowed, "Reconciliation is stalled"),
			conditionMap(ConditionTypeStalled, "True",
				WatchRuleReasonSourceNamespaceNotAllowed,
				"source namespace \"repo-config\" is not admitted"),
		},
		wantStatus: kstatus.FailedStatus,
		wantMsg:    "repo-config",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := kstatus.Compute(watchRuleStatusObject(tt.conds))
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantMsg != "" {
				assert.Contains(t, result.Message, tt.wantMsg)
			}
		})
	}
}

// TestRuleReadiness_SourceAuthorizationIsAPrerequisite asserts the aggregation itself, not just
// its inputs: Ready=True must require SourceNamespaceAuthorized=True even when every pre-existing
// prerequisite is healthy. Without this, a rule could report Ready while its gate said otherwise.
func TestRuleReadiness_SourceAuthorizationIsAPrerequisite(t *testing.T) {
	healthy := []metav1.Condition{
		{Type: ConditionTypeResourcesResolved, Status: metav1.ConditionTrue, Reason: "Resolved"},
		{Type: ConditionTypeGitTargetReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		{Type: ConditionTypeStreamsRunning, Status: metav1.ConditionTrue, Reason: "Streaming"},
	}

	tests := []struct {
		name        string
		sourceNS    *metav1.Condition
		wantReady   metav1.ConditionStatus
		wantStalled metav1.ConditionStatus
	}{{
		name:        "no source condition at all (ClusterWatchRule): unchanged behavior",
		sourceNS:    nil,
		wantReady:   metav1.ConditionTrue,
		wantStalled: metav1.ConditionFalse,
	}, {
		name: "authorized: Ready",
		sourceNS: &metav1.Condition{
			Type: ConditionTypeSourceNamespaceAuthorized, Status: metav1.ConditionTrue,
			Reason: WatchRuleReasonSourceNamespaceAllowed,
		},
		wantReady:   metav1.ConditionTrue,
		wantStalled: metav1.ConditionFalse,
	}, {
		name: "not yet evaluated: progressing, never stalled",
		sourceNS: &metav1.Condition{
			Type: ConditionTypeSourceNamespaceAuthorized, Status: metav1.ConditionUnknown,
			Reason: ReasonProgressing,
		},
		wantReady:   metav1.ConditionFalse,
		wantStalled: metav1.ConditionFalse,
	}, {
		name: "refused: stalled, even with every other prerequisite healthy",
		sourceNS: &metav1.Condition{
			Type: ConditionTypeSourceNamespaceAuthorized, Status: metav1.ConditionFalse,
			Reason: WatchRuleReasonSourceNamespaceNotAllowed,
		},
		wantReady:   metav1.ConditionFalse,
		wantStalled: metav1.ConditionTrue,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := append([]metav1.Condition(nil), healthy...)
			if tt.sourceNS != nil {
				conditions = append(conditions, *tt.sourceNS)
			}

			trio := ruleReadiness(conditions, "WatchRule", "ready").trio()

			assert.Equal(t, tt.wantReady, trio.Ready.Status, "Ready")
			assert.Equal(t, tt.wantStalled, trio.Stalled.Status, "Stalled")
		})
	}
}

func watchRuleStatusObject(conditions []map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "WatchRule",
		"metadata": map[string]interface{}{
			"name":       "repo-config-rule",
			"namespace":  "tenant-acme",
			"generation": int64(4),
		},
		"status": map[string]interface{}{
			"observedGeneration": int64(4),
			"conditions":         conditions,
		},
	}}
}
