// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// simulatedWorker answers like a branch worker that registers a request registerAfter after its first
// attach and pushes its commit resolveAfter after that attach, reporting each change as the worker's
// loop does.
type simulatedWorker struct {
	registerAfter, resolveAfter time.Duration
	changes                     chan git.CommitRequestChange

	mu       sync.Mutex
	started  bool
	phase    git.CommitRequestPhase
	resolved bool
	pushedAt time.Time
}

func (w *simulatedWorker) ServiceCommitRequest(
	_ context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		w.started = true
		change := git.CommitRequestChange{Namespace: attach.Namespace, Name: attach.Name, UID: attach.UID}
		time.AfterFunc(w.registerAfter, func() {
			w.mu.Lock()
			w.phase = git.PhaseWaitingForWindow
			w.mu.Unlock()
			w.changes <- change
		})
		time.AfterFunc(w.resolveAfter, func() {
			w.mu.Lock()
			w.resolved, w.pushedAt = true, time.Now()
			w.mu.Unlock()
			w.changes <- change
		})
	}
	if w.resolved {
		return git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"}, true, nil
	}
	return git.FinalizeResult{Phase: w.phase}, false, nil
}

func (w *simulatedWorker) WithdrawCommitRequest(
	ctx context.Context, attach git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	return w.ServiceCommitRequest(ctx, attach)
}

func (w *simulatedWorker) LookupCommitRequestApply(
	context.Context, git.AttachCommitRequest,
) (git.SaveApply, bool, error) {
	return git.SaveApply{}, false, nil
}

// saveLatency is what a client waiting on a request sees: create to WaitingForWindow, and push to
// Ready.
type saveLatency struct{ registered, ready time.Duration }

// measureSaveLatency serves one request through a source cluster's watch, the production loop of
// CommitRequestServer.consider, with or without the worker's changes waking it.
func measureSaveLatency(t *testing.T, woken bool) saveLatency {
	t.Helper()
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	c := newCommitRequestClient(t, nil, cr, mirroringTarget("edge"), kubeconfigProvider("edge", true))
	worker := &simulatedWorker{
		registerAfter: 50 * time.Millisecond, resolveAfter: 3 * time.Second,
		changes: make(chan git.CommitRequestChange, 4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &CommitRequestServer{Client: c, Finalizer: worker}
	if woken {
		server.Wakes = NewCommitRequestWakes(worker.changes)
		go func() { _ = server.Wakes.Start(ctx) }()
	} else {
		// Nothing listens to the worker's changes: the steps poll.
		go func() {
			for range worker.changes {
				continue
			}
		}()
	}
	watcher := &commitRequestWatcher{
		provider: types.NamespacedName{Namespace: "default", Name: "edge"}, path: watcherPath(c, "edge", false),
	}
	u := &unstructured.Unstructured{}
	u.SetNamespace("default")
	u.SetName("save")
	u.SetUID(cr.UID)

	created := time.Now()
	server.consider(ctx, watcher, u)

	var latency saveLatency
	require.Eventually(t, func() bool {
		var got configv1alpha3.CommitRequest
		if c.Get(ctx, client.ObjectKeyFromObject(cr), &got) != nil {
			return false
		}
		reconciling := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReconciling)
		if latency.registered == 0 && reconciling != nil && reconciling.Reason == string(git.PhaseWaitingForWindow) {
			latency.registered = time.Since(created)
		}
		ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			return false
		}
		worker.mu.Lock()
		latency.ready = time.Since(worker.pushedAt)
		worker.mu.Unlock()
		return true
	}, 10*time.Second, 5*time.Millisecond)
	return latency
}

// TestCommitRequestWakes_AWorkersChangeTakesTheNextStepAtOnce pins gitops-api ask #40: a request's
// progress follows its worker's changes instead of a fixed poll. The worker here registers the request
// 50ms after its attach and pushes 3s after it; with the changes waking the steps, a client waiting
// for WaitingForWindow and then for Ready sees each within a few milliseconds, not the poll.
func TestCommitRequestWakes_AWorkersChangeTakesTheNextStepAtOnce(t *testing.T) {
	polled := measureSaveLatency(t, false)
	woken := measureSaveLatency(t, true)
	t.Logf("create -> WaitingForWindow: polled %s, woken %s", polled.registered, woken.registered)
	t.Logf("push -> Ready: polled %s, woken %s", polled.ready, woken.ready)

	assert.GreaterOrEqual(t, polled.registered, commitRequestPollInterval, "polled, the registration waits a poll")
	assert.Less(t, woken.registered, 250*time.Millisecond, "woken, it is reported at once")
	assert.Less(t, woken.ready, 250*time.Millisecond, "woken, the push is reported at once")
}

// TestCommitRequestWakes_AChangeMidStepIsKeptForTheNextWait pins that a change reported while the
// request's goroutine is between waits is not lost: its next wait ends at once.
func TestCommitRequestWakes_AChangeMidStepIsKeptForTheNextWait(t *testing.T) {
	changes := make(chan git.CommitRequestChange, 1)
	wakes := NewCommitRequestWakes(changes)
	woken, stop := wakes.watch("uid-save")
	defer stop()

	wakes.wake(git.CommitRequestChange{Namespace: "default", Name: "save", UID: "uid-save"})
	select {
	case <-woken:
	default:
		t.Fatal("the change was lost")
	}
	select {
	case ev := <-wakes.Events():
		assert.Equal(t, "save", ev.Object.GetName(), "the control-cluster request is reconciled too")
	default:
		t.Fatal("no reconcile was queued")
	}

	stop()
	wakes.wake(git.CommitRequestChange{Namespace: "default", Name: "save", UID: "uid-save"})
	select {
	case <-woken:
		t.Fatal("a request no goroutine serves any more is not woken")
	default:
	}
}

// TestCommitRequestWakes_NilWakesPoll pins that without wakes every step polls as it always did.
func TestCommitRequestWakes_NilWakesPoll(t *testing.T) {
	var wakes *CommitRequestWakes
	assert.Nil(t, wakes.Events())
	assert.Zero(t, wakes.fallback())
	woken, stop := wakes.watch("uid")
	stop()
	assert.Nil(t, woken)
}
