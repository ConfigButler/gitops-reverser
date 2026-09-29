// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// finishedWithDeleteAfter is a Committed request carrying the given delete-after value.
func finishedWithDeleteAfter(name, deleteAfter string) *configv1alpha3.CommitRequest {
	cr := withReadyCommitted(newCommitRequest(name))
	cr.Annotations = map[string]string{CommitRequestDeleteAfterAnnotation: deleteAfter}
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

func committedFinalizer() *fakeFinalizer {
	return &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
		resolved: true,
	}
}

// Finishing is what writes the deletion time, so it is on the object for anyone to read or change
// from the moment the outcome is.
func TestCommitRequestTTL_FinishingStampsTheDeletionTime(t *testing.T) {
	c := newCommitRequestClient(t, nil, newCommitRequest("save-now"))
	r := &CommitRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
		TTL: 48 * time.Hour,
	}

	reconcileCommitRequest(t, r, "save-now")

	got := fetchCommitRequest(t, c, "save-now")
	value, ok := got.Annotations[CommitRequestDeleteAfterAnnotation]
	require.True(t, ok, "a finished request carries its deletion time")
	deleteAfter, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(48*time.Hour), deleteAfter, time.Minute)
}

func TestCommitRequestTTL_ZeroStampsNothing(t *testing.T) {
	c := newCommitRequestClient(t, nil, newCommitRequest("save-now"))
	r := &CommitRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
	}

	reconcileCommitRequest(t, r, "save-now")

	assert.NotContains(t, fetchCommitRequest(t, c, "save-now").Annotations, CommitRequestDeleteAfterAnnotation)
}

// A request that finished before the annotation existed, or one someone removed it from, is kept:
// only the transition to terminal stamps, never a later look at a terminal request.
func TestCommitRequestTTL_AFinishedRequestWithoutTheAnnotationIsKept(t *testing.T) {
	c := newCommitRequestClient(t, nil, withReadyCommitted(newCommitRequest("save-before")))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: time.Nanosecond}

	res := reconcileCommitRequest(t, r, "save-before")

	assert.Zero(t, res.RequeueAfter)
	got := fetchCommitRequest(t, c, "save-before")
	assert.NotContains(t, got.Annotations, CommitRequestDeleteAfterAnnotation, "it is not stamped after the fact")
}

func TestCommitRequestTTL_DeletesOnceTheTimeHasPassed(t *testing.T) {
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	c := newCommitRequestClient(t, nil, finishedWithDeleteAfter("save-old", past))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileCommitRequest(t, r, "save-old")

	assert.Zero(t, res.RequeueAfter)
	assert.False(t, commitRequestExists(t, c, "save-old"),
		"the annotation alone decides; a TTL of zero now does not reach back to it")
}

// The request comes back when its time is up rather than on a poll, so a namespace full of recent
// saves costs one requeue each and nothing in between.
func TestCommitRequestTTL_ComesBackWhenTheTimeIsUp(t *testing.T) {
	future := time.Now().Add(47 * time.Hour).UTC().Format(time.RFC3339)
	c := newCommitRequestClient(t, nil, finishedWithDeleteAfter("save-recent", future))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileCommitRequest(t, r, "save-recent")

	assert.True(t, commitRequestExists(t, c, "save-recent"))
	assert.InDelta(t, (47 * time.Hour).Seconds(), res.RequeueAfter.Seconds(), 60)
}

func TestCommitRequestTTL_AnUnreadableTimeKeepsTheRequest(t *testing.T) {
	c := newCommitRequestClient(t, nil, finishedWithDeleteAfter("save-typo", "tomorrow"))
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileCommitRequest(t, r, "save-typo")

	assert.Zero(t, res.RequeueAfter)
	assert.True(t, commitRequestExists(t, c, "save-typo"))
}

// A request still waiting for its outcome is neither stamped nor deleted; it is polled as before.
func TestCommitRequestTTL_LeavesARequestInProgressAlone(t *testing.T) {
	cr := withInProgress(newCommitRequest("save-pending"))
	cr.CreationTimestamp = metav1.Now() // inside the resolve window, so it polls rather than fails closed
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{
		Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice(), TTL: time.Nanosecond,
	}

	res := reconcileCommitRequest(t, r, "save-pending")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter, "it keeps polling for the outcome")
	assert.NotContains(t, fetchCommitRequest(t, c, "save-pending").Annotations, CommitRequestDeleteAfterAnnotation)
}
