// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestHandleResyncRequest_AHealNeverClosesTheWindow pins Rec 1 (the 8f2ad84 fix) under F3's rule that
// no snapshot is parked: a HEAL resync that arrives while a commit window is open must not
// force-finalize (steal) the window. It is discarded, and a fresh snapshot of its GitTarget is asked
// for once the window is idle (docs/design/applier-echoes.md, F3).
func TestHandleResyncRequest_AHealNeverClosesTheWindow(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	var asked []itypes.ResourceReference
	worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	// A live ConfigMap edit opens a window (commitWindow is an hour, so it stays open).
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow, "the edit must open a window")

	scope := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	healCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName: "team-a", GitTargetNamespace: "default", Scope: &scope, Heal: true,
		Result: healCh,
	})
	loop.endWake(0)

	require.NotNil(t, loop.openWindow, "a heal must never force-finalize the open window")
	res := <-healCh
	require.ErrorIs(t, res.Err, ErrResyncDiscarded, "the heal is discarded, not parked")
	assert.Empty(t, asked, "and asked for again only once the window is idle")
	afterRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.Equal(t, initialRef.Hash(), afterRef.Hash(), "nothing committed")

	loop.finalizeOpenWindow()
	loop.endWake(0)
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference("team-a", "default")}, asked)
}

// TestHandleResyncRequest_AHealDoesNotStealSiblingCommitRequestWindow is the shared-worker case of
// Rec 1: one BranchWorker serves N GitTargets and the commit window is a worker singleton, so a
// force-finalizing heal could finalize a DIFFERENT GitTarget's held window. A heal scoped to one
// GitTarget must leave another GitTarget's open CommitRequest window, and its pending request,
// untouched.
func TestHandleResyncRequest_AHealDoesNotStealSiblingCommitRequestWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, crTarget, crTarget)

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, attachReq("alice", 60*time.Second))
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", crTarget)},
		CommitMode: CommitModePerEvent,
	}})
	loop.endWake(0)
	require.NotNil(t, loop.openWindow, "the edit must open a window")
	require.NotNil(t, loop.openWindow.pendingCR, "the window must carry the attached CommitRequest")

	otherScope := ResyncScopeFor(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "")
	healCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName: "team-other", GitTargetNamespace: "default", Scope: &otherScope, Heal: true,
		Result: healCh,
	})
	loop.endWake(0)

	require.NotNil(t, loop.openWindow, "another GitTarget's window must stay open")
	require.NotNil(t, loop.openWindow.pendingCR, "the sibling's CommitRequest must stay attached")
	_, resolved := outcome(t, worker)
	assert.False(t, resolved, "a heal for one GitTarget must not resolve another GitTarget's CommitRequest")
	assert.ErrorIs(t, (<-healCh).Err, ErrResyncDiscarded)
}
