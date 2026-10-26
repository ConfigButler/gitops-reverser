// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configMapEventIn is configMapTargetEvent for a ConfigMap in namespace, "" for a write the window
// keys like a cluster-scoped one.
func configMapEventIn(name, username, namespace string) Event {
	event := configMapTargetEvent(name, username, crTarget)
	event.Identifier.Namespace = namespace
	event.Object.SetNamespace(namespace)
	return event
}

// scopedAttachReq is a request that saves only the writes made in namespace.
func scopedAttachReq(author, namespace string) *AttachSaveRequest {
	req := attachReq(author, 60*time.Second)
	req.SourceNamespace = namespace
	return req
}

// A request confined to a namespace attaches only to a window whose writes are all in it. The
// window's author and GitTarget still decide first: a window of someone else's is foreign, not wider.
func TestMatchesWindow_SourceNamespaceConfinesTheSave(t *testing.T) {
	window := func(events ...Event) *openWindow {
		w := newOpenWindow(events[0], &contentWriter{})
		for _, e := range events {
			w.add(e)
		}
		return w
	}
	shop := configMapEventIn("a", "alice", "shop")
	billing := configMapEventIn("b", "alice", "billing")
	cluster := configMapEventIn("c", "alice", "")
	request := func(namespace string) *pendingSaveRequest {
		return &pendingSaveRequest{
			author: "alice", gitTargetName: crTarget, gitTargetNamespace: "default", sourceNamespace: namespace,
		}
	}

	assert.True(t, request("shop").matchesWindow(window(shop)), "every write is in its namespace")
	assert.False(t, request("shop").matchesWindow(window(billing)), "every write is elsewhere")
	assert.False(t, request("shop").matchesWindow(window(shop, billing)), "one write is elsewhere")
	assert.False(t, request("shop").matchesWindow(window(cluster)), "a cluster-scoped write is in no namespace")
	assert.True(t, request("").matchesWindow(window(shop, billing, cluster)), "no confinement")

	wider := request("shop")
	wider.noteUnmatchedWindow(window(shop, billing))
	assert.Equal(t, FinalizeWindowOutsideNamespace, wider.expiryOutcome())

	bob := configMapEventIn("d", "bob", "shop")
	foreign := request("shop")
	foreign.noteUnmatchedWindow(window(bob))
	assert.Equal(t, FinalizeWindowMismatch, foreign.expiryOutcome(), "bob's window is foreign")
}

// Once a confined request attached, a write by the same author in another namespace does not join
// its window: the window closes with the request's message and only its namespace's write, and the
// other write opens a window of its own.
func TestAttach_AConfinedSaveKeepsOtherNamespacesOutOfItsCommit(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, crTarget, crTarget)

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	req := scopedAttachReq("alice", "shop")
	req.Message = "save: the shop"
	serviceAttach(loop, req)

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{configMapEventIn("cart", "alice", "shop")}}})
	loop.endWake(0)
	require.NotNil(t, loop.openWindow)
	require.NotNil(t, loop.openWindow.pendingCR, "the request attached to the shop window")

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{configMapEventIn("invoice", "alice", "billing")},
	}})
	loop.endWake(0)

	res, ok := outcome(t, worker)
	require.True(t, ok, "the billing write closed the shop window")
	require.NoError(t, res.Err)
	require.Equal(t, FinalizeCommitted, res.Outcome)
	require.NotNil(t, loop.openWindow, "the billing write opened a window of its own")
	assert.Nil(t, loop.openWindow.pendingCR, "and the request is not in it")
	assert.Equal(t, "billing", loop.openWindow.namespace)

	repo, err := gogit.PlainOpen(worker.repoPath())
	require.NoError(t, err)
	commit, err := repo.CommitObject(plumbing.NewHash(res.Commit))
	require.NoError(t, err)
	assert.Equal(t, "save: the shop", commit.Message)
	stats, err := commit.Stats()
	require.NoError(t, err)
	changed := make([]string, 0, len(stats))
	for _, stat := range stats {
		changed = append(changed, stat.Name)
	}
	assert.Contains(t, changed, crTarget+"/shop/configmaps/cart.yaml")
	assert.NotContains(
		t,
		changed,
		crTarget+"/billing/configmaps/invoice.yaml",
		"billing's write is not the shop's save",
	)
}

// A confined request leaves its author's window alone when that window already holds writes from
// elsewhere, and says so when its wait runs out.
func TestAttach_AConfinedSaveLeavesAWiderWindowAlone(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, crTarget, crTarget)

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{
		configMapEventIn("cart", "alice", "shop"),
		configMapEventIn("invoice", "alice", "billing"),
	}}})
	require.NotNil(t, loop.openWindow)

	serviceAttach(loop, scopedAttachReq("alice", "shop"))
	require.Nil(t, loop.openWindow.pendingCR, "a window holding billing's write is not the shop's to save")

	forceDue(loop)
	loop.endWake(0)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeWindowOutsideNamespace, res.Outcome)
	require.NotNil(t, loop.openWindow, "the window is left open")
	assert.Len(t, loop.openWindow.orderedEvents(), 2)
}
