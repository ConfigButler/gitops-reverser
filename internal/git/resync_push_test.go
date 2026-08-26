// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEnqueueResync_ReportsEnqueueOutcome pins the contract the per-target coverage watermark relies
// on (signing-snapshot-tail-replay-failure-investigation.md §7.4): EnqueueResync reports whether the
// request actually entered the FIFO. A queue-full DROP must report enqueued=false (and still notify
// the caller via the result channel) so the watch layer never marks a target reconciled-through-Hc
// for a reconcile that never queued. Before the fix EnqueueResync swallowed the drop, so a caller
// could only learn of it asynchronously on the channel — too late to gate the watermark publish.
func TestEnqueueResync_ReportsEnqueueOutcome(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}

	require.True(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "first",
		Result: make(chan ResyncResult, 1),
	}), "a resync that fits the empty queue reports enqueued=true")

	// A DIFFERENT scope, so it needs a FIFO slot of its own rather than coalescing
	// into the queued one — which is what makes this the queue-full path.
	dropped := make(chan ResyncResult, 1)
	require.False(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "second",
		Result: dropped,
	}), "a full queue drops the resync and reports enqueued=false")
	select {
	case res := <-dropped:
		require.ErrorIs(t, res.Err, ErrFinalizeQueueFull,
			"the dropped resync's caller is still notified via the result channel")
	default:
		t.Fatal("expected a queue-full result on the dropped resync's channel")
	}
}

// TestEnqueueResync_CoalescesSameScope pins the starvation fix. A resync is
// state-based — "make Git match this desired set for this scope" — so a newer
// request for the same GitTarget and scope wholly supersedes one still queued.
// Coalescing them bounds queue depth by the number of distinct scopes instead of
// by the request rate, so a storm of resyncs for ONE target can no longer fill a
// branch worker's shared queue and starve every other GitTarget on that branch.
// In the CI failure that motivated this, 595 dropped resyncs were all for a
// single GitTarget.
func TestEnqueueResync_CoalescesSameScope(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}

	superseded := make(chan ResyncResult, 1)
	require.True(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "target", Revision: "1",
		Result: superseded,
	}))

	// The queue now has no free slot, yet a second request for the SAME scope is
	// accepted: it replaces the queued one rather than being dropped.
	newest := make(chan ResyncResult, 1)
	require.True(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "target", Revision: "2",
		Result: newest,
	}), "a resync for an already-queued scope coalesces instead of being dropped")

	select {
	case res := <-superseded:
		require.ErrorIs(t, res.Err, ErrResyncSuperseded,
			"the superseded caller is told a newer resync replaced it, not that it failed")
	default:
		t.Fatal("expected the superseded resync's channel to be answered")
	}

	// Exactly one marker is queued, and taking it yields the NEWEST request.
	require.Len(t, w.eventQueue, 1, "coalescing must not consume a second FIFO slot")
	item := <-w.eventQueue
	require.NotNil(t, item.Resync)
	current := w.takePendingResync(item.Resync)
	assert.Equal(t, "2", current.Revision, "the marker runs the newest request for its scope")

	// The key is cleared, so the next resync for that scope queues a fresh marker.
	require.True(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "target", Revision: "3",
		Result: make(chan ResyncResult, 1),
	}))
	assert.Len(t, w.eventQueue, 1, "a scope taken off the queue can be queued again")
}

// TestEnqueueResync_AcceptedRequestAlwaysRuns pins the atomicity of insert-and-queue
// under a full queue. The invariant: a request told enqueued=true must either have
// the FIFO marker for its scope, or have been superseded by a later one. Anything
// else is a caller that believes its resync is queued -- and so advances the
// per-type coverage watermark -- for work that will never run, while its drain
// waits for a reply that never comes.
//
// The failure needed the entry to be visible to a concurrent enqueue before its
// marker existed, so this drives the window concurrently and repeatedly rather
// than asserting a single interleaving.
func TestEnqueueResync_AcceptedRequestAlwaysRuns(t *testing.T) {
	for range 200 {
		acceptedCount, supersededCount, markers := raceEnqueuesOnOneScope(t)
		require.Equal(t, acceptedCount, supersededCount+markers,
			"every accepted resync must own the marker or have been superseded")
	}
}

// raceEnqueuesOnOneScope drives concurrent enqueues for a single scope against a
// full queue, and reports how many were accepted, how many of those were
// superseded, and whether a marker survives for the scope.
func raceEnqueuesOnOneScope(t *testing.T) (int, int, int) {
	t.Helper()
	const enqueuers = 8

	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	// Occupy the only slot with an unrelated scope, so every request below races
	// on the same key against a full queue.
	require.True(t, w.EnqueueResync(&ResyncRequest{
		GitTargetNamespace: "ns", GitTargetName: "filler",
		Result: make(chan ResyncResult, 1),
	}))

	acceptedCount, supersededCount, markers := 0, 0, 0
	results := make([]chan ResyncResult, enqueuers)
	accepted := make([]bool, enqueuers)
	var wg sync.WaitGroup
	for i := range enqueuers {
		results[i] = make(chan ResyncResult, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted[i] = w.EnqueueResync(&ResyncRequest{
				GitTargetNamespace: "ns", GitTargetName: "contended",
				Result: results[i],
			})
		}()
	}
	wg.Wait()

	for len(w.eventQueue) > 0 {
		if item := <-w.eventQueue; item.Resync != nil && item.Resync.GitTargetName == "contended" {
			markers++
		}
	}

	for i := range enqueuers {
		if !accepted[i] {
			continue
		}
		acceptedCount++
		select {
		case res := <-results[i]:
			if errors.Is(res.Err, ErrResyncSuperseded) {
				supersededCount++
			}
		default:
		}
	}
	return acceptedCount, supersededCount, markers
}

// TestEnqueueResync_DistinctScopesDoNotCoalesce guards the other half: coalescing
// must key on the scope, so two different types of the same GitTarget stay two
// independent resyncs. Merging them would sweep one type's scope with the other
// type's desired set, which deletes managed documents.
func TestEnqueueResync_DistinctScopesDoNotCoalesce(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 4)}

	configMaps := ResyncScope{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}}
	secrets := ResyncScope{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}}
	sameTypeOtherNS := ResyncScope{
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		Namespace: "other",
	}

	for _, scope := range []ResyncScope{configMaps, secrets, sameTypeOtherNS} {
		require.True(t, w.EnqueueResync(&ResyncRequest{
			GitTargetNamespace: "ns", GitTargetName: "target",
			Scope:  &scope,
			Result: make(chan ResyncResult, 1),
		}))
	}
	assert.Len(t, w.eventQueue, 3, "each distinct scope holds its own FIFO slot")
}

// TestHandleResyncRequest_ClosedWindowIsPushedEvenWhenNoOpResync pins the
// stranded-write fix in the CommitRequest window contract: a
// resync that closes a live commit window but commits nothing of its own must
// still schedule the window's commit for push. Before the fix maybeSchedulePush
// ran only when the resync itself committed, so a window the resync closed was
// committed locally and then stranded — never reaching the remote.
func TestHandleResyncRequest_ClosedWindowIsPushedEvenWhenNoOpResync(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	// A live ConfigMap edit opens a window (commitWindow is an hour, so it stays open).
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow, "the edit must open a window")

	// A type-scoped resync for a DIFFERENT type with an empty desired set: it closes
	// the open window (resync-before-apply) but its own mark-and-sweep — scoped to a
	// type with no documents — changes nothing, so the resync itself does not commit.
	scope := ResyncScope{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}}
	resultCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Scope:              &scope,
		Desired:            nil,
		Result:             resultCh,
	})
	res := <-resultCh
	require.NoError(t, res.Err)
	require.Zero(t, res.Stats.Created+res.Stats.Updated+res.Stats.Deleted,
		"the scoped resync must itself commit nothing")

	// The window's commit must have been pushed, not stranded as a retained pending write.
	assert.Empty(t, loop.pendingWrites, "the closed window's commit must not be retained unpushed")
	afterRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialHash, afterRef.Hash(),
		"the held edit's commit must reach the remote even though the resync was a no-op")
}
