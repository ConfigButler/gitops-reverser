// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

var outageStart = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func failingPublication(paused bool) git.PublicationStatus {
	return git.PublicationStatus{
		Failing: true, Since: outageStart, Cause: "dial tcp: connection refused", IntakePaused: paused,
	}
}

// convergedObservation is a data plane with nothing pending.
func convergedObservation() dataPlaneObservation {
	return dataPlaneObservation{axes: gitTargetAxes{
		Streams: conditionValue{Status: metav1.ConditionTrue},
		GitPath: conditionValue{Status: metav1.ConditionTrue},
		Render:  conditionValue{Status: metav1.ConditionTrue},
	}}
}

func publicationGates(observed dataPlaneObservation, publication git.PublicationStatus) kstatusTrio {
	rd := newGitTargetReadiness()
	gitTargetReadinessGates(rd, observed, healthyDependency(), healthyDependency(), healthyDependency(),
		healthyDependency(), publicationCondition(publication))
	return rd.trio()
}

// A branch that cannot publish is not Ready, and it is progress, not a stall: the retry is
// scheduled and needs nobody. The message says since when, why, and whether intake is paused.
func TestPublicationReadiness_AnOutageIsProgressNotAStall(t *testing.T) {
	trio := publicationGates(convergedObservation(), failingPublication(true))
	assert.Equal(t, metav1.ConditionFalse, trio.Ready.Status)
	assert.Equal(t, ReasonProgressing, trio.Ready.Reason)
	assert.Contains(t, trio.Ready.Message, "dial tcp: connection refused")
	assert.Contains(t, trio.Ready.Message, "2026-10-04T12:00:00Z")
	assert.Contains(t, trio.Ready.Message, "intake of new changes is paused")
	assert.Equal(t, metav1.ConditionTrue, trio.Reconciling.Status)
	assert.Equal(t, metav1.ConditionFalse, trio.Stalled.Status)

	healthy := publicationGates(convergedObservation(), git.PublicationStatus{})
	assert.Equal(t, metav1.ConditionTrue, healthy.Ready.Status, "a branch publishing normally adds nothing")
}

// The publication failure explains the streams a paused branch keeps waiting, so it is reported
// ahead of them. A refused Git path is a different problem that needs a person, and a publication
// that is merely retrying must not hide it.
func TestPublicationReadiness_TakesPrecedenceOverWaitingStreamsNotOverRefusals(t *testing.T) {
	waiting := convergedObservation()
	waiting.streams = watch.StreamSummary{Total: 1, Replaying: 1}
	waiting.axes.Streams = conditionValue{Status: metav1.ConditionFalse, Reason: watch.StreamReasonReplaying,
		Message: "0/1 streams running; replaying: configmaps"}
	trio := publicationGates(waiting, failingPublication(true))
	assert.Contains(t, trio.Ready.Message, "Cannot publish to the Git remote")

	refused := convergedObservation()
	refused.axes.GitPath = conditionValue{Status: metav1.ConditionFalse, Reason: GitTargetReasonUnsupportedContent,
		Message: "the folder holds a kustomization the operator will not edit"}
	trio = publicationGates(refused, failingPublication(false))
	assert.Equal(t, metav1.ConditionTrue, trio.Stalled.Status, "the refusal still needs somebody")
	assert.Equal(t, GitTargetReasonUnsupportedContent, trio.Ready.Reason)
}

// A missing parent branch is reported by the parent gate, which says which branch to create. The
// publication report of the same outage would only repeat it, less precisely.
func TestPublicationReadiness_LeavesAMissingParentToTheParentGate(t *testing.T) {
	missing := failingPublication(false)
	missing.Cause = "parent branch not found on the remote: \"release\""
	missing.ParentUnavailable = true
	trio := publicationGates(convergedObservation(), missing)
	assert.Equal(t, metav1.ConditionTrue, trio.Ready.Status)
}

// A save the worker holds says why it is held, with the same words the GitTarget uses, and the
// status is written again when that reason changes, not only when the phase does.
func TestCommitRequestProgress_AHeldSaveSaysWhyItIsHeld(t *testing.T) {
	held := failingPublication(false).Message()
	reason, message := progressFor(git.PhaseWaitingForPush, held)
	assert.Equal(t, string(git.PhaseWaitingForPush), reason)
	assert.Contains(t, message, waitingForPushMessage)
	assert.Contains(t, message, "dial tcp: connection refused")

	_, plain := progressFor(git.PhaseWaitingForPush, "")
	assert.Equal(t, waitingForPushMessage, plain, "a push that is merely pending says nothing more")

	cr := &configbutleraiv1alpha3.CommitRequest{}
	markCommitRequestProgressing(cr, attributionFromAdmission, git.PhaseWaitingForPush, "")
	assert.True(t, progressRecorded(cr, git.PhaseWaitingForPush, ""))
	assert.False(t, progressRecorded(cr, git.PhaseWaitingForPush, held), "the outage is news to the save")
	markCommitRequestProgressing(cr, attributionFromAdmission, git.PhaseWaitingForPush, held)
	assert.True(t, progressRecorded(cr, git.PhaseWaitingForPush, held))
	assert.False(t, progressRecorded(cr, git.PhaseWaitingForPush, ""), "and so is the recovery")
}
