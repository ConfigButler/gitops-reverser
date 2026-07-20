// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestHandleResyncRequest_HealDefersWhileWindowOpenThenApplies pins Rec 1 (the 8f2ad84 fix): a HEAL
// resync that arrives while a commit window is open must be DEFERRED — it must not force-finalize
// (steal) the window — and must apply once the window finalizes on its own (a silence timeout, a
// CommitRequest). The held edit's commit therefore lands first, then the heal's, preserving arrival
// order without the steal.
func TestHandleResyncRequest_HealDefersWhileWindowOpenThenApplies(t *testing.T) {
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

	// A HEAL resync for the same GitTarget arrives while the window is open.
	scope := ResyncScope{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}}
	healCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Scope:              &scope,
		Heal:               true,
		Desired:            nil,
		Result:             healCh,
	})

	// It must be parked, not applied: the window is still open and the heal has not replied.
	require.NotNil(t, loop.openWindow, "a heal must never force-finalize the open window")
	require.Len(t, loop.deferredHeals, 1, "the heal is parked until the window is idle")
	select {
	case <-healCh:
		t.Fatal("a deferred heal must not reply before it applies")
	default:
	}
	afterDeferRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.Equal(t, initialHash, afterDeferRef.Hash(), "nothing must commit while the heal is deferred")

	// The window finalizes on its own (silence boundary) and the loop drains the parked heal.
	loop.finalizeOpenWindow()
	loop.applyDeferredHeals()

	require.Empty(t, loop.deferredHeals, "the heal applies once the window is idle")
	res := <-healCh
	require.NoError(t, res.Err, "the deferred heal applies cleanly once it gets its turn")
}

// TestHandleResyncRequest_AtomicDrainsDeferredHealFirst proves the atomic-ordering fix: when an
// atomic request finalizes an open window (an idle boundary), a heal parked behind that window —
// which arrived BEFORE the atomic — is drained at that boundary rather than being overtaken by the
// atomic write.
func TestHandleResyncRequest_AtomicDrainsDeferredHealFirst(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now() // avoid an immediate push; we only assert in-memory ordering
	defer loop.stopTimers()

	// A live edit opens a window, then a heal (for a different type) parks behind it.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	scope := ResyncScope{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}}
	healCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName: "team-a", GitTargetNamespace: "default",
		Scope: &scope, Heal: true, Result: healCh,
	})
	require.Len(t, loop.deferredHeals, 1, "the heal parks behind the open window")

	// An atomic request arrives: it finalizes the window, and the parked heal must drain at that
	// boundary (it arrived first) — not be left behind the atomic.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("atomic", "reconciler", "team-a")},
		CommitMode: CommitModeAtomic,
	}})

	require.Empty(t, loop.deferredHeals, "the atomic finalize drained the parked heal at its idle boundary")
	select {
	case res := <-healCh:
		require.NoError(t, res.Err, "the drained heal applied")
	default:
		t.Fatal("the heal must have applied when the atomic finalized the window")
	}
}

// TestHandleResyncRequest_HealDoesNotStealSiblingCommitRequestWindow is the shared-worker case of
// Rec 1: one BranchWorker serves N GitTargets and the commit window is a worker singleton, so a
// force-finalizing heal could finalize a DIFFERENT GitTarget's held window. A heal scoped to one
// GitTarget must leave another GitTarget's open CommitRequest window — and its pending request —
// untouched.
func TestHandleResyncRequest_HealDoesNotStealSiblingCommitRequestWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, crTarget, crTarget)

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// GitTarget crTarget opens a window holding alice's CommitRequest (a 60s grace keeps it open).
	serviceAttach(loop, attachReq("alice", 60))
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", crTarget)},
		CommitMode: CommitModePerEvent,
	}})
	loop.serviceCommitRequests()
	require.NotNil(t, loop.openWindow, "the edit must open a window")
	require.NotNil(t, loop.openWindow.pendingCR, "the window must carry the attached CommitRequest")

	// A heal scoped to a DIFFERENT GitTarget arrives on the shared worker.
	otherScope := ResyncScope{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}}
	healCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName:      "team-other",
		GitTargetNamespace: "default",
		Scope:              &otherScope,
		Heal:               true,
		Result:             healCh,
	})

	// The sibling's window and its CommitRequest must be untouched, and the heal merely parked.
	require.Len(t, loop.deferredHeals, 1, "the heal is parked, not applied")
	require.NotNil(t, loop.openWindow, "another GitTarget's window must stay open")
	require.NotNil(t, loop.openWindow.pendingCR, "the sibling's CommitRequest must stay attached")
	_, resolved := outcome(t, worker)
	assert.False(t, resolved, "a heal for one GitTarget must not resolve another GitTarget's CommitRequest")
	select {
	case <-healCh:
		t.Fatal("a deferred heal must not reply while a sibling window is open")
	default:
	}
}
