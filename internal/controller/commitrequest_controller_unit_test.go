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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// fakeFinalizer records the reconciler's attach calls and replies with a canned
// outcome. resolved=false models the worker still collecting (the controller polls).
type fakeFinalizer struct {
	result   git.FinalizeResult
	resolved bool
	err      error

	calls []git.AttachCommitRequest
}

func (f *fakeFinalizer) ServiceCommitRequest(
	_ context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	f.calls = append(f.calls, attach)
	return f.result, f.resolved, f.err
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

func newCommitRequest(name string) *configv1alpha2.CommitRequest {
	return &configv1alpha2.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: configv1alpha2.CommitRequestSpec{
			TargetRef: configv1alpha2.LocalTargetReference{Name: "team-a-config"},
			Message:   "save: " + name,
		},
	}
}

// withReadyCommitted stamps a terminal Committed CommitRequest (Ready=True).
func withReadyCommitted(cr *configv1alpha2.CommitRequest) *configv1alpha2.CommitRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: crReasonCommitted, Message: "committed",
	})
	return cr
}

// withInProgress stamps the still-running conditions a post-restart redelivery
// would already carry (Ready=False, Reconciling=True), which is non-terminal.
func withInProgress(cr *configv1alpha2.CommitRequest) *configv1alpha2.CommitRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: crReasonWaitingForAuditEvent, Message: "in progress",
	})
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReconciling, Status: metav1.ConditionTrue,
		Reason: crReasonWaitingForAuditEvent, Message: "in progress",
	})
	return cr
}

func newCommitRequestClient(t *testing.T, fns *interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha2.AddToScheme(scheme))
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha2.CommitRequest{})
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

func fetchCommitRequest(t *testing.T, c client.Client, name string) configv1alpha2.CommitRequest {
	t.Helper()
	var cr configv1alpha2.CommitRequest
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &cr))
	return cr
}

// requireCondition asserts a condition's status (and, when non-empty, reason) and
// returns it for further assertions.
func requireCondition(
	t *testing.T,
	cr configv1alpha2.CommitRequest,
	condType string,
	status metav1.ConditionStatus,
	reason string,
) metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(cr.Status.Conditions, condType)
	require.NotNil(t, c, "condition %s must be set", condType)
	assert.Equal(t, status, c.Status, "condition %s status", condType)
	if reason != "" {
		assert.Equal(t, reason, c.Reason, "condition %s reason", condType)
	}
	return *c
}

func TestCommitRequestReconcile_Committed(t *testing.T) {
	cr := newCommitRequest("save-1")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-1")

	got := fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, crReasonPushed)
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributedFromAudit)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionFalse, "")
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Equal(t, "abc123", got.Status.SHA)
	assert.Equal(t, "main", got.Status.Branch)

	require.Len(t, f.calls, 1)
	call := f.calls[0]
	assert.Equal(t, "alice", call.Author,
		"the attach must carry the author attributed from the audit event")
	assert.Equal(t, "team-a-config", call.GitTargetName)
	assert.Equal(t, "default", call.GitTargetNamespace)
	assert.Equal(t, "save: save-1", call.Message)
	assert.Equal(t, "save-1", call.Name)
	assert.Equal(t, "uid-save-1", call.UID)
}

func TestCommitRequestReconcile_NoOpenWindow(t *testing.T) {
	cr := newCommitRequest("save-now")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-now")

	got := fetchCommitRequest(t, c, "save-now")
	// A benign no-commit is Ready=True (serviced) with the specific reason, Pushed=False.
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindowInGrace)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindowInGrace)
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Empty(t, got.Status.SHA)
}

// The author-bound refusal: an open window belonging to someone else is left
// open and the CommitRequest reports the WindowMismatch reason (no commit).
func TestCommitRequestReconcile_WindowMismatchIsExplained(t *testing.T) {
	cr := newCommitRequest("save-foreign")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-foreign")

	got := fetchCommitRequest(t, c, "save-foreign")
	ready := requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
	assert.Equal(t, windowMismatchMessage, ready.Message)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonWindowMismatch)
	assert.Empty(t, got.Status.SHA)
}

// A matching window that finalized with no diff (loop prevention) reports the
// AlreadyPresent reason — never left hanging.
func TestCommitRequestReconcile_AlreadyPresentRejected(t *testing.T) {
	cr := newCommitRequest("save-noop")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-noop")

	got := fetchCommitRequest(t, c, "save-noop")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonAlreadyPresent)
	assert.Empty(t, got.Status.SHA)
}

// A resolved outcome that carries an error becomes a Failed (Stalled=True) request.
func TestCommitRequestReconcile_FinalizeErrorBecomesFailed(t *testing.T) {
	cr := newCommitRequest("save-fail")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Err: errors.New("commit failed: unreachable remote")},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-fail")

	got := fetchCommitRequest(t, c, "save-fail")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Contains(t, stalled.Message, "unreachable remote")
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, "")
}

// A young CommitRequest whose audit event has not been ingested yet polls for
// attribution instead of finalizing: the event is both the author source and
// the ordering anchor (the author's earlier edits entered the audit path before it).
func TestCommitRequestReconcile_AttributionPendingRetries(t *testing.T) {
	cr := newCommitRequest("save-fresh")
	cr.CreationTimestamp = metav1.Now()
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	lookup := &fakeAuthorLookup{found: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	res := reconcileCommitRequest(t, r, "save-fresh")

	assert.Equal(t, commitRequestAttributionRetryDelay, res.RequeueAfter,
		"an unattributed young CommitRequest must poll for its audit event")
	assert.Empty(t, f.calls, "no attach may run before attribution")
	assert.Equal(t, 1, lookup.calls)

	got := fetchCommitRequest(t, c, "save-fresh")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonWaitingForAuditEvent)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, crReasonWaitingForAuditEvent)
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionUnknown, crReasonWaitingForAuditEvent)
}

// Past the attribution bound the request finalizes as the configured committer
// instead of failing solely because audit/Redis is absent. The Attributed
// condition records that no end-user author was named.
func TestCommitRequestReconcile_AttributionTimeoutFallsBackToCommitter(t *testing.T) {
	// The zero CreationTimestamp puts the object far past the bound.
	cr := newCommitRequest("save-unattributed")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: &fakeAuthorLookup{}}

	reconcileCommitRequest(t, r, "save-unattributed")

	got := fetchCommitRequest(t, c, "save-unattributed")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, crReasonPushed)
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionFalse, crReasonAuditEventNotObserved)
	require.Len(t, f.calls, 1)
	assert.Empty(t, f.calls[0].Author, "blank author means the worker commits as the configured committer")
}

// The close-delay collect window is the worker's job now: the controller does not
// hold the finalize itself. While the worker has not resolved the attach, the
// controller polls — spec.closeDelaySeconds is passed through to the worker, not
// consumed here — and once the author is settled the request records the distinct
// WaitingForCloseDelay wait (the post-attribution close delay plus commit and push).
func TestCommitRequestReconcile_NotResolvedRecordsCloseDelayWait(t *testing.T) {
	cr := newCommitRequest("save-linger")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.CloseDelaySeconds = 30
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	res := reconcileCommitRequest(t, r, "save-linger")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter,
		"an unresolved attach must be polled, not held by a controller-side delay")
	require.Len(t, f.calls, 1, "the attach is sent the instant the author is known")
	assert.Equal(t, int32(30), f.calls[0].CloseDelaySeconds,
		"closeDelaySeconds is passed to the worker, not consumed here")

	got := fetchCommitRequest(t, c, "save-linger")
	// Author settled and attached: the request is in the WaitingForCloseDelay wait,
	// not the WaitingForAuditEvent wait.
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributedFromAudit)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, crReasonWaitingForCloseDelay)
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonWaitingForCloseDelay)
	assert.False(t, commitRequestIsTerminal(&got), "an unresolved request is not terminal")
}

// A transient service error (e.g. the GitTarget momentarily unreadable) keeps the
// request polling within the safety window rather than failing it outright.
func TestCommitRequestReconcile_ServiceErrorPolls(t *testing.T) {
	cr := newCommitRequest("save-transient")
	cr.CreationTimestamp = metav1.Now()
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{err: errors.New("get GitTarget: not found")}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	res := reconcileCommitRequest(t, r, "save-transient")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter)
	got := fetchCommitRequest(t, c, "save-transient")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, crReasonWaitingForCloseDelay)
	assert.False(t, commitRequestIsTerminal(&got))
}

// Past the resolve safety window an attach the worker never resolved fails closed.
func TestCommitRequestReconcile_ResolveTimeoutFailsClosed(t *testing.T) {
	// Zero CreationTimestamp: far past the resolve bound.
	cr := newCommitRequest("save-stuck")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-stuck")

	got := fetchCommitRequest(t, c, "save-stuck")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Equal(t, resolveTimeoutMessage, stalled.Message)
}

func TestCommitRequestReconcile_TerminalShortCircuits(t *testing.T) {
	cr := withReadyCommitted(newCommitRequest("save-done"))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	lookup := attributedAlice()
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	reconcileCommitRequest(t, r, "save-done")

	assert.Zero(t, lookup.calls)
	assert.Empty(t, f.calls, "a terminal CommitRequest must never re-attach")
}

// A reconcile triggered by a stale cache echo (the cached object still says
// in-progress while the live object is already terminal) must not re-run the
// finalize: the uncached APIReader read is the guard.
func TestCommitRequestReconcile_StaleCacheEchoDoesNotRefinalize(t *testing.T) {
	stale := withInProgress(newCommitRequest("save-echo"))
	cached := newCommitRequestClient(t, nil, stale)
	terminal := withReadyCommitted(newCommitRequest("save-echo"))
	live := newCommitRequestClient(t, nil, terminal)

	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: cached, APIReader: live, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-echo")

	assert.Empty(t, f.calls)
}

// Committer-only mode (no AuthorLookup) never waits for an audit event: a freshly
// created CommitRequest attaches immediately with a blank author and commits as the
// configured committer, recording Attributed=True (AttributionNotRequired).
func TestCommitRequestReconcile_CommitterOnlyCommitsWithoutWaiting(t *testing.T) {
	cr := newCommitRequest("save-committer-only")
	cr.CreationTimestamp = metav1.Now() // fresh: a waiting path would requeue instead of commit
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "c0ffee", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f} // AuthorLookup nil

	res := reconcileCommitRequest(t, r, "save-committer-only")

	assert.Zero(t, res.RequeueAfter, "committer-only must not requeue waiting for an audit event")
	got := fetchCommitRequest(t, c, "save-committer-only")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, crReasonPushed)
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributionNotRequired)
	assert.Equal(t, "c0ffee", got.Status.SHA)
	require.Len(t, f.calls, 1, "the attach is sent immediately, with no attribution wait")
	assert.Empty(t, f.calls[0].Author, "committer-only attaches with a blank author")
}

// Committer-only mode reports Attributed=True immediately and never parks the
// request in a "waiting for audit event" state: even while the attach is still
// being polled, Attributed is AttributionNotRequired, not WaitingForAuditEvent.
func TestCommitRequestReconcile_CommitterOnlyAttributedImmediately(t *testing.T) {
	cr := newCommitRequest("save-committer-poll")
	cr.CreationTimestamp = metav1.Now()
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f} // AuthorLookup nil

	res := reconcileCommitRequest(t, r, "save-committer-poll")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter, "an unresolved attach is polled")
	require.Len(t, f.calls, 1)
	assert.Empty(t, f.calls[0].Author)

	got := fetchCommitRequest(t, c, "save-committer-poll")
	requireCondition(t, got, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributionNotRequired)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, crReasonWaitingForCloseDelay)
	assert.False(t, commitRequestIsTerminal(&got), "in the close-delay wait, not terminal")
}

// With no Finalizer wired the controller is inert: it neither attaches, stamps any
// condition, nor drives the request to a terminal outcome. (Production always wires
// the Finalizer; this is the disabled guard at the top of Reconcile.)
func TestCommitRequestReconcile_NilFinalizerIsInert(t *testing.T) {
	cr := newCommitRequest("save-no-finalizer")
	c := newCommitRequestClient(t, nil, cr)
	r := &CommitRequestReconciler{Client: c, APIReader: c} // Finalizer nil, AuthorLookup nil

	res := reconcileCommitRequest(t, r, "save-no-finalizer")

	assert.Zero(t, res.RequeueAfter)
	got := fetchCommitRequest(t, c, "save-no-finalizer")
	assert.Empty(t, got.Status.Conditions, "a disabled controller stamps nothing")
	assert.False(t, commitRequestIsTerminal(&got))
}

func TestCommitRequestReconcile_ObjectDeletedIsBenign(t *testing.T) {
	c := newCommitRequestClient(t, nil)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "gone")

	assert.Empty(t, f.calls)
}

func TestCommitRequestReconcile_TerminalWriteRetriesOnConflict(t *testing.T) {
	// Already in-progress: this models a post-restart redelivery, so the only
	// status write is the terminal one — the write the conflict hits.
	cr := withInProgress(newCommitRequest("save-retry"))

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
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "ddd111", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-retry")

	got := fetchCommitRequest(t, c, "save-retry")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	assert.Equal(t, "ddd111", got.Status.SHA)
	require.Len(t, f.calls, 1, "the conflict retry must re-write status, not re-attach")
}

// --- pure helpers (formerly internal/queue/commit_request_test.go) ---

func TestApplyFinalizeResultToStatus(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc", Branch: "main"}, nil, attributionResolved)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionTrue, crReasonPushed)
		requireCondition(t, cr, ConditionTypeReconciling, metav1.ConditionFalse, "")
		requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionFalse, "")
		requireCondition(t, cr, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributedFromAudit)
		assert.Equal(t, "abc", cr.Status.SHA)
		assert.Equal(t, "main", cr.Status.Branch)
	})

	t.Run("no window in grace is a benign ready", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}, nil, attributionNotRequired)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindowInGrace)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindowInGrace)
		requireCondition(t, cr, ConditionTypeAttributed, metav1.ConditionTrue, crReasonAttributionNotRequired)
		assert.Empty(t, cr.Status.SHA)
	})

	t.Run("window mismatch surfaces the reason", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"}, nil, attributionResolved)
		ready := requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
		assert.Equal(t, windowMismatchMessage, ready.Message)
	})

	t.Run("already present is a benign ready", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"}, nil, attributionResolved)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
		assert.Empty(t, cr.Status.SHA)
	})

	t.Run("finalize error stalls", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc"},
			errors.New("boom"), attributionResolved)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
		stalled := requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
		assert.Equal(t, "boom", stalled.Message)
	})

	t.Run("unknown outcome stalls", func(t *testing.T) {
		var cr configv1alpha2.CommitRequest
		applyFinalizeResultToStatus(&cr, git.FinalizeResult{}, nil, attributionResolved)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionFalse, crReasonUnexpectedOutcome)
		stalled := requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionTrue, crReasonUnexpectedOutcome)
		assert.Contains(t, stalled.Message, "unexpected finalize outcome")
	})
}

func TestCommitRequestIsTerminal(t *testing.T) {
	assert.False(t, commitRequestIsTerminal(newCommitRequest("empty")))
	assert.False(t, commitRequestIsTerminal(withInProgress(newCommitRequest("in-progress"))))
	assert.True(t, commitRequestIsTerminal(withReadyCommitted(newCommitRequest("committed"))))

	stalled := newCommitRequest("failed")
	apimeta.SetStatusCondition(&stalled.Status.Conditions, metav1.Condition{
		Type: ConditionTypeStalled, Status: metav1.ConditionTrue, Reason: crReasonFinalizeFailed, Message: "boom",
	})
	assert.True(t, commitRequestIsTerminal(stalled))
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
