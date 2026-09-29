// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// finishedAgo stamps a Committed request whose Ready condition turned True `ago` before now,
// created a minute before that.
func finishedAgo(name string, ago time.Duration) *configv1alpha3.CommitRequest {
	cr := withReadyCommitted(newCommitRequest(name))
	finished := metav1.NewTime(time.Now().Add(-ago))
	cr.CreationTimestamp = metav1.NewTime(finished.Add(-time.Minute))
	apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady).LastTransitionTime = finished
	return cr
}

func commitRequestExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var cr configv1alpha3.CommitRequest
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &cr)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestCommitRequestTTL_DeletesAFinishedRequestPastIt(t *testing.T) {
	c := newCommitRequestClient(t, nil, finishedAgo("save-old", 49*time.Hour))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: 48 * time.Hour}

	res := reconcileCommitRequest(t, r, "save-old")

	assert.Zero(t, res.RequeueAfter)
	assert.False(t, commitRequestExists(t, c, "save-old"), "a request past its TTL is deleted")
}

// The request comes back when it expires rather than on a poll, so a namespace full of recent
// saves costs one requeue each and nothing in between.
func TestCommitRequestTTL_KeepsAYoungerRequestAndComesBackWhenItExpires(t *testing.T) {
	c := newCommitRequestClient(t, nil, finishedAgo("save-recent", time.Hour))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: 48 * time.Hour}

	res := reconcileCommitRequest(t, r, "save-recent")

	assert.True(t, commitRequestExists(t, c, "save-recent"))
	assert.InDelta(t, (47 * time.Hour).Seconds(), res.RequeueAfter.Seconds(), 60,
		"the requeue lands when the TTL runs out")
}

// Age is counted from when the outcome became readable, not from creation: a request that took
// long to resolve still gets the whole TTL after it did.
func TestCommitRequestTTL_CountsFromTheFinishNotTheCreation(t *testing.T) {
	cr := finishedAgo("save-slow", time.Hour)
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-72 * time.Hour))
	c := newCommitRequestClient(t, nil, cr)
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: 48 * time.Hour}

	reconcileCommitRequest(t, r, "save-slow")

	assert.True(t, commitRequestExists(t, c, "save-slow"))
}

func TestCommitRequestTTL_ZeroKeepsEverything(t *testing.T) {
	c := newCommitRequestClient(t, nil, finishedAgo("save-kept", 365*24*time.Hour))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileCommitRequest(t, r, "save-kept")

	assert.Zero(t, res.RequeueAfter, "with no TTL there is nothing to come back for")
	assert.True(t, commitRequestExists(t, c, "save-kept"))
}

func TestCommitRequestTTL_KeepAnnotationExemptsOneRequest(t *testing.T) {
	cr := finishedAgo("save-pinned", 49*time.Hour)
	cr.Annotations = map[string]string{CommitRequestKeepAnnotation: "true"}
	c := newCommitRequestClient(t, nil, cr)
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: 48 * time.Hour}

	res := reconcileCommitRequest(t, r, "save-pinned")

	assert.Zero(t, res.RequeueAfter)
	assert.True(t, commitRequestExists(t, c, "save-pinned"))
}

// Only a finished request expires. One still waiting for its outcome is polled as before, however
// old it is, and is deleted only on a later pass once it has finished and aged.
func TestCommitRequestTTL_NeverDeletesARequestInProgress(t *testing.T) {
	cr := withInProgress(newCommitRequest("save-pending"))
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{
		Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice(), TTL: time.Nanosecond,
	}

	res := reconcileCommitRequest(t, r, "save-pending")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter, "it keeps polling for the outcome")
	assert.True(t, commitRequestExists(t, c, "save-pending"))
	assert.Len(t, f.calls, 1)
}
