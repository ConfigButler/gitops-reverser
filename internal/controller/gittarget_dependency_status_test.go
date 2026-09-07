// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// gitTargetWithReady builds a GitTarget carrying one Ready condition, the input gitTargetReadyCondition
// projects into the readiness a WatchRule mirrors.
func gitTargetWithReady(
	gen int64,
	status metav1.ConditionStatus,
	reason, message string,
) *configbutleraiv1alpha3.GitTarget {
	t := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "team-a", Generation: gen},
	}
	if status != "" {
		t.Status.Conditions = []metav1.Condition{{
			Type: GitTargetConditionReady, Status: status, Reason: reason, Message: message,
		}}
	}
	return t
}

func TestGitTargetReadyProjectionChanged(t *testing.T) {
	p := gitTargetReadyProjectionChanged()

	// The reported bug: a GitTarget heals and publishes it as a status-only update. Dropping this
	// left the rule's mirrored GitTargetReady frozen on the failing snapshot for a full requeue,
	// contradicting the rule's own stream count.
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, metav1.ConditionFalse, "WatchError", "1/2 streams running"),
		ObjectNew: gitTargetWithReady(1, metav1.ConditionTrue, GitTargetReasonReady, "all streams running"),
	}), "a heal (status-only, same generation) must fire")

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, metav1.ConditionFalse, "WatchError", "blocked"),
		ObjectNew: gitTargetWithReady(1, metav1.ConditionFalse, ReasonProgressing, "blocked"),
	}), "a reason move at the same status must fire")

	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, metav1.ConditionTrue, GitTargetReasonReady, "all streams running"),
		ObjectNew: gitTargetWithReady(1, metav1.ConditionTrue, GitTargetReasonReady, "all streams running"),
	}), "an unchanged projection must not fire")

	// Message is deliberately excluded: it moves with transient stream detail, and firing on it
	// would re-enqueue every rule of a target on churn that changes no verdict.
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, metav1.ConditionFalse, "WatchError", "1 blocked (a)"),
		ObjectNew: gitTargetWithReady(1, metav1.ConditionFalse, "WatchError", "1 blocked (b)"),
	}), "a message-only change must not fire")

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, metav1.ConditionTrue, GitTargetReasonReady, "ready"),
		ObjectNew: gitTargetWithReady(2, metav1.ConditionTrue, GitTargetReasonReady, "ready"),
	}), "a spec (generation) change must fire")

	// A GitTarget that has not published Ready yet projects as Unknown, so the first publish is a
	// move the rule has to see.
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: gitTargetWithReady(1, "", "", ""),
		ObjectNew: gitTargetWithReady(1, metav1.ConditionTrue, GitTargetReasonReady, "ready"),
	}), "the first Ready publish must fire")

	assert.True(t, p.Create(event.CreateEvent{}))
	assert.True(t, p.Delete(event.DeleteEvent{}))
	assert.False(t, p.Generic(event.GenericEvent{}))
}
