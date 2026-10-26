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
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

// fakeFinalizer records the reconciler's attach calls and replies with a canned
// outcome. resolved=false models the worker still collecting (the controller polls).
type fakeFinalizer struct {
	result   git.FinalizeResult
	resolved bool
	err      error

	calls []git.AttachSaveRequest

	// The withdraw reply. Left unset, the withdraw answers like the router does before it cancels
	// anything: with the worker's outcome, or its phase while unresolved. withdrawPending makes an
	// unset reply mean "not answered yet" instead.
	withdrawResult   git.FinalizeResult
	withdrawResolved bool
	withdrawErr      error
	withdrawPending  bool
	withdrawCalls    []git.AttachSaveRequest

	// The apply a request with catchUp: AfterPush asked for, and the lookups made.
	apply        git.SaveApply
	applyKnown   bool
	applyErr     error
	applyLookups int
}

func (f *fakeFinalizer) LookupSaveRequestApply(
	_ context.Context, _ git.AttachSaveRequest,
) (git.SaveApply, bool, error) {
	f.applyLookups++
	return f.apply, f.applyKnown, f.applyErr
}

func (f *fakeFinalizer) WithdrawSaveRequest(
	_ context.Context, attach git.AttachSaveRequest,
) (git.FinalizeResult, bool, error) {
	f.withdrawCalls = append(f.withdrawCalls, attach)
	if f.withdrawPending || f.withdrawResolved || f.withdrawErr != nil || f.withdrawResult.Phase != "" {
		return f.withdrawResult, f.withdrawResolved, f.withdrawErr
	}
	return f.result, f.resolved, f.err
}

func (f *fakeFinalizer) ServiceSaveRequest(
	_ context.Context, attach git.AttachSaveRequest,
) (git.FinalizeResult, bool, error) {
	f.calls = append(f.calls, attach)
	return f.result, f.resolved, f.err
}

// fakeAuthorLookup is the command-author source stub: found=false models a miss
// (the validate-operator-types webhook is not configured, or a best-effort write missed),
// which the controller resolves to the committer immediately — present-or-never.
type fakeAuthorLookup struct {
	author queue.CommandAuthor
	found  bool
	calls  int
}

func (f *fakeAuthorLookup) LookupCommandAuthor(
	_ context.Context, _ types.UID,
) (queue.CommandAuthor, bool) {
	f.calls++
	return f.author, f.found
}

func attributedAlice() *fakeAuthorLookup {
	return &fakeAuthorLookup{author: queue.CommandAuthor{Author: "alice"}, found: true}
}

// newSaveRequest builds a request created just now, inside its safety window, as the API server
// would stamp it. pastTheBound ages one beyond it.
func newSaveRequest(name string) *configv1alpha3.SaveRequest {
	return &configv1alpha3.SaveRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.Now(),
		},
		Spec: configv1alpha3.SaveRequestSpec{
			GitTargetRef: meta.NamespacedObjectReference{Name: "team-a-config"},
			Message:      "save: " + name,
		},
	}
}

// pastTheBound ages a request beyond its safety window, where the controller only withdraws.
func pastTheBound(cr *configv1alpha3.SaveRequest) *configv1alpha3.SaveRequest {
	cr.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * resolveRequestWindow(cr.Spec).resolveTimeout()))
	return cr
}

// withReadyCommitted stamps a terminal Committed SaveRequest (Ready=True).
func withReadyCommitted(cr *configv1alpha3.SaveRequest) *configv1alpha3.SaveRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: crReasonCommitted, Message: "committed",
	})
	return cr
}

// withInProgress stamps the still-running conditions a post-restart redelivery
// would already carry (Ready=False, Reconciling=True), which is non-terminal.
func withInProgress(cr *configv1alpha3.SaveRequest) *configv1alpha3.SaveRequest {
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReady, Status: metav1.ConditionFalse,
		Reason: string(git.PhaseWaitingForWindow), Message: "in progress",
	})
	apimeta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionTypeReconciling, Status: metav1.ConditionTrue,
		Reason: string(git.PhaseWaitingForWindow), Message: "in progress",
	})
	return cr
}

func newSaveRequestClient(t *testing.T, fns *interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&configv1alpha3.SaveRequest{})
	if fns != nil {
		builder = builder.WithInterceptorFuncs(*fns)
	}
	return builder.Build()
}

func reconcileSaveRequest(t *testing.T, r *SaveRequestReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: name},
	})
	require.NoError(t, err)
	return res
}

func fetchSaveRequest(t *testing.T, c client.Client, name string) configv1alpha3.SaveRequest {
	t.Helper()
	var cr configv1alpha3.SaveRequest
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &cr))
	return cr
}

// requireCondition asserts a condition's status (and, when non-empty, reason) and
// returns it for further assertions.
func requireCondition(
	t *testing.T,
	cr configv1alpha3.SaveRequest,
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

func TestSaveRequestReconcile_Committed(t *testing.T) {
	cr := newSaveRequest("save-1")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-1")

	got := fetchSaveRequest(t, c, "save-1")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionTrue, crReasonAttributedFromAdmission)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionFalse, "")
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Equal(t, "abc123", got.Status.Commit)
	assert.Equal(t, "main", got.Status.Branch)

	require.Len(t, f.calls, 1)
	call := f.calls[0]
	assert.Equal(t, "alice", call.Author,
		"the attach must carry the author captured at admission")
	assert.Equal(t, "team-a-config", call.GitTargetName)
	assert.Equal(t, "default", call.GitTargetNamespace)
	assert.Equal(t, "save: save-1", call.Message)
	assert.Equal(t, "save-1", call.Name)
	assert.Equal(t, "uid-save-1", call.UID)
}

func TestSaveRequestReconcile_NoOpenWindow(t *testing.T) {
	cr := newSaveRequest("save-now")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-now")

	got := fetchSaveRequest(t, c, "save-now")
	// A benign no-commit is Ready=True (serviced) with the specific reason, Pushed=False.
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindow)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindow)
	requireCondition(t, got, ConditionTypeStalled, metav1.ConditionFalse, "")
	assert.Empty(t, got.Status.Commit)
}

// whenNothingToCommit: CommitEmpty reaches the worker, and a recorded empty commit reports its
// commit and Pushed=True while the Ready reason keeps the cause.
func TestSaveRequestReconcile_CommitEmptyRecordsTheMessageAndKeepsTheCause(t *testing.T) {
	for _, tc := range []struct {
		outcome git.FinalizeOutcome
		reason  string
	}{
		{git.FinalizeNoOpenWindow, crReasonNoWindow},
		{git.FinalizeAlreadyPresent, crReasonAlreadyPresent},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			cr := newSaveRequest("save-empty")
			cr.Spec.WhenNothingToCommit = configv1alpha3.NothingToCommitCommitEmpty
			c := newSaveRequestClient(t, nil, cr)
			f := &fakeFinalizer{
				result:   git.FinalizeResult{Outcome: tc.outcome, Commit: "abc123", Branch: "main"},
				resolved: true,
			}
			r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

			reconcileSaveRequest(t, r, "save-empty")

			require.Len(t, f.calls, 1)
			assert.True(t, f.calls[0].CommitEmpty, "the choice must reach the worker")
			got := fetchSaveRequest(t, c, "save-empty")
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
// open and the SaveRequest reports the WindowMismatch reason (no commit).
func TestSaveRequestReconcile_WindowMismatchIsExplained(t *testing.T) {
	cr := newSaveRequest("save-foreign")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-foreign")

	got := fetchSaveRequest(t, c, "save-foreign")
	ready := requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
	assert.Equal(t, windowMismatchMessage, ready.Message)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonWindowMismatch)
	assert.Empty(t, got.Status.Commit)
}

// A matching window that finalized with no diff (loop prevention) reports the
// AlreadyPresent reason — never left hanging.
func TestSaveRequestReconcile_AlreadyPresentRejected(t *testing.T) {
	cr := newSaveRequest("save-noop")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-noop")

	got := fetchSaveRequest(t, c, "save-noop")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, crReasonAlreadyPresent)
	assert.Empty(t, got.Status.Commit)
}

// A resolved outcome that carries an error becomes a Failed (Stalled=True) request.
func TestSaveRequestReconcile_FinalizeErrorBecomesFailed(t *testing.T) {
	cr := newSaveRequest("save-fail")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Err: errors.New("commit failed: unreachable remote")},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-fail")

	got := fetchSaveRequest(t, c, "save-fail")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Contains(t, stalled.Message, "unreachable remote")
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionFalse, "")
}

// TestSaveRequestReconcile_LookupMissClaimsNoActor verifies that a lookup miss claims no actor
// immediately — present-or-never, no wait. A freshly created SaveRequest attaches at once with a
// blank author and records AuthorAttributed=False (CommitterFallback). The matching window determines
// the Git author.
func TestSaveRequestReconcile_LookupMissClaimsNoActor(t *testing.T) {
	cr := newSaveRequest("save-fresh")
	cr.CreationTimestamp = metav1.Now() // fresh: the old path would have requeued here
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Branch: "main"},
		resolved: true,
	}
	lookup := &fakeAuthorLookup{found: false}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	res := reconcileSaveRequest(t, r, "save-fresh")

	assert.Zero(t, res.RequeueAfter, "a miss is final; the request must not poll for an author")
	assert.Equal(t, 1, lookup.calls, "the author is looked up exactly once, synchronously")
	require.Len(t, f.calls, 1, "the attach is sent immediately on a miss")
	assert.Empty(t, f.calls[0].Author, "a miss attaches with a blank, unnamed actor")

	got := fetchSaveRequest(t, c, "save-fresh")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionFalse, crReasonCommitterFallback)
}

// A request whose window was never defaulted must reach the worker carrying the schema defaults,
// not zeros. The API server normally fills the block in, so nil arrives here only from a client
// that bypassed defaulting — and a zero there would be the old defect: the request expires before
// the write it exists to publish can open a window.
func TestSaveRequestReconcile_AnOmittedWindowUsesTheDefaults(t *testing.T) {
	cr := newSaveRequest("save-default")
	cr.CreationTimestamp = metav1.Now()
	require.Nil(t, cr.Spec.Window, "the fixture must leave the block unset")
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-default")

	require.Len(t, f.calls, 1)
	assert.Equal(t, configv1alpha3.AttachCurrentOrNext, f.calls[0].Attach)
	assert.Equal(t, defaultAttachTimeout, f.calls[0].AttachTimeout)
	assert.Equal(t, defaultMaxDuration, f.calls[0].MaxDuration)
	assert.Nil(t, f.calls[0].IdleTimeout, "no idle close unless one is asked for")
}

// The pointer types exist so that an explicit zero survives defaulting, and each field keeps its
// own meaning: a zero attachTimeout is not a zero maxDuration.
func TestSaveRequestReconcile_ExplicitZerosArePreservedFieldByField(t *testing.T) {
	cr := newSaveRequest("save-now")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.Window = &configv1alpha3.SaveRequestWindow{
		AttachTimeout: &metav1.Duration{},
		IdleTimeout:   &metav1.Duration{},
		MaxDuration:   &metav1.Duration{Duration: 30 * time.Second},
	}
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-now")

	require.Len(t, f.calls, 1)
	assert.Zero(t, f.calls[0].AttachTimeout, "an explicit \"0s\" must not be re-read as an omitted field")
	require.NotNil(t, f.calls[0].IdleTimeout)
	assert.Zero(t, *f.calls[0].IdleTimeout)
	assert.Equal(t, 30*time.Second, f.calls[0].MaxDuration, "a zero attachTimeout never forces a zero collection")
}

// The window is the worker's job: the controller does not hold the finalize itself. While the
// worker has not resolved the attach, the controller polls and reports the phase the WORKER
// reports, never one it inferred from having sent the attach.
func TestSaveRequestReconcile_NotResolvedReportsTheWorkersPhase(t *testing.T) {
	cr := newSaveRequest("save-linger")
	cr.CreationTimestamp = metav1.Now()
	cr.Spec.Window = &configv1alpha3.SaveRequestWindow{MaxDuration: &metav1.Duration{Duration: 30 * time.Second}}
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	res := reconcileSaveRequest(t, r, "save-linger")

	assert.Equal(t, saveRequestPollInterval, res.RequeueAfter,
		"an unresolved attach must be polled, not held by a controller-side delay")
	require.Len(t, f.calls, 1, "the attach is sent the instant the author is known")
	assert.Equal(t, 30*time.Second, f.calls[0].MaxDuration,
		"the window is passed to the worker, not consumed here")

	got := fetchSaveRequest(t, c, "save-linger")
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionTrue, crReasonAttributedFromAdmission)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, ReasonProgressing)
	assert.False(t, saveRequestIsTerminal(&got), "an unresolved request is not terminal")

	for _, phase := range []git.SaveRequestPhase{
		git.PhaseWaitingForWindow, git.PhaseCollectingWindow, git.PhaseWaitingForPush,
	} {
		f.result = git.FinalizeResult{Phase: phase}
		reconcileSaveRequest(t, r, "save-linger")
		got = fetchSaveRequest(t, c, "save-linger")
		requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, string(phase))
		requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, string(phase))
	}

	// A phase the worker has not (yet) reported keeps what is shown rather than guessing.
	f.result = git.FinalizeResult{}
	reconcileSaveRequest(t, r, "save-linger")
	got = fetchSaveRequest(t, c, "save-linger")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, string(git.PhaseWaitingForPush))
}

// A transient service error (e.g. the GitTarget momentarily unreadable) keeps the
// request polling within the safety window rather than failing it outright.
func TestSaveRequestReconcile_ServiceErrorPolls(t *testing.T) {
	cr := newSaveRequest("save-transient")
	cr.CreationTimestamp = metav1.Now()
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{err: errors.New("get GitTarget: not found")}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	res := reconcileSaveRequest(t, r, "save-transient")

	assert.Equal(t, saveRequestPollInterval, res.RequeueAfter)
	got := fetchSaveRequest(t, c, "save-transient")
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, ReasonProgressing)
	assert.False(t, saveRequestIsTerminal(&got))
}

// Past the safety window a request the worker never acted on is withdrawn, and fails closed once
// the worker confirms the withdraw.
func TestSaveRequestReconcile_ResolveTimeoutFailsClosed(t *testing.T) {
	cr := pastTheBound(newSaveRequest("save-stuck"))
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:           git.FinalizeResult{Phase: git.PhaseWaitingForWindow},
		withdrawResult:   git.FinalizeResult{Branch: "main", Err: git.ErrSaveRequestWithdrawn},
		withdrawResolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-stuck")

	require.Len(t, f.withdrawCalls, 1, "the request is withdrawn before it is failed")
	assert.Empty(t, f.calls, "past the bound no attach is sent: it would queue behind the withdraw")
	got := fetchSaveRequest(t, c, "save-stuck")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Equal(t, resolveTimeoutMessage, stalled.Message)
}

// A request the worker holds is never failed on the controller's clock: a down remote or a parent
// branch recovery can keep it in WaitingForPush far past the bound, and the worker still commits it.
func TestSaveRequestReconcile_AHeldRequestOutlivesTheBound(t *testing.T) {
	for _, phase := range []git.SaveRequestPhase{git.PhaseCollectingWindow, git.PhaseWaitingForPush} {
		t.Run(string(phase), func(t *testing.T) {
			cr := pastTheBound(newSaveRequest("save-held"))
			c := newSaveRequestClient(t, nil, cr)
			f := &fakeFinalizer{result: git.FinalizeResult{Phase: phase}}
			r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

			res := reconcileSaveRequest(t, r, "save-held")

			assert.Equal(t, saveRequestPollInterval, res.RequeueAfter)
			assert.Empty(t, f.calls, "past the bound no attach is sent")
			assert.Len(t, f.withdrawCalls, 1, "the withdraw finds the request held and changes nothing")
			got := fetchSaveRequest(t, c, "save-held")
			assert.False(t, saveRequestIsTerminal(&got))

			// The push lands much later: the request resolves Committed, not FinalizeFailed.
			f.result = git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc", Branch: "main"}
			f.resolved = true
			reconcileSaveRequest(t, r, "save-held")
			got = fetchSaveRequest(t, c, "save-held")
			requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		})
	}
}

// Until the worker answers the withdraw the request keeps polling; and when the worker turns out to
// hold it after all (it attached between the poll and the withdraw), nothing fails.
func TestSaveRequestReconcile_AnUnansweredWithdrawKeepsPolling(t *testing.T) {
	for name, reply := range map[string]git.FinalizeResult{
		"not handled yet":         {},
		"the worker now holds it": {Phase: git.PhaseCollectingWindow},
	} {
		t.Run(name, func(t *testing.T) {
			cr := pastTheBound(newSaveRequest("save-race"))
			c := newSaveRequestClient(t, nil, cr)
			f := &fakeFinalizer{
				result:          git.FinalizeResult{Phase: git.PhaseWaitingForWindow},
				withdrawResult:  reply,
				withdrawPending: true,
			}
			r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

			res := reconcileSaveRequest(t, r, "save-race")

			assert.Equal(t, saveRequestPollInterval, res.RequeueAfter)
			got := fetchSaveRequest(t, c, "save-race")
			assert.False(t, saveRequestIsTerminal(&got))
		})
	}
}

// A worker that resolved the request on its own just before the withdraw wins: its real outcome is
// what the request reports.
func TestSaveRequestReconcile_AWithdrawThatLostTheRaceReportsTheRealOutcome(t *testing.T) {
	cr := pastTheBound(newSaveRequest("save-late"))
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		withdrawResult:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc", Branch: "main"},
		withdrawResolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-late")

	got := fetchSaveRequest(t, c, "save-late")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	assert.Equal(t, "abc", got.Status.Commit)
}

// A GitTarget that never started a branch worker is named as the cause.
func TestSaveRequestReconcile_NoWorkerIsNamedWhenItFailsClosed(t *testing.T) {
	cr := pastTheBound(newSaveRequest("save-no-worker"))
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result: git.FinalizeResult{Phase: git.PhaseWaitingForWorker},
		withdrawResult: git.FinalizeResult{
			Branch: "main", Phase: git.PhaseWaitingForWorker, Err: git.ErrSaveRequestWithdrawn,
		},
		withdrawResolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-no-worker")

	got := fetchSaveRequest(t, c, "save-no-worker")
	stalled := requireCondition(t, got, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
	assert.Equal(t, noWorkerTimeoutMessage, stalled.Message)
}

func TestSaveRequestReconcile_TerminalShortCircuits(t *testing.T) {
	cr := withReadyCommitted(newSaveRequest("save-done"))
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{}
	lookup := attributedAlice()
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

	reconcileSaveRequest(t, r, "save-done")

	assert.Zero(t, lookup.calls)
	assert.Empty(t, f.calls, "a terminal SaveRequest must never re-attach")
}

// A reconcile triggered by a stale cache echo (the cached object still says
// in-progress while the live object is already terminal) must not re-run the
// finalize: the uncached APIReader read is the guard.
func TestSaveRequestReconcile_StaleCacheEchoDoesNotRefinalize(t *testing.T) {
	stale := withInProgress(newSaveRequest("save-echo"))
	cached := newSaveRequestClient(t, nil, stale)
	terminal := withReadyCommitted(newSaveRequest("save-echo"))
	live := newSaveRequestClient(t, nil, terminal)

	f := &fakeFinalizer{}
	r := &SaveRequestReconciler{Client: cached, APIReader: live, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-echo")

	assert.Empty(t, f.calls)
}

// Webhook-disabled mode (no AuthorLookup) never waits: a freshly created SaveRequest
// attaches immediately with a blank author and records AuthorAttributed=False
// (AuthorCaptureDisabled). The matching window determines the Git author.
func TestSaveRequestReconcile_ConfiguredAuthorCommitsWithoutWaiting(t *testing.T) {
	cr := newSaveRequest("save-configured-author")
	cr.CreationTimestamp = metav1.Now() // fresh: a waiting path would requeue instead of commit
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f} // AuthorLookup nil

	res := reconcileSaveRequest(t, r, "save-configured-author")

	assert.Zero(t, res.RequeueAfter, "webhook-disabled mode must not requeue waiting for an author")
	got := fetchSaveRequest(t, c, "save-configured-author")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	requireCondition(t, got, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionFalse, crReasonAuthorCaptureDisabled)
	assert.Equal(t, "c0ffee", got.Status.Commit)
	require.Len(t, f.calls, 1, "the attach is sent immediately, with no attribution wait")
	assert.Empty(t, f.calls[0].Author, "webhook-disabled mode attaches with a blank author")
}

// Webhook-disabled mode settles AuthorAttributed=False (AuthorCaptureDisabled) immediately
// and never parks the request in a "waiting for the author" state: even while the
// attach is still being polled, the request reports progress.
func TestSaveRequestReconcile_ConfiguredAuthorAttributedImmediately(t *testing.T) {
	cr := newSaveRequest("save-committer-poll")
	cr.CreationTimestamp = metav1.Now()
	c := newSaveRequestClient(t, nil, cr)
	f := &fakeFinalizer{resolved: false}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f} // AuthorLookup nil

	res := reconcileSaveRequest(t, r, "save-committer-poll")

	assert.Equal(t, saveRequestPollInterval, res.RequeueAfter, "an unresolved attach is polled")
	require.Len(t, f.calls, 1)
	assert.Empty(t, f.calls[0].Author)

	got := fetchSaveRequest(t, c, "save-committer-poll")
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionFalse, crReasonAuthorCaptureDisabled)
	requireCondition(t, got, ConditionTypeReconciling, metav1.ConditionTrue, ReasonProgressing)
	assert.False(t, saveRequestIsTerminal(&got), "in progress, not terminal")
}

// With no Finalizer wired the controller is inert: it neither attaches, stamps any
// condition, nor drives the request to a terminal outcome. (Production always wires
// the Finalizer; this is the disabled guard at the top of Reconcile.)
func TestSaveRequestReconcile_NilFinalizerIsInert(t *testing.T) {
	cr := newSaveRequest("save-no-finalizer")
	c := newSaveRequestClient(t, nil, cr)
	r := &SaveRequestReconciler{Client: c, APIReader: c} // Finalizer nil, AuthorLookup nil

	res := reconcileSaveRequest(t, r, "save-no-finalizer")

	assert.Zero(t, res.RequeueAfter)
	got := fetchSaveRequest(t, c, "save-no-finalizer")
	assert.Empty(t, got.Status.Conditions, "a disabled controller stamps nothing")
	assert.False(t, saveRequestIsTerminal(&got))
}

func TestSaveRequestReconcile_ObjectDeletedIsBenign(t *testing.T) {
	c := newSaveRequestClient(t, nil)
	f := &fakeFinalizer{}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "gone")

	assert.Empty(t, f.calls)
}

func TestSaveRequestReconcile_TerminalWriteRetriesOnConflict(t *testing.T) {
	// Already in-progress: this models a post-restart redelivery, so the only
	// status write is the terminal one — the write the conflict hits.
	cr := withInProgress(newSaveRequest("save-retry"))

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
					schema.GroupResource{Group: "configbutler.ai", Resource: "saverequests"},
					obj.GetName(), errors.New("simulated"))
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
	c := newSaveRequestClient(t, &fns, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "ddd111", Branch: "main"},
		resolved: true,
	}
	r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileSaveRequest(t, r, "save-retry")

	got := fetchSaveRequest(t, c, "save-retry")
	requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
	assert.Equal(t, "ddd111", got.Status.Commit)
	require.Len(t, f.calls, 1, "the conflict retry must re-write status, not re-attach")
}

// --- pure helpers (formerly internal/queue/save_request_test.go) ---

func TestApplyFinalizeResultToStatus(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(
			&cr,
			git.FinalizeResult{
				Outcome: git.FinalizeCommitted,
				Commit:  "abc",
				Branch:  "main",
			},
			nil,
			attributionFromAdmission,
		)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionTrue, ReasonSucceeded)
		requireCondition(t, cr, ConditionTypeReconciling, metav1.ConditionFalse, "")
		requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionFalse, "")
		requireCondition(t, cr, ConditionTypeAuthorAttributed, metav1.ConditionTrue, crReasonAttributedFromAdmission)
		assert.Equal(t, "abc", cr.Status.Commit)
		assert.Equal(t, "main", cr.Status.Branch)
	})

	t.Run("no window in grace is a benign ready (no admission author)", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeNoOpenWindow, Branch: "main"}, nil, attributionCommitter)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonNoWindow)
		requireCondition(t, cr, ConditionTypePushed, metav1.ConditionFalse, crReasonNoWindow)
		requireCondition(t, cr, ConditionTypeAuthorAttributed, metav1.ConditionFalse, crReasonCommitterFallback)
		assert.Empty(t, cr.Status.Commit)
	})

	t.Run("window mismatch surfaces the reason", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeWindowMismatch, Branch: "main"}, nil, attributionFromAdmission)
		ready := requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonWindowMismatch)
		assert.Equal(t, windowMismatchMessage, ready.Message)
	})

	t.Run("already present is a benign ready", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeAlreadyPresent, Branch: "main"}, nil, attributionFromAdmission)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionTrue, crReasonAlreadyPresent)
		assert.Empty(t, cr.Status.Commit)
	})

	t.Run("finalize error stalls", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(&cr,
			git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "abc"},
			errors.New("boom"), attributionFromAdmission)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionFalse, crReasonFinalizeFailed)
		stalled := requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionTrue, crReasonFinalizeFailed)
		assert.Equal(t, "boom", stalled.Message)
	})

	t.Run("unknown outcome stalls", func(t *testing.T) {
		var cr configv1alpha3.SaveRequest
		applyFinalizeResultToStatus(&cr, git.FinalizeResult{}, nil, attributionFromAdmission)
		requireCondition(t, cr, ConditionTypeReady, metav1.ConditionFalse, crReasonUnexpectedOutcome)
		stalled := requireCondition(t, cr, ConditionTypeStalled, metav1.ConditionTrue, crReasonUnexpectedOutcome)
		assert.Contains(t, stalled.Message, "unexpected finalize outcome")
	})
}

func TestSaveRequestIsTerminal(t *testing.T) {
	assert.False(t, saveRequestIsTerminal(newSaveRequest("empty")))
	assert.False(t, saveRequestIsTerminal(withInProgress(newSaveRequest("in-progress"))))
	assert.True(t, saveRequestIsTerminal(withReadyCommitted(newSaveRequest("committed"))))

	stalled := newSaveRequest("failed")
	apimeta.SetStatusCondition(&stalled.Status.Conditions, metav1.Condition{
		Type: ConditionTypeStalled, Status: metav1.ConditionTrue, Reason: crReasonFinalizeFailed, Message: "boom",
	})
	assert.True(t, saveRequestIsTerminal(stalled))
}

// A SaveRequest stored before spec.targetRef became spec.gitTargetRef serves an EMPTY name after
// the upgrade prunes it, and its spec is immutable, so no apply can put the name back. The
// reconciler must say that once and stop, rather than retry a "get GitTarget" that names the empty
// string and can never succeed.
func TestSaveRequestReconciler_PrunedGitTargetRefIsTerminal(t *testing.T) {
	request := &configv1alpha3.SaveRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stored-before-the-rename",
			Namespace: "team-a",
			UID:       "cr-uid",
		},
		Spec: configv1alpha3.SaveRequestSpec{
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
	reconciler := &SaveRequestReconciler{Client: k8sClient, Scheme: scheme, Finalizer: finalizer}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: request.Name, Namespace: request.Namespace},
	})

	require.NoError(t, err, "an unrepairable request is a terminal outcome, not a reconcile error")
	assert.Zero(t, result.RequeueAfter, "it must not be retried: no apply can ever fix it")
	assert.Empty(t, finalizer.calls, "the router must never be asked for GitTarget \"\"")

	var got configv1alpha3.SaveRequest
	require.NoError(t, k8sClient.Get(context.Background(),
		types.NamespacedName{Name: request.Name, Namespace: request.Namespace}, &got))
	assert.True(t, saveRequestIsTerminal(&got), "the request must reach a terminal condition")
	stalled := findCondition(got.Status.Conditions, ConditionTypeStalled)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionTrue, stalled.Status)
	assert.Equal(t, crReasonGitTargetRefPruned, stalled.Reason)
	assert.Contains(t, stalled.Message, "delete it and create a new one",
		"the message must name the only repair there is, because the spec is immutable")
}

func TestSaveRequestReconciler_LiteralValidation(t *testing.T) {
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
			cr := newSaveRequest(tc.name)
			cr.Spec.Message = tc.message
			c := newSaveRequestClient(t, nil, cr)
			finalizer := &fakeFinalizer{}
			r := &SaveRequestReconciler{Client: c, Finalizer: finalizer}
			reconcileSaveRequest(t, r, cr.Name)
			if tc.valid {
				require.Len(t, finalizer.calls, 1)
				assert.Equal(t, tc.message, finalizer.calls[0].Message)
			} else {
				require.Empty(t, finalizer.calls)
				var got configv1alpha3.SaveRequest
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(cr), &got))
				assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
			}
		})
	}
}

// A request claims a window whose writes name no author only when it asked, its GitTarget allows
// what it asked, and the GitTarget's KubeProvider has never received an audit fact. A request that
// asks for more than its GitTarget allows fails at once, before anything is attached.
func TestSaveRequestAttach_ClaimsUnattributedWindowsOnlyWhenAskedAllowedAndUnaudited(t *testing.T) {
	target := func(policy configv1alpha3.UnattributedWindowPolicy) *configv1alpha3.GitTarget {
		gt := &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-config", Namespace: "default"},
			Spec: configv1alpha3.GitTargetSpec{
				KubeProviderRef: meta.NamespacedObjectReference{Name: "workspace", Namespace: "providers"},
			},
		}
		if policy != "" {
			gt.Spec.AllowedSaveRequests = []configv1alpha3.GitTargetSaveRequestGrant{{
				ConsumerNamespaces:  configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
				UnattributedWindows: policy,
			}}
		}
		return gt
	}
	provider := func(facts metav1.ConditionStatus) *configv1alpha3.KubeProvider {
		p := &configv1alpha3.KubeProvider{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "providers"}}
		if facts != "" {
			p.Status.Conditions = []metav1.Condition{{
				Type: KubeProviderConditionAuditFactsReceived, Status: facts, Reason: "Test",
			}}
		}
		return p
	}
	unaudited := provider(metav1.ConditionUnknown)
	cases := []struct {
		name    string
		ask     configv1alpha3.WindowAuthor
		objects []client.Object
		want    git.UnattributedClaim
		refused bool
	}{
		{
			"own asks nothing",
			configv1alpha3.WindowAuthorOwn,
			[]client.Object{
				target(configv1alpha3.UnattributedWindowsTakeOver),
				unaudited,
			},
			git.UnattributedClaimNone,
			false,
		},
		{
			"takeover allowed",
			configv1alpha3.WindowAuthorTakeOver,
			[]client.Object{
				target(configv1alpha3.UnattributedWindowsTakeOver),
				unaudited,
			},
			git.UnattributedClaimTakeOver,
			false,
		},
		{
			"saved-by under a takeover policy",
			configv1alpha3.WindowAuthorSavedBy,
			[]client.Object{
				target(configv1alpha3.UnattributedWindowsTakeOver),
				unaudited,
			},
			git.UnattributedClaimSavedBy,
			false,
		},
		{
			"saved-by allowed",
			configv1alpha3.WindowAuthorSavedBy,
			[]client.Object{
				target(configv1alpha3.UnattributedWindowsSavedBy),
				unaudited,
			},
			git.UnattributedClaimSavedBy,
			false,
		},
		{
			"author attribution off",
			configv1alpha3.WindowAuthorTakeOver,
			[]client.Object{
				target(configv1alpha3.UnattributedWindowsTakeOver),
				provider(""),
			},
			git.UnattributedClaimTakeOver,
			false,
		},
		{"audit facts received", configv1alpha3.WindowAuthorTakeOver,
			[]client.Object{target(configv1alpha3.UnattributedWindowsTakeOver), provider(metav1.ConditionTrue)},
			git.UnattributedClaimNone, false},
		{"KubeProvider missing", configv1alpha3.WindowAuthorTakeOver,
			[]client.Object{target(configv1alpha3.UnattributedWindowsTakeOver)}, git.UnattributedClaimNone, false},
		{"takeover under a saved-by policy", configv1alpha3.WindowAuthorTakeOver,
			[]client.Object{target(configv1alpha3.UnattributedWindowsSavedBy), unaudited}, "", true},
		{"saved-by under the default policy", configv1alpha3.WindowAuthorSavedBy,
			[]client.Object{target(""), unaudited}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newSaveRequest("save")
			cr.Spec.Window = &configv1alpha3.SaveRequestWindow{Author: tc.ask}
			c := newSaveRequestClient(t, nil, append([]client.Object{cr}, tc.objects...)...)
			f := &fakeFinalizer{}
			r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileSaveRequest(t, r, "save")

			if tc.refused {
				assert.Empty(t, f.calls, "a refused request is never attached")
				var got configv1alpha3.SaveRequest
				require.NoError(
					t,
					c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "save"}, &got),
				)
				ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionFalse, ready.Status)
				assert.Equal(t, crReasonTakeOverNotAllowed, ready.Reason)
				assert.True(t, apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeStalled))
				return
			}
			require.NotEmpty(t, f.calls, "the request must be attached")
			assert.Equal(t, tc.want, f.calls[0].Unattributed)
		})
	}
}

// A SaveRequest may name a GitTarget in another namespace, which saves for it only when a grant in its
// spec.allowedSaveRequests admits the request's namespace. Omitted, the list admits the GitTarget's own
// namespace alone; a refused request fails at once, before anything is attached.
func TestSaveRequestAttach_AGitTargetElsewhereMustAdmitTheRequest(t *testing.T) {
	platformTarget := func(grants []configv1alpha3.GitTargetSaveRequestGrant) *configv1alpha3.GitTarget {
		return &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "platform"},
			Spec:       configv1alpha3.GitTargetSpec{AllowedSaveRequests: grants},
		}
	}
	fromNames := func(names ...configv1alpha3.GrantNamespace) []configv1alpha3.GitTargetSaveRequestGrant {
		return []configv1alpha3.GitTargetSaveRequestGrant{{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromNames, Names: names},
		}}
	}
	cases := []struct {
		name     string
		grants   []configv1alpha3.GitTargetSaveRequestGrant
		admitted bool
	}{
		{"the default admits only the GitTarget's own namespace", nil, false},
		{"a grant for another namespace", fromNames("team-b"), false},
		{"a grant for the request's namespace", fromNames("default"), true},
		{"every namespace", []configv1alpha3.GitTargetSaveRequestGrant{{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromAll},
		}}, true},
		{"an empty list admits nobody", []configv1alpha3.GitTargetSaveRequestGrant{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newSaveRequest("save")
			cr.Spec.GitTargetRef = meta.NamespacedObjectReference{Name: "shared", Namespace: "platform"}
			c := newSaveRequestClient(t, nil, cr, platformTarget(tc.grants))
			f := &fakeFinalizer{}
			r := &SaveRequestReconciler{Client: c, APIReader: c, Finalizer: f}

			reconcileSaveRequest(t, r, "save")

			if tc.admitted {
				require.NotEmpty(t, f.calls, "the request must be attached")
				assert.Equal(t, "platform", f.calls[0].GitTargetNamespace)
				assert.Equal(t, "shared", f.calls[0].GitTargetName)
				return
			}
			assert.Empty(t, f.calls, "a refused request is never attached")
			var got configv1alpha3.SaveRequest
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "save"}, &got))
			ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, crReasonNotAllowed, ready.Reason)
			assert.Contains(t, ready.Message, "spec.allowedSaveRequests")
			assert.True(t, apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeStalled))
		})
	}
}
