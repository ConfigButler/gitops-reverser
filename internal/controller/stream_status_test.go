// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

func TestRuleReadiness_GitTargetReadyStalledBlocksRule(t *testing.T) {
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
	trio := ruleReadiness(conditions, "WatchRule", "rule ready").trio()

	assert.Equal(t, metav1.ConditionFalse, trio.Ready.Status)
	assert.Equal(t, metav1.ConditionFalse, trio.Reconciling.Status)
	assert.Equal(t, metav1.ConditionTrue, trio.Stalled.Status)
	assert.Equal(t, GitTargetReasonUnsupportedContent, trio.Stalled.Reason)
}
