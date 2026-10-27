// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

func catchingUp(cr *configv1alpha3.CommitRequest) *configv1alpha3.CommitRequest {
	cr.Spec.CatchUp = configv1alpha3.CatchUpAfterPush
	return cr
}

// TestCommitRequestReconcile_ReportsTheApplyItAskedFor: once the save is Ready, a request with
// catchUp: AfterPush is reconciled on, and Applied follows the worker's report until it
// is settled. Ready stays the save's.
func TestCommitRequestReconcile_ReportsTheApplyItAskedFor(t *testing.T) {
	c := newCommitRequestClient(t, nil, catchingUp(newCommitRequest("save-1")))
	f := committedFinalizer()
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-1")
	assert.Zero(t, f.applyLookups, "the save resolves first")

	f.apply, f.applyKnown = git.SaveApply{Reason: git.SaveApplyRetrying, Message: "the apply did not complete"}, true
	res := reconcileCommitRequest(t, r, "save-1")
	assert.Positive(t, res.RequeueAfter, "followed until it is settled")
	got := fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeApplied, metav1.ConditionUnknown, git.SaveApplyRetrying)
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)

	f.apply = git.SaveApply{
		Done:            true,
		Applied:         true,
		Reason:          git.SaveApplyApplied,
		AppliedRevision: "main@sha1:c0ffee",
	}
	reconcileCommitRequest(t, r, "save-1")
	got = fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeApplied, metav1.ConditionTrue, git.SaveApplyApplied)
	assert.Equal(t, "main@sha1:c0ffee", got.Status.AppliedRevision)

	lookups := f.applyLookups
	reconcileCommitRequest(t, r, "save-1")
	assert.Equal(t, lookups, f.applyLookups, "settled: not looked up again")
}

// TestCommitRequestReconcile_ASettledApplyWithoutAnApply: a request that pushed nothing, or whose
// GitTarget names no applier, is Applied=True: no apply was needed, and the cluster already holds what
// it saved. One whose commit the branch lost is Applied=False. None touches Ready.
func TestCommitRequestReconcile_ASettledApplyWithoutAnApply(t *testing.T) {
	for _, tc := range []struct {
		apply git.SaveApply
		want  metav1.ConditionStatus
	}{
		{git.SaveApply{Done: true, Applied: true, Reason: git.SaveApplyNothingToApply}, metav1.ConditionTrue},
		{git.SaveApply{Done: true, Reason: git.SaveApplyCommitDropped}, metav1.ConditionFalse},
		{git.SaveApply{Done: true, Applied: true, Reason: git.SaveApplyNoApplier}, metav1.ConditionTrue},
	} {
		t.Run(tc.apply.Reason, func(t *testing.T) {
			c := newCommitRequestClient(t, nil, catchingUp(newCommitRequest("save-1")))
			f := committedFinalizer()
			f.apply, f.applyKnown = tc.apply, true
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileCommitRequest(t, r, "save-1")
			reconcileCommitRequest(t, r, "save-1")
			got := fetchCommitRequest(t, c, "save-1")
			requireCondition(t, got, ConditionTypeApplied, tc.want, tc.apply.Reason)
			requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
			assert.Empty(t, got.Status.AppliedRevision)
		})
	}
}

// TestCommitRequestReconcile_AnApplyNobodyKnowsIsLost: a worker that knows nothing of the apply
// shortly after Ready is still taking it on; long after, it restarted, and Applied says it is lost.
func TestCommitRequestReconcile_AnApplyNobodyKnowsIsLost(t *testing.T) {
	c := newCommitRequestClient(t, nil, catchingUp(newCommitRequest("save-1")))
	f := committedFinalizer()
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}
	reconcileCommitRequest(t, r, "save-1")

	reconcileCommitRequest(t, r, "save-1")
	got := fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeApplied, metav1.ConditionUnknown, git.SaveApplyApplying)

	ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
	ready.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * applyLostAfter))
	require.NoError(t, c.Status().Update(context.Background(), &got))
	reconcileCommitRequest(t, r, "save-1")
	got = fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeApplied, metav1.ConditionUnknown, crReasonApplyLost)
	assert.False(t, followsApply(&got), "lost is settled")
}

// TestCommitRequestReconcile_NoCatchUpReportsNoApply: the default asks no apply, and has no Applied.
func TestCommitRequestReconcile_NoCatchUpReportsNoApply(t *testing.T) {
	c := newCommitRequestClient(t, nil, newCommitRequest("save-1"))
	f := committedFinalizer()
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-1")
	reconcileCommitRequest(t, r, "save-1")
	got := fetchCommitRequest(t, c, "save-1")
	assert.Nil(t, apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeApplied))
	assert.Zero(t, f.applyLookups)
}
