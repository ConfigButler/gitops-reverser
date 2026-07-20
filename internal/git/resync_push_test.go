// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
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

	require.True(t, w.EnqueueResync(&ResyncRequest{Result: make(chan ResyncResult, 1)}),
		"a resync that fits the empty queue reports enqueued=true")

	dropped := make(chan ResyncResult, 1)
	require.False(t, w.EnqueueResync(&ResyncRequest{Result: dropped}),
		"a full queue drops the resync and reports enqueued=false")
	select {
	case res := <-dropped:
		require.ErrorIs(t, res.Err, ErrFinalizeQueueFull,
			"the dropped resync's caller is still notified via the result channel")
	default:
		t.Fatal("expected a queue-full result on the dropped resync's channel")
	}
}

// TestHandleResyncRequest_ClosedWindowIsPushedEvenWhenNoOpResync pins the
// stranded-write fix (docs/spec/commitrequest-design.md §6.4.2, §9.5): a
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
