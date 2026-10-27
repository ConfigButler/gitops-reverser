// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// fakeFinalizer records the reconciler's attach calls and replies with a canned
// outcome. resolved=false models the worker still collecting (the controller polls).
type fakeFinalizer struct {
	result   git.FinalizeResult
	resolved bool
	err      error

	calls []git.AttachCommitRequest

	// The withdraw reply. Left unset, the withdraw answers like the router does before it cancels
	// anything: with the worker's outcome, or its phase while unresolved. withdrawPending makes an
	// unset reply mean "not answered yet" instead.
	withdrawResult   git.FinalizeResult
	withdrawResolved bool
	withdrawErr      error
	withdrawPending  bool
	withdrawCalls    []git.AttachCommitRequest

	// The apply a request with catchUp: AfterPush asked for, and the lookups made.
	apply        git.SaveApply
	applyKnown   bool
	applyErr     error
	applyLookups int
}

func (f *fakeFinalizer) LookupCommitRequestApply(
	_ context.Context, _ git.AttachCommitRequest,
) (git.SaveApply, bool, error) {
	f.applyLookups++
	return f.apply, f.applyKnown, f.applyErr
}

func (f *fakeFinalizer) WithdrawCommitRequest(
	_ context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	f.withdrawCalls = append(f.withdrawCalls, attach)
	if f.withdrawPending || f.withdrawResolved || f.withdrawErr != nil || f.withdrawResult.Phase != "" {
		return f.withdrawResult, f.withdrawResolved, f.withdrawErr
	}
	return f.result, f.resolved, f.err
}

func (f *fakeFinalizer) ServiceCommitRequest(
	_ context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	f.calls = append(f.calls, attach)
	return f.result, f.resolved, f.err
}

// newCommitRequest builds a request created just now, inside its safety window, as the API server
// would stamp it. pastTheBound ages one beyond it.
func newCommitRequest(name string) *configv1alpha3.CommitRequest {
	return &configv1alpha3.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.Now(),
		},
		Spec: configv1alpha3.CommitRequestSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: "team-a-config"},
			Message:      "save: " + name,
		},
	}
}

// pastTheBound ages a request beyond its safety window, where the controller only withdraws.
func pastTheBound(cr *configv1alpha3.CommitRequest) *configv1alpha3.CommitRequest {
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * resolveRequestWindow(cr.Spec).resolveTimeout()))
	return cr
}

// withReadyCommitted stamps a terminal Committed CommitRequest (Ready=True).
func withReadyCommitted(cr *configv1alpha3.CommitRequest) *configv1alpha3.CommitRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: crReasonCommitted, Message: "committed",
	})
	return cr
}

// withInProgress stamps the still-running conditions a post-restart redelivery
// would already carry (Ready=False, Reconciling=True), which is non-terminal.
func withInProgress(cr *configv1alpha3.CommitRequest) *configv1alpha3.CommitRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: string(git.PhaseWaitingForWindow), Message: "in progress",
	})
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReconciling, Status: metav1.ConditionTrue,
		Reason: string(git.PhaseWaitingForWindow), Message: "in progress",
	})
	// Admitted when it was first seen, by the GitTarget withGitTargets gives it.
	key := cr.GitTargetKey()
	cr.Status.GitTargetUID = "uid-" + key.Namespace + "-" + key.Name
	return cr
}

func newCommitRequestClient(t *testing.T, fns *interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()
	objects = withGitTargets(objects)
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha3.CommitRequest{})
	if fns != nil {
		builder = builder.WithInterceptorFuncs(*fns)
	}
	return builder.Build()
}

// withGitTargets gives each GitTarget a UID, as the API server would, and, when objects hold no
// GitTarget at all, adds the one each CommitRequest names: a request is only attached once a GitTarget
// admitted it. A test that passes GitTargets of its own gets only those.
func withGitTargets(objects []client.Object) []client.Object {
	present := map[client.ObjectKey]bool{}
	for _, obj := range objects {
		if target, ok := obj.(*configv1alpha3.GitTarget); ok {
			present[client.ObjectKeyFromObject(target)] = true
			if target.UID == "" {
				target.UID = types.UID("uid-" + target.Namespace + "-" + target.Name)
			}
		}
	}
	if len(present) > 0 {
		return objects
	}
	for _, obj := range objects {
		request, ok := obj.(*configv1alpha3.CommitRequest)
		if !ok || request.Spec.GitTargetRef.Name == "" || present[request.GitTargetKey()] {
			continue
		}
		key := request.GitTargetKey()
		present[key] = true
		objects = append(objects, &configv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace, UID: types.UID("uid-" + key.Namespace + "-" + key.Name),
		}})
	}
	return objects
}

func reconcileCommitRequest(t *testing.T, r *CommitRequestReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	})
	require.NoError(t, err)
	return res
}

func fetchCommitRequest(t *testing.T, c client.Client, name string) configv1alpha3.CommitRequest {
	t.Helper()
	var cr configv1alpha3.CommitRequest
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &cr))
	return cr
}

// requireCondition asserts a condition's status (and, when non-empty, reason) and
// returns it for further assertions.
func requireCondition(
	t *testing.T,
	cr configv1alpha3.CommitRequest,
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
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-1")

	got := fetchCommitRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionFalse, "")
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Equal(t, "abc123", got.Status.Commit)
	assert.Equal(t, "main", got.Status.Branch)

	require.Len(t, f.calls, 1)
	call := f.calls[0]
	assert.Empty(t, call.Author, "with attribution off, the request names no author")
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
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-now")

	got := fetchCommitRequest(t, c, "save-now")
	// A benign no-commit is Ready=True (serviced) with the specific reason, Pushed=False.
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindow)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindow)
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Empty(t, got.Status.Commit)
}

// whenNothingToCommit: CommitEmpty reaches the worker, and a recorded empty commit reports its
// commit and Pushed=True while the Ready reason keeps the cause.
func TestCommitRequestReconcile_CommitEmptyRecordsTheMessageAndKeepsTheCause(t *testing.T) {
	for _, tc := range []struct {
		outcome git.FinalizeOutcome
		reason  string
	}{
		{git.FinalizeNoOpenWindow, crReasonNoWindow},
		{git.FinalizeAlreadyPresent, crReasonAlreadyPresent},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			cr := newCommitRequest("save-empty")
			cr.Spec.WhenNothingToCommit = configv1alpha3.NothingToCommitCommitEmpty
			c := newCommitRequestClient(t, nil, cr)
			f := &fakeFinalizer{
				result:   git.FinalizeResult{Outcome: tc.outcome, Commit: "abc123", Branch: "main"},
				resolved: true,
			}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileCommitRequest(t, r, "save-empty")

			require.Len(t, f.calls, 1)
			assert.True(t, f.calls[0].CommitEmpty, "the choice must reach the worker")
			got := fetchCommitRequest(t, c, "save-empty")
			requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, tc.reason)
			requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
			requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
			assert.Equal(t, "abc123", got.Status.Commit)
			ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
			assert.Contains(t, ready.Message, "recorded in an empty commit")
		})
	}
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
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-foreign")

	got := fetchCommitRequest(t, c, "save-foreign")
	ready := requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
	assert.Equal(t, windowMismatchMessage, ready.Message)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonWindowMismatch)
	assert.Empty(t, got.Status.Commit)
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
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-noop")

	got := fetchCommitRequest(t, c, "save-noop")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonAlreadyPresent)
	assert.Empty(t, got.Status.Commit)
}

// A resolved outcome that carries an error becomes a Failed (Stalled=True) request.
func TestCommitRequestReconcile_FinalizeErrorBecomesFailed(t *testing.T) {
	cr := newCommitRequest("save-fail")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Err: errors.New("commit failed: unreachable remote")},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-fail")

	got := fetchCommitRequest(t, c, "save-fail")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Contains(t, stalled.Message, "unreachable remote")
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, "")
}

// A request whose window was never defaulted must reach the worker carrying the schema defaults,
// not zeros. The API server normally fills the block in, so nil arrives here only from a client
// that bypassed defaulting — and a zero there would be the old defect: the request expires before
// the write it exists to publish can open a window.
func TestCommitRequestReconcile_AnOmittedWindowUsesTheDefaults(t *testing.T) {
	cr := newCommitRequest("save-default")
	cr.CreationTimestamp = metav1.Now()
	require.Nil(t, cr.Spec.Window, "the fixture must leave the block unset")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-default")

	require.Len(t, f.calls, 1)
	assert.Equal(t, configv1alpha3.AttachCurrentOrNext, f.calls[0].Attach)
	assert.Equal(t, defaultAttachTimeout, f.calls[0].AttachTimeout)
	assert.Equal(t, defaultMaxDuration, f.calls[0].MaxDuration)
	assert.Nil(t, f.calls[0].IdleTimeout, "no idle close unless one is asked for")
}

// The pointer types exist so that an explicit zero survives defaulting, and each field keeps its
// own meaning: a zero attachTimeout is not a zero maxDuration.
func TestCommitRequestReconcile_ExplicitZerosArePreservedFieldByField(t *testing.T) {
	cr := newCommitRequest("save-now")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.Window = &configv1alpha3.CommitRequestWindow{
		AttachTimeout: &metav1.Duration{},
		IdleTimeout:   &metav1.Duration{},
		MaxDuration:   &metav1.Duration{Duration: 30 * time.Second},
	}
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-now")

	require.Len(t, f.calls, 1)
	assert.Zero(t, f.calls[0].AttachTimeout, "an explicit \"0s\" must not be re-read as an omitted field")
	require.NotNil(t, f.calls[0].IdleTimeout)
	assert.Zero(t, *f.calls[0].IdleTimeout)
	assert.Equal(t, 30*time.Second, f.calls[0].MaxDuration, "a zero attachTimeout never forces a zero collection")
}

// The window is the worker's job: the controller does not hold the finalize itself. While the
// worker has not resolved the attach, the controller polls and reports the phase the WORKER
// reports, never one it inferred from having sent the attach.
func TestCommitRequestReconcile_NotResolvedReportsTheWorkersPhase(t *testing.T) {
	cr := newCommitRequest("save-linger")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.Window = &configv1alpha3.CommitRequestWindow{MaxDuration: &metav1.Duration{Duration: 30 * time.Second}}
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	res := reconcileCommitRequest(t, r, "save-linger")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter,
		"an unresolved attach must be polled, not held by a controller-side delay")
	require.Len(t, f.calls, 1, "the attach is sent the instant the author is known")
	assert.Equal(t, 30*time.Second, f.calls[0].MaxDuration,
		"the window is passed to the worker, not consumed here")

	got := fetchCommitRequest(t, c, "save-linger")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, ReasonProgressing)
	assert.False(t, commitRequestIsTerminal(&got), "an unresolved request is not terminal")

	for _, phase := range []git.CommitRequestPhase{
		git.PhaseWaitingForWindow, git.PhaseCollectingWindow, git.PhaseWaitingForPush,
	} {
		f.result = git.FinalizeResult{Phase: phase}
		reconcileCommitRequest(t, r, "save-linger")
		got = fetchCommitRequest(t, c, "save-linger")
		requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, string(phase))
		requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, string(phase))
	}

	// A phase the worker has not (yet) reported keeps what is shown rather than guessing.
	f.result = git.FinalizeResult{}
	reconcileCommitRequest(t, r, "save-linger")
	got = fetchCommitRequest(t, c, "save-linger")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, string(git.PhaseWaitingForPush))
}

// A transient service error (e.g. the GitTarget momentarily unreadable) keeps the
// request polling within the safety window rather than failing it outright.
func TestCommitRequestReconcile_ServiceErrorPolls(t *testing.T) {
	cr := newCommitRequest("save-transient")
	cr.CreationTimestamp = metav1.Now()
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{err: errors.New("get GitTarget: not found")}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	res := reconcileCommitRequest(t, r, "save-transient")

	assert.Equal(t, commitRequestPollInterval, res.RequeueAfter)
	got := fetchCommitRequest(t, c, "save-transient")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, ReasonProgressing)
	assert.False(t, commitRequestIsTerminal(&got))
}

// Past the safety window a request the worker never acted on is withdrawn, and fails closed once
// the worker confirms the withdraw.
func TestCommitRequestReconcile_ResolveTimeoutFailsClosed(t *testing.T) {
	cr := pastTheBound(newCommitRequest("save-stuck"))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:           git.FinalizeResult{Phase: git.PhaseWaitingForWindow},
		withdrawResult:   git.FinalizeResult{Branch: "main", Err: git.ErrCommitRequestWithdrawn},
		withdrawResolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-stuck")

	require.Len(t, f.withdrawCalls, 1, "the request is withdrawn before it is failed")
	assert.Empty(t, f.calls, "past the bound no attach is sent: it would queue behind the withdraw")
	got := fetchCommitRequest(t, c, "save-stuck")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Equal(t, resolveTimeoutMessage, stalled.Message)
}

// A request the worker holds is never failed on the controller's clock: a down remote or a parent
// branch recovery can keep it in WaitingForPush far past the bound, and the worker still commits it.
func TestCommitRequestReconcile_AHeldRequestOutlivesTheBound(t *testing.T) {
	for _, phase := range []git.CommitRequestPhase{git.PhaseCollectingWindow, git.PhaseWaitingForPush} {
		t.Run(string(phase), func(t *testing.T) {
			cr := pastTheBound(newCommitRequest("save-held"))
			c := newCommitRequestClient(t, nil, cr)
			f := &fakeFinalizer{result: git.FinalizeResult{Phase: phase}}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			res := reconcileCommitRequest(t, r, "save-held")

			assert.Equal(t, commitRequestPollInterval, res.RequeueAfter)
			assert.Empty(t, f.calls, "past the bound no attach is sent")
			assert.Len(t, f.withdrawCalls, 1, "the withdraw finds the request held and changes nothing")
			got := fetchCommitRequest(t, c, "save-held")
			assert.False(t, commitRequestIsTerminal(&got))

			// The push lands much later: the request resolves Committed, not FinalizeFailed.
			f.result = git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc", Branch: "main"}
			f.resolved = true
			reconcileCommitRequest(t, r, "save-held")
			got = fetchCommitRequest(t, c, "save-held")
			requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		})
	}
}

// Until the worker answers the withdraw the request keeps polling; and when the worker turns out to
// hold it after all (it attached between the poll and the withdraw), nothing fails.
func TestCommitRequestReconcile_AnUnansweredWithdrawKeepsPolling(t *testing.T) {
	for name, reply := range map[string]git.FinalizeResult{
		"not handled yet":         {},
		"the worker now holds it": {Phase: git.PhaseCollectingWindow},
	} {
		t.Run(name, func(t *testing.T) {
			cr := pastTheBound(newCommitRequest("save-race"))
			c := newCommitRequestClient(t, nil, cr)
			f := &fakeFinalizer{
				result:          git.FinalizeResult{Phase: git.PhaseWaitingForWindow},
				withdrawResult:  reply,
				withdrawPending: true,
			}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			res := reconcileCommitRequest(t, r, "save-race")

			assert.Equal(t, commitRequestPollInterval, res.RequeueAfter)
			got := fetchCommitRequest(t, c, "save-race")
			assert.False(t, commitRequestIsTerminal(&got))
		})
	}
}

// A worker that resolved the request on its own just before the withdraw wins: its real outcome is
// what the request reports.
func TestCommitRequestReconcile_AWithdrawThatLostTheRaceReportsTheRealOutcome(t *testing.T) {
	cr := pastTheBound(newCommitRequest("save-late"))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		withdrawResult:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc", Branch: "main"},
		withdrawResolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-late")

	got := fetchCommitRequest(t, c, "save-late")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	assert.Equal(t, "abc", got.Status.Commit)
}

// A GitTarget that never started a branch worker is named as the cause.
func TestCommitRequestReconcile_NoWorkerIsNamedWhenItFailsClosed(t *testing.T) {
	cr := pastTheBound(newCommitRequest("save-no-worker"))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Phase: git.PhaseWaitingForWorker},
		withdrawResult: git.FinalizeResult{
			Branch: "main", Phase: git.PhaseWaitingForWorker, Err: git.ErrCommitRequestWithdrawn,
		},
		withdrawResolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-no-worker")

	got := fetchCommitRequest(t, c, "save-no-worker")
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Equal(t, noWorkerTimeoutMessage, stalled.Message)
}

func TestCommitRequestReconcile_TerminalShortCircuits(t *testing.T) {
	cr := withReadyCommitted(newCommitRequest("save-done"))
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-done")

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
	r := &CommitRequestReconciler{Client: cached, APIReader: live, Finalizer: f}

	reconcileCommitRequest(t, r, "save-echo")

	assert.Empty(t, f.calls)
}

// A freshly created CommitRequest attaches immediately with a blank author. The matching window
// determines the Git author.
func TestCommitRequestReconcile_ConfiguredAuthorCommitsWithoutWaiting(t *testing.T) {
	cr := newCommitRequest("save-configured-author")
	cr.CreationTimestamp = metav1.Now() // fresh: a waiting path would requeue instead of commit
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	res := reconcileCommitRequest(t, r, "save-configured-author")

	assert.Zero(t, res.RequeueAfter, "webhook-disabled mode must not requeue waiting for an author")
	got := fetchCommitRequest(t, c, "save-configured-author")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
	assert.Equal(t, "c0ffee", got.Status.Commit)
	require.Len(t, f.calls, 1, "the attach is sent immediately, with no attribution wait")
	assert.Empty(t, f.calls[0].Author, "webhook-disabled mode attaches with a blank author")
}

// With no Finalizer wired the controller is inert: it neither attaches, stamps any
// condition, nor drives the request to a terminal outcome. (Production always wires
// the Finalizer; this is the disabled guard at the top of Reconcile.)
func TestCommitRequestReconcile_NilFinalizerIsInert(t *testing.T) {
	cr := newCommitRequest("save-no-finalizer")
	c := newCommitRequestClient(t, nil, cr)
	r := &CommitRequestReconciler{Client: c, APIReader: c} // Finalizer nil

	res := reconcileCommitRequest(t, r, "save-no-finalizer")

	assert.Zero(t, res.RequeueAfter)
	got := fetchCommitRequest(t, c, "save-no-finalizer")
	assert.Empty(t, got.Status.Conditions, "a disabled controller stamps nothing")
	assert.False(t, commitRequestIsTerminal(&got))
}

func TestCommitRequestReconcile_ObjectDeletedIsBenign(t *testing.T) {
	c := newCommitRequestClient(t, nil)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

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
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "ddd111", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-retry")

	got := fetchCommitRequest(t, c, "save-retry")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	assert.Equal(t, "ddd111", got.Status.Commit)
	require.Len(t, f.calls, 1, "the conflict retry must re-write status, not re-attach")
}

// --- pure helpers (formerly internal/queue/commit_request_test.go) ---

func TestApplyFinalizeResultToStatus(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(
			&cr,
			git.FinalizeResult{
				Outcome: git.FinalizeCommitted,
				Commit:  "abc",
				Branch:  "main",
			},
			nil,
		)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
		requireCondition(t, cr, ConditionTypeReconciling, metav1.ConditionFalse, "")
		requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionFalse, "")
		assert.Equal(t, "abc", cr.Status.Commit)
		assert.Equal(t, "main", cr.Status.Branch)
	})

	t.Run("no window in grace is a benign ready", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}, nil)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindow)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindow)
		assert.Empty(t, cr.Status.Commit)
	})

	t.Run("window mismatch surfaces the reason", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"}, nil)
		ready := requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
		assert.Equal(t, windowMismatchMessage, ready.Message)
	})

	t.Run("already present is a benign ready", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"}, nil)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
		assert.Empty(t, cr.Status.Commit)
	})

	t.Run("finalize error stalls", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc"},
			errors.New("boom"))
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
		stalled := requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
		assert.Equal(t, "boom", stalled.Message)
	})

	t.Run("unknown outcome stalls", func(t *testing.T) {
		var cr configv1alpha3.CommitRequest
		applyFinalizeResultToStatus(&cr, git.FinalizeResult{}, nil)
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

// A CommitRequest stored before spec.targetRef became spec.gitTargetRef serves an EMPTY name after
// the upgrade prunes it, and its spec is immutable, so no apply can put the name back. The
// reconciler must say that once and stop, rather than retry a "get GitTarget" that names the empty
// string and can never succeed.
func TestCommitRequestReconciler_PrunedGitTargetRefIsTerminal(t *testing.T) {
	request := &configv1alpha3.CommitRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stored-before-the-rename",
			Namespace: "team-a",
			UID:       "cr-uid",
		},
		Spec: configv1alpha3.CommitRequestSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: ""},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(request).
		WithStatusSubresource(request).
		Build()

	finalizer := &fakeFinalizer{}
	reconciler := &CommitRequestReconciler{Client: k8sClient, Scheme: scheme, Finalizer: finalizer}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: request.Name, Namespace: request.Namespace},
	})

	require.NoError(t, err, "an unrepairable request is a terminal outcome, not a reconcile error")
	assert.Zero(t, result.RequeueAfter, "it must not be retried: no apply can ever fix it")
	assert.Empty(t, finalizer.calls, "the router must never be asked for GitTarget \"\"")

	var got configv1alpha3.CommitRequest
	require.NoError(t, k8sClient.Get(context.Background(),
		types.NamespacedName{Name: request.Name, Namespace: request.Namespace}, &got))
	assert.True(t, commitRequestIsTerminal(&got), "the request must reach a terminal condition")
	stalled := findCondition(got.Status.Conditions, ConditionTypeStalled)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionTrue, stalled.Status)
	assert.Equal(t, crReasonGitTargetRefPruned, stalled.Reason)
	assert.Contains(t, stalled.Message, "delete it and create a new one",
		"the message must name the only repair there is, because the spec is immutable")
}

func TestCommitRequestReconciler_LiteralValidation(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		valid         bool
	}{
		{"unicode", strings.Repeat("ü", 1024), true},
		{"literal", "  fix: {{.Author}}\n\nbody  ", true},
		{"whitespace", " \n ", false},
		{"oversize", strings.Repeat("ü", 1025), false},
		{"control", "bad\tmessage", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCommitRequest(tc.name)
			cr.Spec.Message = tc.message
			c := newCommitRequestClient(t, nil, cr)
			finalizer := &fakeFinalizer{}
			r := &CommitRequestReconciler{Client: c, Finalizer: finalizer}
			reconcileCommitRequest(t, r, cr.Name)
			if tc.valid {
				require.Len(t, finalizer.calls, 1)
				assert.Equal(t, tc.message, finalizer.calls[0].Message)
			} else {
				require.Empty(t, finalizer.calls)
				var got configv1alpha3.CommitRequest
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(cr), &got))
				assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
			}
		})
	}
}

// A CommitRequest may name a GitTarget in another namespace, which saves for it only when a grant in its
// spec.allowedCommitRequests admits the request's namespace. Omitted, the list admits the GitTarget's own
// namespace alone; a refused request fails at once, before anything is attached.
func TestCommitRequestAttach_AGitTargetElsewhereMustAdmitTheRequest(t *testing.T) {
	platformTarget := func(grants []configv1alpha3.GitTargetCommitRequestGrant) *configv1alpha3.GitTarget {
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "platform"},
			Spec:       configv1alpha3.GitTargetSpec{AllowedCommitRequests: grants},
		}
	}
	fromNames := func(names ...configv1alpha3.GrantNamespace) []configv1alpha3.GitTargetCommitRequestGrant {
		return []configv1alpha3.GitTargetCommitRequestGrant{{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromNames, Names: names},
		}}
	}
	cases := []struct {
		name     string
		grants   []configv1alpha3.GitTargetCommitRequestGrant
		admitted bool
	}{
		{"the default admits only the GitTarget's own namespace", nil, false},
		{"a grant for another namespace", fromNames("team-b"), false},
		{"a grant for the request's namespace", fromNames("default"), true},
		{"every namespace", []configv1alpha3.GitTargetCommitRequestGrant{{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromAll},
		}}, true},
		{"an empty list admits nobody", []configv1alpha3.GitTargetCommitRequestGrant{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCommitRequest("save")
			cr.Spec.GitTargetRef = meta.NamespacedObjectReference{Name: "shared", Namespace: "platform"}
			c := newCommitRequestClient(t, nil, cr, platformTarget(tc.grants))
			f := &fakeFinalizer{}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileCommitRequest(t, r, "save")

			if tc.admitted {
				require.NotEmpty(t, f.calls, "the request must be attached")
				assert.Equal(t, "platform", f.calls[0].GitTargetNamespace)
				assert.Equal(t, "shared", f.calls[0].GitTargetName)
				return
			}
			assert.Empty(t, f.calls, "a refused request is never attached")
			var got configv1alpha3.CommitRequest
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "save"}, &got))
			ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, crReasonNotAllowed, ready.Reason)
			assert.Contains(t, ready.Message, "spec.allowedCommitRequests")
			assert.True(t, apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeStalled))
		})
	}
}

// A request whose GitTarget does not exist is not attached: it waits, and once a GitTarget by that
// name appears it must admit the request first. Its earlier in-progress conditions grant nothing.
func TestCommitRequestAttach_AGitTargetCreatedLaterMustAdmitTheRequest(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef = meta.NamespacedObjectReference{Name: "later", Namespace: "platform"}
	c := newCommitRequestClient(t, nil, cr, &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "platform"},
	})
	f := &fakeFinalizer{resolved: true, result: git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee"}}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	res := reconcileCommitRequest(t, r, "save")
	assert.Empty(t, f.calls, "nothing is attached before a GitTarget admitted the request")
	assert.Positive(t, res.RequeueAfter, "it waits for the GitTarget")
	got := fetchCommitRequest(t, c, "save")
	require.NotNil(t, apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady), "it reports progress")
	assert.Empty(t, got.Status.GitTargetUID)

	require.NoError(t, c.Create(context.Background(), &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "later", Namespace: "platform", UID: "uid-later"},
		Spec:       configv1alpha3.GitTargetSpec{AllowedCommitRequests: []configv1alpha3.GitTargetCommitRequestGrant{}},
	}))
	reconcileCommitRequest(t, r, "save")

	assert.Empty(t, f.calls, "the GitTarget that appeared admits nobody")
	got = fetchCommitRequest(t, c, "save")
	ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, crReasonNotAllowed, ready.Reason)
}

// The GitTarget that admitted a request is recorded by UID, and a refusal by that same GitTarget
// afterwards, once its grants were lowered, does not fail a request an attach may already have reached.
func TestCommitRequestAttach_ALoweredGrantKeepsARequestItAdmitted(t *testing.T) {
	cr := newCommitRequest("save")
	target := &configv1alpha3.GitTarget{ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "default"}}
	c := newCommitRequestClient(t, nil, cr, target)
	f := &fakeFinalizer{}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save")
	require.Len(t, f.calls, 1)
	assert.Equal(t, string(target.UID), f.calls[0].GitTargetUID, "the attach is bound to the admitting GitTarget")
	got := fetchCommitRequest(t, c, "save")
	assert.Equal(t, string(target.UID), got.Status.GitTargetUID)

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(target), target))
	target.Spec.AllowedCommitRequests = []configv1alpha3.GitTargetCommitRequestGrant{}
	require.NoError(t, c.Update(context.Background(), target))
	reconcileCommitRequest(t, r, "save")

	assert.Len(t, f.calls, 2, "the attach is sent again, as before the grant was lowered")
	got = fetchCommitRequest(t, c, "save")
	ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.NotEqual(t, crReasonNotAllowed, ready.Reason)
}

// A GitTarget recreated with grants that refuse a request does not fail it while the worker the
// GitTarget it replaced sent it to may hold it: that worker answers first. A save it committed is
// reported as committed; one nothing held is refused; one it holds is waited for.
func TestCommitRequestAttach_ARefusingReplacementAsksTheWorkerFirst(t *testing.T) {
	tests := []struct {
		name       string
		withdraw   git.FinalizeResult
		resolved   bool
		wantReason string
		wantCommit string
	}{
		{
			name:       "the worker committed it",
			withdraw:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
			resolved:   true,
			wantReason: crReasonCommitted,
			wantCommit: "c0ffee",
		},
		{
			name:       "nothing held it",
			withdraw:   git.FinalizeResult{Err: git.ErrCommitRequestWithdrawn},
			resolved:   true,
			wantReason: crReasonNotAllowed,
		},
		{
			name:     "the worker holds it",
			withdraw: git.FinalizeResult{Phase: git.PhaseWaitingForPush},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCommitRequest("save")
			target := &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "default", UID: "uid-first"},
			}
			c := newCommitRequestClient(t, nil, cr, target)
			f := &fakeFinalizer{}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileCommitRequest(t, r, "save")
			require.Len(t, f.calls, 1)

			require.NoError(t, c.Delete(context.Background(), target))
			refusing := configv1alpha3.GitTargetSpec{
				AllowedCommitRequests: []configv1alpha3.GitTargetCommitRequestGrant{},
			}
			require.NoError(t, c.Create(context.Background(), &configv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "default", UID: "uid-second"},
				Spec:       refusing,
			}))
			f.withdrawResult, f.withdrawResolved, f.withdrawPending = tc.withdraw, tc.resolved, true
			res := reconcileCommitRequest(t, r, "save")

			assert.Len(t, f.calls, 1, "nothing is attached through the replacement")
			require.Len(t, f.withdrawCalls, 1)
			assert.Equal(t, "uid-first", f.withdrawCalls[0].GitTargetUID, "the worker is asked about the first one")
			got := fetchCommitRequest(t, c, "save")
			assert.Equal(t, "uid-first", got.Status.GitTargetUID)
			ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
			require.NotNil(t, ready)
			if tc.wantReason == "" {
				assert.Positive(t, res.RequeueAfter, "a held request is waited for")
				assert.NotEqual(t, metav1.ConditionTrue, ready.Status)
				assert.NotEqual(t, crReasonNotAllowed, ready.Reason)
				return
			}
			assert.Equal(t, tc.wantReason, ready.Reason)
			assert.Equal(t, tc.wantCommit, got.Status.Commit)
		})
	}
}
