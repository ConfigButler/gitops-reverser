// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// drainingWorker is a branch worker that holds a request, collecting its window, until the shutdown
// settles it: then it has decided the request's outcome and stopped, and only a lookup reads it.
type drainingWorker struct {
	mu       sync.Mutex
	outcome  git.FinalizeResult
	stopped  bool
	attaches int
	lookups  int
}

func (w *drainingWorker) stop(outcome git.FinalizeResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outcome, w.stopped = outcome, true
}

func (w *drainingWorker) ServiceCommitRequest(
	context.Context, git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attaches++
	if w.stopped {
		return w.outcome, true, nil
	}
	return git.FinalizeResult{Phase: git.PhaseCollectingWindow}, false, nil
}

func (w *drainingWorker) WithdrawCommitRequest(
	ctx context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	return w.ServiceCommitRequest(ctx, attach)
}

func (w *drainingWorker) LookupCommitRequestOutcome(
	context.Context, git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lookups++
	return w.outcome, w.stopped, nil
}

func (w *drainingWorker) LookupCommitRequestApply(
	context.Context, git.AttachCommitRequest,
) (git.SaveApply, bool, error) {
	return git.SaveApply{}, false, nil
}

// counts are the attaches and the lookups the worker was asked for.
func (w *drainingWorker) counts() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attaches, w.lookups
}

var shutdownCommit = git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"}

// The race a clean shutdown had: the reconciler attached the save, the worker settled it while it
// stopped, and no step of the reconciler ran after that to read the outcome. Settle writes it, from
// the worker's lookup alone, without attaching the request again.
func TestCommitRequestsInFlight_TheShutdownWritesWhatTheWorkerSettled(t *testing.T) {
	c := newCommitRequestClient(t, nil, newCommitRequest("save"))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	r := &CommitRequestReconciler{Client: c, Finalizer: worker, InFlight: inFlight}
	key := types.NamespacedName{Namespace: "default", Name: "save"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.Equal(t, string(git.PhaseCollectingWindow), commitRequestReady(t, c, "save").Reason)
	attachedBefore, _ := worker.counts()

	worker.stop(shutdownCommit)
	inFlight.Settle(context.Background())

	stored := fetchCommitRequest(t, c, "save")
	assert.Equal(t, crReasonCommitted, commitRequestReady(t, c, "save").Reason)
	assert.Equal(t, "c0ffee", stored.Status.Commit)
	attaches, lookups := worker.counts()
	assert.Equal(t, attachedBefore, attaches, "the shutdown attaches nothing")
	assert.Equal(t, 1, lookups)
	assert.Empty(t, inFlight.remembered(), "a written outcome is forgotten")
}

// A failure the worker decided at shutdown, its last push refused, is written too: the request
// reports it instead of being sent to a new worker that never saw it.
func TestCommitRequestsInFlight_AFailureAtShutdownIsWritten(t *testing.T) {
	c := newCommitRequestClient(t, nil, withInProgress(newCommitRequest("save")))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	core := &commitRequestCore{reader: c, finalizer: worker, inFlight: inFlight}
	serveStep(t, core, controlPath(c), "save")

	worker.stop(git.FinalizeResult{Branch: "main", Err: git.ErrBranchWorkerStopped})
	inFlight.Settle(context.Background())

	stalled := conditionIsTrue(fetchCommitRequest(t, c, "save").Status.Conditions, ConditionTypeStalled)
	assert.True(t, stalled, "the request fails with the worker's answer")
}

// A request no worker decided anything for, waiting for a window or still on its way to one, keeps its
// status: the next process attaches it again from there. Settle writes nothing for it.
func TestCommitRequestsInFlight_ARequestNoWorkerDecidedIsLeftForTheNextProcess(t *testing.T) {
	c := newCommitRequestClient(t, nil, withInProgress(newCommitRequest("save")))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	core := &commitRequestCore{reader: c, finalizer: worker, inFlight: inFlight}
	serveStep(t, core, controlPath(c), "save")
	before := fetchCommitRequest(t, c, "save")

	inFlight.Settle(context.Background())

	after := fetchCommitRequest(t, c, "save")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is written")
	assert.Len(t, inFlight.remembered(), 1, "it stays in flight")
}

// Settle's write is the owner's write: a conflict reads the request again and applies the worker's
// outcome onto it, and an outcome already written is left alone.
func TestCommitRequestsInFlight_AConflictReappliesTheOutcome(t *testing.T) {
	conflicts := 1
	fns := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			cr, ok := obj.(*configv1alpha3.CommitRequest)
			if ok && conflicts > 0 && commitRequestIsTerminal(cr) {
				conflicts--
				return apierrors.NewConflict(commitRequestGVR().GroupResource(), cr.Name, nil)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	c := newCommitRequestClient(t, &fns, withInProgress(newCommitRequest("save")))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	serveStep(t, &commitRequestCore{reader: c, finalizer: worker, inFlight: inFlight}, controlPath(c), "save")

	worker.stop(shutdownCommit)
	inFlight.Settle(context.Background())

	assert.Zero(t, conflicts)
	assert.Equal(t, crReasonCommitted, commitRequestReady(t, c, "save").Reason)
	assert.Equal(t, "c0ffee", fetchCommitRequest(t, c, "save").Status.Commit)
}

// A conflict whose fresh object holds an outcome already, written by a step that ran before the stop,
// leaves that outcome, and the request is finished: Settle forgets it rather than listing it as left
// without one.
func TestCommitRequestsInFlight_AnOutcomeWrittenFirstIsForgotten(t *testing.T) {
	conflicts := 1
	fns := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			cr, ok := obj.(*configv1alpha3.CommitRequest)
			if ok && conflicts > 0 && commitRequestIsTerminal(cr) {
				conflicts--
				first := &configv1alpha3.CommitRequest{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(cr), first); err != nil {
					return err
				}
				applyFinalizeResultToStatus(first, git.FinalizeResult{
					Outcome: git.FinalizeCommitted, Commit: "beef", Branch: "main",
				}, nil)
				if err := c.SubResource(sub).Update(ctx, first, opts...); err != nil {
					return err
				}
				return apierrors.NewConflict(commitRequestGVR().GroupResource(), cr.Name, nil)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	c := newCommitRequestClient(t, &fns, withInProgress(newCommitRequest("save")))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	serveStep(t, &commitRequestCore{reader: c, finalizer: worker, inFlight: inFlight}, controlPath(c), "save")

	worker.stop(shutdownCommit)
	inFlight.Settle(context.Background())

	assert.Zero(t, conflicts)
	assert.Equal(t, "beef", fetchCommitRequest(t, c, "save").Status.Commit, "the outcome written first stays")
	assert.Empty(t, inFlight.remembered(), "a request that holds an outcome is finished")
}

// A finished request is never written over: the write of an outcome checks for one before it applies
// its own.
func TestCommitRequestCore_AnOutcomeIsNotWrittenOverAFinishedRequest(t *testing.T) {
	c := newCommitRequestClient(t, nil, withReadyCommitted(newCommitRequest("save")))
	before := fetchCommitRequest(t, c, "save")
	core := &commitRequestCore{reader: c}

	_, written, err := core.writeOutcome(context.Background(), controlPath(c), before.DeepCopy(),
		commitRequestOutcome{result: git.FinalizeResult{Branch: "main", Err: git.ErrBranchWorkerStopped},
			err: git.ErrBranchWorkerStopped}, true)

	require.NoError(t, err)
	assert.False(t, written)
	after := fetchCommitRequest(t, c, "save")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is written")
	assert.False(t, conditionIsTrue(after.Status.Conditions, ConditionTypeStalled))
}

// A request on a source cluster is settled through the identity that serves it, after the server's
// watch has stopped with the rest of the controllers.
func TestCommitRequestsInFlight_ASourceClusterRequestIsSettledOnItsCluster(t *testing.T) {
	provider := kubeconfigProvider("edge", true)
	c := newCommitRequestClient(t, nil, mirroringTarget("edge"), provider)
	gvr := commitRequestGVR()
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "CommitRequestList"}, sourceCommitRequest("default", "save"))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	s := &CommitRequestServer{
		Client: c, Finalizer: worker, InFlight: inFlight, Log: logr.Discard(),
		Clusters: fakeClusters{dynamic: source, discovery: fakeDiscovery(nil,
			commitRequestResources("configbutler.ai/v1alpha3", "commitrequests"))},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.syncProviders(ctx)
	readReady := func() string {
		u, err := source.Resource(gvr).Namespace("default").Get(context.Background(), "save", metav1.GetOptions{})
		require.NoError(t, err)
		cr, err := commitRequestFromUnstructured(u)
		require.NoError(t, err)
		if ready := findCondition(cr.Status.Conditions, ConditionTypeReady); ready != nil {
			return ready.Reason
		}
		return ""
	}
	require.Eventually(t, func() bool { return readReady() == string(git.PhaseCollectingWindow) },
		10*time.Second, 20*time.Millisecond, "the request is attached through the provider's watch")
	cancel()
	s.stopAll()

	worker.stop(shutdownCommit)
	inFlight.Settle(context.Background())

	assert.Equal(t, crReasonCommitted, readReady(), "written on the source cluster")
}

// A nil set remembers nothing, and settles nothing.
func TestCommitRequestsInFlight_Nil(t *testing.T) {
	var inFlight *CommitRequestsInFlight
	assert.NotPanics(t, func() {
		inFlight.track(&commitRequestCore{}, commitRequestPath{}, types.NamespacedName{Name: "save"})
		inFlight.forget(commitRequestPath{}, types.NamespacedName{Name: "save"})
		inFlight.Settle(context.Background())
	})
}

// A shutdown out of time writes nothing more: what is left is attached again by the next process.
func TestCommitRequestsInFlight_OutOfTimeLeavesTheRest(t *testing.T) {
	c := newCommitRequestClient(t, nil, withInProgress(newCommitRequest("save")))
	worker := &drainingWorker{}
	inFlight := &CommitRequestsInFlight{Log: logr.Discard()}
	serveStep(t, &commitRequestCore{reader: c, finalizer: worker, inFlight: inFlight}, controlPath(c), "save")
	worker.stop(shutdownCommit)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inFlight.Settle(ctx)

	assert.Equal(t, string(git.PhaseCollectingWindow), commitRequestReady(t, c, "save").Reason)
	assert.Len(t, inFlight.remembered(), 1)
}
