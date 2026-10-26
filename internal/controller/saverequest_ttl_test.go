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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// finishedWithDeleteAfter is a Committed request carrying the given delete-after value.
func finishedWithDeleteAfter(name, deleteAfter string) *configv1alpha3.SaveRequest {
	cr := withReadyCommitted(newSaveRequest(name))
	cr.Annotations = map[string]string{SaveRequestDeleteAfterAnnotation: deleteAfter}
	return cr
}

func saveRequestExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var cr configv1alpha3.SaveRequest
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
func TestSaveRequestTTL_FinishingStampsTheDeletionTime(t *testing.T) {
	c := newSaveRequestClient(t, nil, newSaveRequest("save-now"))
	r := &SaveRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
		TTL: 48 * time.Hour,
	}

	reconcileSaveRequest(t, r, "save-now")

	got := fetchSaveRequest(t, c, "save-now")
	value, ok := got.Annotations[SaveRequestDeleteAfterAnnotation]
	require.True(t, ok, "a finished request carries its deletion time")
	deleteAfter, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(48*time.Hour), deleteAfter, time.Minute)
}

func TestSaveRequestTTL_ZeroStampsNothing(t *testing.T) {
	c := newSaveRequestClient(t, nil, newSaveRequest("save-now"))
	r := &SaveRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
	}

	reconcileSaveRequest(t, r, "save-now")

	assert.NotContains(t, fetchSaveRequest(t, c, "save-now").Annotations, SaveRequestDeleteAfterAnnotation)
}

// A request that finished before the annotation existed, or one someone removed it from, is kept:
// only the transition to terminal stamps, never a later look at a terminal request.
func TestSaveRequestTTL_AFinishedRequestWithoutTheAnnotationIsKept(t *testing.T) {
	c := newSaveRequestClient(t, nil, withReadyCommitted(newSaveRequest("save-before")))
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}, TTL: time.Nanosecond}

	res := reconcileSaveRequest(t, r, "save-before")

	assert.Zero(t, res.RequeueAfter)
	got := fetchSaveRequest(t, c, "save-before")
	assert.NotContains(t, got.Annotations, SaveRequestDeleteAfterAnnotation, "it is not stamped after the fact")
}

func TestSaveRequestTTL_DeletesOnceTheTimeHasPassed(t *testing.T) {
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	c := newSaveRequestClient(t, nil, finishedWithDeleteAfter("save-old", past))
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileSaveRequest(t, r, "save-old")

	assert.Zero(t, res.RequeueAfter)
	assert.False(t, saveRequestExists(t, c, "save-old"),
		"the annotation alone decides; a TTL of zero now does not reach back to it")
}

// The request comes back when its time is up rather than on a poll, so a namespace full of recent
// saves costs one requeue each and nothing in between.
func TestSaveRequestTTL_ComesBackWhenTheTimeIsUp(t *testing.T) {
	future := time.Now().Add(47 * time.Hour).UTC().Format(time.RFC3339)
	c := newSaveRequestClient(t, nil, finishedWithDeleteAfter("save-recent", future))
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileSaveRequest(t, r, "save-recent")

	assert.True(t, saveRequestExists(t, c, "save-recent"))
	assert.InDelta(t, (47 * time.Hour).Seconds(), res.RequeueAfter.Seconds(), 60)
}

func TestSaveRequestTTL_AnUnreadableTimeKeepsTheRequest(t *testing.T) {
	c := newSaveRequestClient(t, nil, finishedWithDeleteAfter("save-typo", "tomorrow"))
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: &fakeFinalizer{}}

	res := reconcileSaveRequest(t, r, "save-typo")

	assert.Zero(t, res.RequeueAfter)
	assert.True(t, saveRequestExists(t, c, "save-typo"))
}

// A request still waiting for its outcome is neither stamped nor deleted; it is polled as before.
func TestSaveRequestTTL_LeavesARequestInProgressAlone(t *testing.T) {
	cr := withInProgress(newSaveRequest("save-pending"))
	cr.CreationTimestamp = metav1.Now() // inside the resolve window, so it polls rather than fails closed
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &SaveRequestReconciler{
		Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice(), TTL: time.Nanosecond,
	}

	res := reconcileSaveRequest(t, r, "save-pending")

	assert.Equal(t, saveRequestPollInterval, res.RequeueAfter, "it keeps polling for the outcome")
	assert.NotContains(t, fetchSaveRequest(t, c, "save-pending").Annotations, SaveRequestDeleteAfterAnnotation)
}

// A person who extends delete-after in the moment between the terminal status write and the stamp
// must keep the extension. The stamp is computed from the object the status write returned, so an
// unconditional patch would overwrite the edit and delete the request early.
func TestSaveRequestTTL_StampDoesNotOverwriteAConcurrentExtension(t *testing.T) {
	const extended = "2099-01-01T00:00:00Z"
	c := newSaveRequestClient(t, &interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			if err := cl.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			if !saveRequestIsTerminal(obj.(*configv1alpha3.SaveRequest)) {
				return nil
			}
			// The edit lands right after the terminal status is published, before the stamp.
			var live configv1alpha3.SaveRequest
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(obj), &live))
			live.Annotations = map[string]string{SaveRequestDeleteAfterAnnotation: extended}
			require.NoError(t, cl.Update(ctx, &live))
			return nil
		},
	}, newSaveRequest("save-now"))
	r := &SaveRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
		TTL: time.Hour,
	}

	reconcileSaveRequest(t, r, "save-now")

	assert.Equal(t, extended,
		fetchSaveRequest(t, c, "save-now").Annotations[SaveRequestDeleteAfterAnnotation],
		"the extension made after the request finished survives the stamp")
}

// A submitter may choose its own deletion time up front; finishing does not replace it.
func TestSaveRequestTTL_AValueSetAtCreationIsKept(t *testing.T) {
	const chosen = "2099-01-01T00:00:00Z"
	cr := newSaveRequest("save-now")
	cr.Annotations = map[string]string{SaveRequestDeleteAfterAnnotation: chosen}
	c := newSaveRequestClient(t, nil, cr)
	r := &SaveRequestReconciler{
		Client: c, APIReader: c, Finalizer: committedFinalizer(), AuthorLookup: attributedAlice(),
		TTL: time.Hour,
	}

	reconcileSaveRequest(t, r, "save-now")

	assert.Equal(t, chosen,
		fetchSaveRequest(t, c, "save-now").Annotations[SaveRequestDeleteAfterAnnotation])
}
