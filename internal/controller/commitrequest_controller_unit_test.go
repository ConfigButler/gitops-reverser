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
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// fakeFinalizer records the reconciler's finalize calls and replies with a
// canned outcome.
type fakeFinalizer struct {
	result git.FinalizeResult
	err    error

	finalizeCalls []fakeFinalizeCall
}

type fakeFinalizeCall struct {
	Author             string
	GitTargetName      string
	GitTargetNamespace string
	Message            string
}

func (f *fakeFinalizer) FinalizeGitTargetWindow(
	_ context.Context,
	author, gitTargetName, gitTargetNamespace, message string,
) (git.FinalizeResult, error) {
	f.finalizeCalls = append(f.finalizeCalls, fakeFinalizeCall{
		Author:             author,
		GitTargetName:      gitTargetName,
		GitTargetNamespace: gitTargetNamespace,
		Message:            message,
	})
	return f.result, f.err
}

// fakeAuthorLookup is the attribution source stub: found=false models the
// CommitRequest's audit event still being in flight.
type fakeAuthorLookup struct {
	author string
	found  bool
	calls  int
}

func (f *fakeAuthorLookup) LookupCommitRequestAuthor(
	_ context.Context, _, _ string, _ types.UID,
) (string, bool) {
	f.calls++
	return f.author, f.found
}

func attributedAlice() *fakeAuthorLookup { return &fakeAuthorLookup{author: "alice", found: true} }

func newCommitRequest(name string, phase configv1alpha1.CommitRequestPhase) *configv1alpha1.CommitRequest {
	cr := &configv1alpha1.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: configv1alpha1.CommitRequestSpec{
			GitTargetRef: configv1alpha1.CommitRequestGitTargetReference{Name: "team-a-config"},
			Message:      "save: " + name,
		},
	}
	cr.Status.Phase = phase
	return cr
}

func newCommitRequestClient(t *testing.T, fns *interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha1.CommitRequest{})
	if fns != nil {
		builder = builder.WithInterceptorFuncs(*fns)
	}
	return builder.Build()
}

func reconcileCommitRequest(t *testing.T, r *CommitRequestReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	})
	require.NoError(t, err)
	return res
}

func fetchCommitRequest(t *testing.T, c client.Client, name string) configv1alpha1.CommitRequest {
	t.Helper()
	var cr configv1alpha1.CommitRequest
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &cr))
	return cr
}

func TestCommitRequestReconcile_Committed(t *testing.T) {
	cr := newCommitRequest("save-1", "")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-1")

	got := fetchCommitRequest(t, c, "save-1")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, got.Status.Phase)
	assert.Equal(t, "abc123", got.Status.SHA)
	assert.Equal(t, "main", got.Status.Branch)
	assert.Empty(t, got.Status.Message)
	assert.NotNil(t, got.Status.ObservedTime)

	require.Len(t, f.finalizeCalls, 1)
	call := f.finalizeCalls[0]
	assert.Equal(t, "alice", call.Author,
		"the finalize must carry the author attributed from the audit event")
	assert.Equal(t, "team-a-config", call.GitTargetName)
	assert.Equal(t, "default", call.GitTargetNamespace)
	assert.Equal(t, "save: save-1", call.Message)
}

func TestCommitRequestReconcile_NoOpenWindow(t *testing.T) {
	cr := newCommitRequest("save-now", "")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"},
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-now")

	got := fetchCommitRequest(t, c, "save-now")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, got.Status.Phase)
	assert.Empty(t, got.Status.SHA)
	assert.Empty(t, got.Status.Message)
}

// The author-bound refusal: an open window belonging to someone else is left
// open and the CommitRequest terminates with the explanatory message.
func TestCommitRequestReconcile_WindowMismatchIsExplained(t *testing.T) {
	cr := newCommitRequest("save-foreign", "")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"},
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-foreign")

	got := fetchCommitRequest(t, c, "save-foreign")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, got.Status.Phase)
	assert.Equal(t, windowMismatchMessage, got.Status.Message)
	assert.Empty(t, got.Status.SHA)
}

func TestCommitRequestReconcile_FinalizeErrorBecomesFailed(t *testing.T) {
	cr := newCommitRequest("save-fail", "")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		err: errors.New("worker queue saturated"),
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-fail")

	got := fetchCommitRequest(t, c, "save-fail")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "worker queue saturated")
}

// A young CommitRequest whose audit event has not been ingested yet polls for
// attribution instead of finalizing: the event is both the author source and
// the ordering anchor (the author's earlier edits entered the audit path
// before it).
func TestCommitRequestReconcile_AttributionPendingRetries(t *testing.T) {
	cr := newCommitRequest("save-fresh", "")
	cr.CreationTimestamp = metav1.Now()
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	lookup := &fakeAuthorLookup{found: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	res := reconcileCommitRequest(t, r, "save-fresh")

	assert.Equal(t, commitRequestAttributionRetryDelay, res.RequeueAfter,
		"an unattributed young CommitRequest must poll for its audit event")
	assert.Empty(t, f.finalizeCalls, "no finalize may run before attribution")
	got := fetchCommitRequest(t, c, "save-fresh")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, got.Status.Phase)
	assert.Equal(t, 1, lookup.calls)
}

// Past the attribution bound the request fails closed: finalizing a window
// without knowing the requester would risk committing someone else's work.
func TestCommitRequestReconcile_AttributionTimeoutFailsClosed(t *testing.T) {
	// The zero CreationTimestamp puts the object far past the bound.
	cr := newCommitRequest("save-unattributed", "")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: &fakeAuthorLookup{}}

	reconcileCommitRequest(t, r, "save-unattributed")

	got := fetchCommitRequest(t, c, "save-unattributed")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, got.Status.Phase)
	assert.Equal(t, attributionFailedMessage, got.Status.Message)
	assert.Empty(t, f.finalizeCalls)
}

// spec.delaySeconds holds the finalize as an extra collect window, anchored at
// the creation time.
func TestCommitRequestReconcile_DelaySecondsHoldsFinalize(t *testing.T) {
	cr := newCommitRequest("save-linger", "")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.DelaySeconds = 30
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	res := reconcileCommitRequest(t, r, "save-linger")

	assert.Greater(t, res.RequeueAfter, time.Duration(0),
		"the finalize must be held for the requested collect window")
	assert.LessOrEqual(t, res.RequeueAfter, 30*time.Second)
	assert.Empty(t, f.finalizeCalls)
	got := fetchCommitRequest(t, c, "save-linger")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, got.Status.Phase)
}

func TestCommitRequestReconcile_DelayElapsedProceeds(t *testing.T) {
	// Zero CreationTimestamp: the delay window has long passed.
	cr := newCommitRequest("save-lingered", "")
	cr.Spec.DelaySeconds = 5
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "eee222", Branch: "main"},
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-lingered")

	got := fetchCommitRequest(t, c, "save-lingered")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, got.Status.Phase)
	require.Len(t, f.finalizeCalls, 1)
}

func TestCommitRequestReconcile_TerminalPhaseShortCircuits(t *testing.T) {
	cr := newCommitRequest("save-done", configv1alpha1.CommitRequestPhaseCommitted)
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	lookup := attributedAlice()
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	reconcileCommitRequest(t, r, "save-done")

	assert.Zero(t, lookup.calls)
	assert.Empty(t, f.finalizeCalls, "a terminal CommitRequest must never re-finalize")
}

// A reconcile triggered by a stale cache echo (the cached object still says
// WaitingForAuditEvent while the live object is already terminal) must not
// re-run the finalize: the uncached APIReader read is the guard.
func TestCommitRequestReconcile_StaleCacheEchoDoesNotRefinalize(t *testing.T) {
	stale := newCommitRequest("save-echo", configv1alpha1.CommitRequestPhaseWaitingForAuditEvent)
	cached := newCommitRequestClient(t, nil, stale)
	terminal := newCommitRequest("save-echo", configv1alpha1.CommitRequestPhaseCommitted)
	live := newCommitRequestClient(t, nil, terminal)

	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: cached, APIReader: live, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-echo")

	assert.Empty(t, f.finalizeCalls)
}

func TestCommitRequestReconcile_NilSeamsLeaveWaiting(t *testing.T) {
	cr := newCommitRequest("save-wait", "")
	c := newCommitRequestClient(t, nil, cr)
	r := &CommitRequestReconciler{Client: c, APIReader: c}

	reconcileCommitRequest(t, r, "save-wait")

	got := fetchCommitRequest(t, c, "save-wait")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseWaitingForAuditEvent, got.Status.Phase)
}

func TestCommitRequestReconcile_ObjectDeletedIsBenign(t *testing.T) {
	c := newCommitRequestClient(t, nil)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "gone")

	assert.Empty(t, f.finalizeCalls)
}

func TestCommitRequestReconcile_TerminalWriteRetriesOnConflict(t *testing.T) {
	// Phase already stamped: this models a post-restart redelivery, so the
	// only status write is the terminal one — the write the conflict hits.
	cr := newCommitRequest("save-retry", configv1alpha1.CommitRequestPhaseWaitingForAuditEvent)

	conflicts := 1
	fns := interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(
					schema.GroupResource{Group: "configbutler.ai", Resource: "commitrequests"},
					obj.GetName(), errors.New("simulated"))
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
	c := newCommitRequestClient(t, &fns, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "ddd111", Branch: "main"},
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-retry")

	got := fetchCommitRequest(t, c, "save-retry")
	assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, got.Status.Phase)
	assert.Equal(t, "ddd111", got.Status.SHA)
	require.Len(t, f.finalizeCalls, 1, "the conflict retry must re-write status, not re-finalize")
}

// --- pure helpers (formerly internal/queue/commit_request_test.go) ---

func TestApplyFinalizeResultToStatus(t *testing.T) {
	now := metav1.Now()

	t.Run("committed", func(t *testing.T) {
		var cr configv1alpha1.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseCommitted, cr.Status.Phase)
		assert.Equal(t, "abc", cr.Status.SHA)
		assert.Equal(t, "main", cr.Status.Branch)
		assert.Empty(t, cr.Status.Message)
	})

	t.Run("no open window", func(t *testing.T) {
		var cr configv1alpha1.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, cr.Status.Phase)
		assert.Empty(t, cr.Status.SHA)
	})

	t.Run("window mismatch surfaces the reason", func(t *testing.T) {
		var cr configv1alpha1.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseNoOpenWindow, cr.Status.Phase)
		assert.Equal(t, windowMismatchMessage, cr.Status.Message)
	})

	t.Run("finalize error wins", func(t *testing.T) {
		var cr configv1alpha1.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc"},
			errors.New("boom"), now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, cr.Status.Phase)
		assert.Equal(t, "boom", cr.Status.Message)
	})

	t.Run("unknown outcome is failed", func(t *testing.T) {
		var cr configv1alpha1.CommitRequest
		applyFinalizeResultToStatus(&cr, git.FinalizeResult{}, nil, now)
		assert.Equal(t, configv1alpha1.CommitRequestPhaseFailed, cr.Status.Phase)
		assert.Contains(t, cr.Status.Message, "unexpected finalize outcome")
	})
}

func TestIsTerminalCommitRequestPhase(t *testing.T) {
	assert.False(t, isTerminalCommitRequestPhase(""))
	assert.False(t, isTerminalCommitRequestPhase(configv1alpha1.CommitRequestPhaseWaitingForAuditEvent))
	assert.True(t, isTerminalCommitRequestPhase(configv1alpha1.CommitRequestPhaseCommitted))
	assert.True(t, isTerminalCommitRequestPhase(configv1alpha1.CommitRequestPhaseNoOpenWindow))
	assert.True(t, isTerminalCommitRequestPhase(configv1alpha1.CommitRequestPhaseFailed))
}

func TestCapCommitRequestMessage(t *testing.T) {
	short := "save the world"
	assert.Equal(t, short, capCommitRequestMessage(short))

	long := strings.Repeat("ü", commitRequestMessageMaxBytes) // 2 bytes per rune
	capped := capCommitRequestMessage(long)
	assert.LessOrEqual(t, len(capped), commitRequestMessageMaxBytes)
	assert.True(t, utf8.ValidString(capped), "the cap must not split a multi-byte rune")
}

func TestTruncateUTF8(t *testing.T) {
	assert.Equal(t, "abc", truncateUTF8("abc", 10))
	assert.Equal(t, "ab", truncateUTF8("abc", 2))
	// "é" is 2 bytes; truncating at 3 bytes must drop the split rune.
	assert.Equal(t, "aé", truncateUTF8("aéé", 3))
	assert.True(t, utf8.ValidString(truncateUTF8(strings.Repeat("世", 100), 7)))
}
