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

// TestCommitRequestKstatusContract pins the CommitRequest condition set to the
// kstatus the GitOps tooling derives from it: Reconciling=True → InProgress,
// Stalled=True → Failed, otherwise Current (with observedGeneration current).
func TestCommitRequestKstatusContract(t *testing.T) {
	tests := []struct {
		name       string
		conds      []map[string]interface{}
		wantStatus kstatus.Status
		wantMsg    string
	}{
		{
			name: "committer-only in the close-delay wait (attributed immediately)",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", crReasonWaitingForCloseDelay, closeDelayMessage),
				conditionMap(ConditionTypeReconciling, "True", crReasonWaitingForCloseDelay, closeDelayMessage),
				conditionMap(ConditionTypeStalled, "False", crReasonWaitingForCloseDelay, notStalledMessage),
				conditionMap(ConditionTypeAttributed, "True", crReasonAttributionNotRequired, "attribution disabled"),
				conditionMap(ConditionTypePushed, "Unknown", crReasonWaitingForCloseDelay, pushPendingMessage),
			},
			wantStatus: kstatus.InProgressStatus,
		},
		{
			name: "attributed mode waiting for the audit event",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", crReasonWaitingForAuditEvent, "waiting for audit event"),
				conditionMap(ConditionTypeReconciling, "True", crReasonWaitingForAuditEvent, "waiting for audit event"),
				conditionMap(ConditionTypeStalled, "False", crReasonWaitingForAuditEvent, notStalledMessage),
				conditionMap(ConditionTypeAttributed, "Unknown", crReasonWaitingForAuditEvent, "waiting"),
			},
			wantStatus: kstatus.InProgressStatus,
		},
		{
			name: "committed and pushed",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "True", crReasonCommitted, "closed, committed, and pushed"),
				conditionMap(ConditionTypeReconciling, "False", crReasonCommitted, "closed, committed, and pushed"),
				conditionMap(ConditionTypeStalled, "False", crReasonCommitted, notStalledMessage),
				conditionMap(ConditionTypeAttributed, "True", crReasonAttributedFromAudit, "from audit event"),
				conditionMap(ConditionTypePushed, "True", crReasonPushed, "pushed"),
			},
			wantStatus: kstatus.CurrentStatus,
		},
		{
			name: "benign rejection (no open window) is Current, not Failed",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "True", crReasonNoWindowInGrace, noWindowInGraceMessage),
				conditionMap(ConditionTypeReconciling, "False", crReasonNoWindowInGrace, noWindowInGraceMessage),
				conditionMap(ConditionTypeStalled, "False", crReasonNoWindowInGrace, notStalledMessage),
				conditionMap(ConditionTypePushed, "False", crReasonNoWindowInGrace, noWindowInGraceMessage),
			},
			wantStatus: kstatus.CurrentStatus,
		},
		{
			name: "finalize failure is Failed",
			conds: []map[string]interface{}{
				conditionMap(ConditionTypeReady, "False", crReasonFinalizeFailed, "commit failed: unreachable remote"),
				conditionMap(
					ConditionTypeReconciling,
					"False",
					crReasonFinalizeFailed,
					"commit failed: unreachable remote",
				),
				conditionMap(ConditionTypeStalled, "True", crReasonFinalizeFailed, "commit failed: unreachable remote"),
				conditionMap(ConditionTypePushed, "False", crReasonFinalizeFailed, "no commit was pushed"),
			},
			wantStatus: kstatus.FailedStatus,
			wantMsg:    "unreachable remote",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := kstatus.Compute(commitRequestStatusObject(tt.conds))
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantMsg != "" {
				assert.Contains(t, result.Message, tt.wantMsg)
			}
		})
	}
}

func commitRequestStatusObject(conditions []map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "configbutler.ai/v1alpha2",
		"kind":       "CommitRequest",
		"metadata": map[string]interface{}{
			"name":       "save-a",
			"namespace":  "default",
			"generation": int64(7),
		},
		"status": map[string]interface{}{
			"observedGeneration": int64(7),
			"conditions":         conditions,
		},
	}}
}
