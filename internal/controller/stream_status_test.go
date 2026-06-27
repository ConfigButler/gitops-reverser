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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestApplyRuleKstatus_GitTargetReadyStalledBlocksRule(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: ConditionTypeResourcesResolved, Status: metav1.ConditionTrue, Reason: "Resolved", Message: "resolved"},
		{
			Type:    ConditionTypeGitTargetReady,
			Status:  metav1.ConditionFalse,
			Reason:  GitTargetReasonUnsupportedContent,
			Message: "Git path refused at kustomization.yaml: uses patches",
		},
		{
			Type:    ConditionTypeStreamsRunning,
			Status:  metav1.ConditionTrue,
			Reason:  watch.StreamReasonAllStreamsReady,
			Message: "1/1 streams running",
		},
	}
	got := map[string]metav1.ConditionStatus{}
	reasons := map[string]string{}

	applyRuleKstatus(
		conditions,
		"rule ready",
		"Ready",
		"rule is not stalled",
		func(conditionType string, status metav1.ConditionStatus, reason, _ string) {
			got[conditionType] = status
			reasons[conditionType] = reason
		},
		func(reason, _ string) {
			got[ConditionTypeReady] = metav1.ConditionFalse
			got[ConditionTypeReconciling] = metav1.ConditionFalse
			got[ConditionTypeStalled] = metav1.ConditionTrue
			reasons[ConditionTypeReady] = reason
			reasons[ConditionTypeReconciling] = reason
			reasons[ConditionTypeStalled] = reason
		},
	)

	assert.Equal(t, metav1.ConditionFalse, got[ConditionTypeReady])
	assert.Equal(t, metav1.ConditionFalse, got[ConditionTypeReconciling])
	assert.Equal(t, metav1.ConditionTrue, got[ConditionTypeStalled])
	assert.Equal(t, GitTargetReasonUnsupportedContent, reasons[ConditionTypeStalled])
}
