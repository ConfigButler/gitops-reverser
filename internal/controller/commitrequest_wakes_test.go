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

// simulatedWorker answers like a branch worker, and moves only when the test says so: report changes
// its state and reports the change, as the worker's loop does.
type simulatedWorker struct {
	change  git.CommitRequestChange
	changes chan git.CommitRequestChange

	mu       sync.Mutex
	steps    int
	phase    git.CommitRequestPhase
	resolved bool
}

func (w *simulatedWorker) ServiceCommitRequest(
	context.Context, git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.steps++
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

func (w *simulatedWorker) LookupCommitRequestOutcome(
	context.Context, git.AttachCommitRequest,
) (git.FinalizeResult, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.resolved {
		return git.FinalizeResult{Outcome: git.FinalizeCommitted, Commit: "c0ffee", Branch: "main"}, true, nil
	}
	return git.FinalizeResult{}, false, nil
}

func (w *simulatedWorker) LookupCommitRequestApply(
	context.Context, git.AttachCommitRequest,
) (git.SaveApply, bool, error) {
	return git.SaveApply{}, false, nil
}

// report applies a change to the worker's state and reports it. The channel is buffered: on the
// polled path nothing reads it.
func (w *simulatedWorker) report(change func(*simulatedWorker)) {
	w.mu.Lock()
	change(w)
	w.mu.Unlock()
	w.changes <- w.change
}

func (w *simulatedWorker) stepsTaken() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.steps
}

// steppedTimers stands in for time.After between a request's steps: none fires until the test fires
// them, so a step taken without fire was taken by a wake.
type steppedTimers struct {
	mu      sync.Mutex
	pending []chan time.Time
}

func (s *steppedTimers) after(time.Duration) <-chan time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan time.Time, 1)
	s.pending = append(s.pending, ch)
	return ch
}

func (s *steppedTimers) armed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// fire runs out every timer armed so far.
func (s *steppedTimers) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.pending {
		ch <- time.Now()
	}
	s.pending = nil
}

// servedSave is one request served through a source cluster's watch, the production loop of
// CommitRequestServer.consider, on stepped timers, with or without the worker's changes waking it.
type servedSave struct {
	t      *testing.T
	c      client.Client
	cr     *configv1alpha3.CommitRequest
	worker *simulatedWorker
	timers *steppedTimers
}

func serveSave(t *testing.T, woken bool) *servedSave {
	t.Helper()
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	c := newCommitRequestClient(t, nil, cr, mirroringTarget("edge"), kubeconfigProvider("edge", true))
	worker := &simulatedWorker{
		change:  git.CommitRequestChange{Namespace: "default", Name: "save", UID: string(cr.UID)},
		changes: make(chan git.CommitRequestChange, 4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	timers := &steppedTimers{}
	server := &CommitRequestServer{Client: c, Finalizer: worker, after: timers.after}
	if woken {
		server.Wakes = NewCommitRequestWakes(worker.changes)
		go func() { _ = server.Wakes.Start(ctx) }()
	}
	watcher := &commitRequestWatcher{
		provider: types.NamespacedName{Namespace: "default", Name: "edge"}, path: watcherPath(c, "edge", false),
	}
	u := &unstructured.Unstructured{}
	u.SetNamespace("default")
	u.SetName("save")
	u.SetUID(cr.UID)
	server.consider(ctx, watcher, u)

	s := &servedSave{t: t, c: c, cr: cr, worker: worker, timers: timers}
	require.Eventually(t, func() bool { return worker.stepsTaken() > 0 && timers.armed() }, 10*time.Second,
		time.Millisecond, "the first step attaches the request and waits")
	return s
}

func (s *servedSave) conditions() []metav1.Condition {
	var got configv1alpha3.CommitRequest
	if s.c.Get(context.Background(), client.ObjectKeyFromObject(s.cr), &got) != nil {
		return nil
	}
	return got.Status.Conditions
}

func (s *servedSave) registered() bool {
	reconciling := apimeta.FindStatusCondition(s.conditions(), ConditionTypeReconciling)
	return reconciling != nil && reconciling.Reason == string(git.PhaseWaitingForWindow)
}

func (s *servedSave) ready() bool {
	return apimeta.IsStatusConditionTrue(s.conditions(), ConditionTypeReady)
}

func registerRequest(w *simulatedWorker) { w.phase = git.PhaseWaitingForWindow }
func pushRequest(w *simulatedWorker)     { w.resolved = true }

// TestCommitRequestWakes_AWorkersChangeTakesTheNextStepAtOnce pins gitops-api ask #40: a request's
// progress follows its worker's changes instead of a fixed poll. No timer between steps fires unless
// the test fires it, so a step taken without one was taken by the worker's change.
func TestCommitRequestWakes_AWorkersChangeTakesTheNextStepAtOnce(t *testing.T) {
	t.Run("woken, each change takes the next step", func(t *testing.T) {
		s := serveSave(t, true)
		s.worker.report(registerRequest)
		require.Eventually(t, s.registered, 10*time.Second, time.Millisecond,
			"the registration is reported with no poll fired")
		s.worker.report(pushRequest)
		require.Eventually(t, s.ready, 10*time.Second, time.Millisecond,
			"the push is reported with no poll fired")
	})

	t.Run("polled, each change waits for the poll", func(t *testing.T) {
		s := serveSave(t, false)
		s.worker.report(registerRequest)
		assert.Never(t, s.registered, 200*time.Millisecond, 5*time.Millisecond,
			"nothing takes the step before the poll")
		s.timers.fire()
		require.Eventually(t, s.registered, 10*time.Second, time.Millisecond, "the poll takes it")

		require.Eventually(t, s.timers.armed, 10*time.Second, time.Millisecond)
		s.worker.report(pushRequest)
		assert.Never(t, s.ready, 200*time.Millisecond, 5*time.Millisecond,
			"nothing takes the step before the poll")
		s.timers.fire()
		require.Eventually(t, s.ready, 10*time.Second, time.Millisecond, "the poll takes it")
	})
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

// TestCommitRequestWakes_AnotherWatcherLeavesTheOwnersWake pins that two KubeProviders watching
// the same source cluster each keep their own wake of a request. The one that does not own it looks
// at it after every write the owner makes, and leaves at once; neither its watch nor its leaving may
// take the owner's wake away, or the owner falls back to the slow poll.
func TestCommitRequestWakes_AnotherWatcherLeavesTheOwnersWake(t *testing.T) {
	wakes := NewCommitRequestWakes(make(chan git.CommitRequestChange))
	owner, stopOwner := wakes.watch("uid-save")
	defer stopOwner()
	change := git.CommitRequestChange{Namespace: "default", Name: "save", UID: "uid-save"}

	for range 2 {
		other, stopOther := wakes.watch("uid-save")
		wakes.wake(change)
		select {
		case <-other:
		default:
			t.Fatal("the other watcher was not woken")
		}
		stopOther()
		stopOther()
		select {
		case <-owner:
		default:
			t.Fatal("the owner was not woken while another watcher looked at its request")
		}
	}

	wakes.wake(change)
	select {
	case <-owner:
	default:
		t.Fatal("the owner was not woken after the other watcher left")
	}
	stopOwner()
	wakes.mu.Lock()
	assert.Empty(t, wakes.served, "a request nobody serves leaves nothing behind")
	wakes.mu.Unlock()
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
