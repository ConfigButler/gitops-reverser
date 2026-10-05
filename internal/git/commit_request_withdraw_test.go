// SPDX-License-Identifier: Apache-2.0

package git

// The controller withdraws a CommitRequest before failing it closed. These pin the worker's half of
// that agreement: a request the worker has not acted on is cancelled for good, and a request it
// holds (attached, or committed) is not, so a request reported failed is never committed later.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func TestWithdraw_AWaitingRequestIsCancelledForGood(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	serviceAttach(loop, req)
	require.Equal(t, PhaseWaitingForWindow, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	loop.handleQueueItem(WorkItem{Withdraw: req})

	res, ok := outcome(t, worker)
	require.True(t, ok, "a withdrawn request resolves")
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
	assert.Empty(t, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	// A late re-send of the attach, and a window it would have attached to, change nothing.
	serviceAttach(loop, req)
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("late", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	assert.Nil(t, loop.openWindow.pendingCR, "a withdrawn request never attaches")
	assert.Empty(t, loop.pendingCRs)
	res, _ = outcome(t, worker)
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
}

// A withdraw that arrives before the worker ever saw the attach leaves a tombstone, so the attach
// that was still in flight cannot register afterwards.
func TestWithdraw_AnUnregisteredRequestCannotRegisterLater(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	loop.handleQueueItem(WorkItem{Withdraw: req})
	serviceAttach(loop, req)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
	assert.Empty(t, loop.pendingCRs)
}

// A request attached to a window is held: the withdraw is a no-op, and the request still resolves
// with its commit. This is the case the controller reads as "keep waiting".
func TestWithdraw_AnAttachedRequestIsHeldAndStillCommits(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	req := attachReq("alice", time.Hour)
	serviceAttach(loop, req)
	require.Equal(t, PhaseCollectingWindow, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))

	loop.handleQueueItem(WorkItem{Withdraw: req})

	_, resolved := outcome(t, worker)
	require.False(t, resolved, "a held request is not withdrawn")
	assert.True(t, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName).Held())

	forceDue(loop)
	loop.pushPending()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
}

// A request that already resolved keeps its outcome.
func TestWithdraw_AResolvedRequestKeepsItsOutcome(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", 0)
	serviceAttach(loop, req) // no window: resolves NoOpenWindow at once
	before, ok := outcome(t, worker)
	require.True(t, ok)

	loop.handleQueueItem(WorkItem{Withdraw: req})

	after, _ := outcome(t, worker)
	assert.Equal(t, before, after)
}

func TestEnqueueWithdraw_QueueFullIsDroppedAndNilIsANoOp(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	w.EnqueueWithdraw(nil)
	assert.Empty(t, w.eventQueue)

	w.eventQueue <- WorkItem{} // saturate
	w.EnqueueWithdraw(attachReq("alice", 0))
	assert.Zero(t, w.inflightItems.Load(), "a dropped withdraw must not leak an inflight count")
}

// The withdrawn outcome is what turns a late attach into a no-op, so it must outlive every attach
// still on the FIFO, however long that attach waits: a TTL alone only moves the problem.
func TestWithdraw_AnOutcomeIsKeptWhileAnAttachForItIsQueued(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := attachReq("alice", time.Hour)
	loop.handleQueueItem(WorkItem{Withdraw: req})
	worker.EnqueueAttach(req) // a late attach, still on the FIFO

	// The outcome runs past its TTL while the attach waits, and a GC pass runs.
	ageCommitRequestOutcome(worker, req.id(), 2*commitRequestOutcomeTTL)
	worker.recordCommitRequestOutcome(commitRequestID{Namespace: "default", Name: "other"}, FinalizeResult{})
	_, kept := outcome(t, worker)
	require.True(t, kept, "an outcome with an attach still queued is not collected")

	loop.handleQueueItem(<-worker.eventQueue)
	loop.endWake(0)
	assert.Empty(t, loop.pendingCRs, "the late attach does not register the withdrawn request")
	res, _ := outcome(t, worker)
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)

	// With nothing queued for it any more, the outcome is collected as usual.
	ageCommitRequestOutcome(worker, req.id(), 2*commitRequestOutcomeTTL)
	worker.recordCommitRequestOutcome(commitRequestID{Namespace: "default", Name: "other"}, FinalizeResult{})
	_, kept = outcome(t, worker)
	assert.False(t, kept)
}

// A worker whose loop never ran, because its GitProvider could not be read, is still listed by the
// manager, so attaches and withdraws keep arriving. It must answer them rather than queue them for
// a loop that is gone.
func TestWithdraw_AWorkerThatExitedAtStartupAnswers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	worker := NewBranchWorker(fake.NewClientBuilder().WithScheme(scheme).Build(), logr.Discard(),
		"no-such-provider", "default", "main", RepoIdentity{URL: "file:///nowhere"}, nil, BranchWorkerLimits{})
	owners := &commitRequestOwners{}
	worker.crOwners = owners

	req := attachReq("alice", time.Hour)
	worker.EnqueueAttach(req) // queued before the loop gives up
	require.NoError(t, worker.Start(context.Background()))
	worker.wg.Wait() // the loop has exited

	_, owned := owners.owner(req.id())
	assert.False(t, owned, "a request the worker never acted on is given back")
	assert.Zero(t, worker.inflightItems.Load(), "the queue it never read is drained")

	worker.EnqueueAttach(req)
	_, resolved := outcome(t, worker)
	assert.False(t, resolved, "an attach is refused, not queued")

	worker.EnqueueWithdraw(req)
	res, resolved := outcome(t, worker)
	require.True(t, resolved, "a withdraw is answered at once")
	require.ErrorIs(t, res.Err, ErrCommitRequestWithdrawn)
	require.ErrorIs(t, res.Err, ErrBranchWorkerStopped)
	assert.Zero(t, worker.inflightItems.Load())
}

// A worker that is retired keeps answering for the request it held: its shutdown settles it, and
// that outcome stays reachable through the owner table after the manager stopped listing it.
// Reading "no worker" as "nothing holds it" is what reported such a request withdrawn.
func TestWithdraw_ARetiredWorkerStillAnswersForWhatItHeld(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	owners := &commitRequestOwners{}
	worker.crOwners = owners
	require.NoError(t, worker.Start(context.Background()))

	require.True(t, worker.Enqueue(configMapTargetEvent("held", "alice", "team-a")))
	req := attachReq("alice", time.Hour)
	worker.EnqueueAttach(req)
	require.Eventually(t, func() bool {
		return worker.LookupCommitRequestPhase("default", crName, "uid-"+crName).Held()
	}, 5*time.Second, 10*time.Millisecond)

	worker.Stop()

	owner, owned := owners.owner(req.id())
	require.True(t, owned, "the retired worker still holds the request")
	assert.Same(t, worker, owner)
	res, resolved := outcome(t, worker)
	require.True(t, resolved, "its shutdown settled the request, by its last push or by failing it")
	require.NotErrorIs(t, res.Err, ErrCommitRequestWithdrawn)

	worker.EnqueueWithdraw(req)
	after, _ := outcome(t, worker)
	assert.Equal(t, res, after, "a withdraw after the fact keeps the real outcome")
}

// A request still waiting for a window when its worker stops is given back: the old loop can no
// longer commit it, so the controller may send it to the GitTarget's next worker.
func TestWithdraw_AStoppedWorkerGivesBackWhatItNeverActedOn(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	owners := &commitRequestOwners{}
	worker.crOwners = owners
	require.NoError(t, worker.Start(context.Background()))

	req := attachReq("alice", time.Hour)
	worker.EnqueueAttach(req)
	require.Eventually(t, func() bool {
		return worker.LookupCommitRequestPhase("default", crName, "uid-"+crName) == PhaseWaitingForWindow
	}, 5*time.Second, 10*time.Millisecond)

	worker.Stop()

	_, owned := owners.owner(req.id())
	assert.False(t, owned)
	assert.Empty(t, worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))
	_, resolved := outcome(t, worker)
	assert.False(t, resolved, "giving it back is not an outcome")
}

func TestCommitRequestOwners_OneOwnerAtATime(t *testing.T) {
	a, b := &BranchWorker{}, &BranchWorker{}
	id := commitRequestID{Namespace: "default", Name: crName}
	owners := &commitRequestOwners{}

	require.True(t, owners.claim(id, a))
	assert.True(t, owners.claim(id, a), "claiming again is idempotent")
	assert.False(t, owners.claim(id, b), "a second worker cannot take a held request")
	owners.release(id, b)
	got, ok := owners.owner(id)
	require.True(t, ok, "only the owner can release")
	assert.Same(t, a, got)
	owners.release(id, a)
	assert.True(t, owners.claim(id, b))

	var none *commitRequestOwners
	assert.True(t, none.claim(id, a), "a worker outside a manager owns what it is sent")
	_, ok = none.owner(id)
	assert.False(t, ok)
	none.release(id, a)
}

func TestEnqueueAttach_AnotherWorkersRequestIsRefused(t *testing.T) {
	owners := &commitRequestOwners{}
	holder := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1), crOwners: owners}
	other := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1), crOwners: owners}
	req := attachReq("alice", 0)

	holder.EnqueueAttach(req)
	other.EnqueueAttach(req)

	assert.Len(t, holder.eventQueue, 1)
	assert.Empty(t, other.eventQueue, "the request belongs to the worker that accepted it first")
	assert.Zero(t, other.inflightItems.Load())
}

// A dropped attach gives the request back unless the worker knows it otherwise.
func TestEnqueueAttach_AQueueFullDropGivesTheRequestBack(t *testing.T) {
	owners := &commitRequestOwners{}
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1), crOwners: owners}
	w.eventQueue <- WorkItem{} // saturate

	req := attachReq("alice", 0)
	w.EnqueueAttach(req)

	_, owned := owners.owner(req.id())
	assert.False(t, owned)
	assert.Empty(t, w.crQueuedAttaches)
}

func ageCommitRequestOutcome(w *BranchWorker, id commitRequestID, by time.Duration) {
	w.crOutcomesMu.Lock()
	defer w.crOutcomesMu.Unlock()
	entry := w.crOutcomes[id]
	entry.resolvedAt = entry.resolvedAt.Add(-by)
	w.crOutcomes[id] = entry
}

func TestCommitRequestPhase_Held(t *testing.T) {
	assert.True(t, PhaseCollectingWindow.Held())
	assert.True(t, PhaseWaitingForPush.Held())
	assert.False(t, PhaseWaitingForWindow.Held())
	assert.False(t, PhaseWaitingForWorker.Held())
	assert.False(t, CommitRequestPhase("").Held())
}
