/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import (
	"context"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// crName is the single CommitRequest name these tests register; each test uses one.
// crTarget is the GitTarget the worker serves (see createPlainGitTarget).
const (
	crName   = "save"
	crTarget = "team-a"
)

// serviceAttach drives one attach + servicing pass on the loop, the way run()
// does after dequeuing an attach work item.
func serviceAttach(loop *branchWorkerEventLoop, req *AttachCommitRequest) {
	loop.handleAttachCommitRequest(req)
	loop.serviceCommitRequests()
}

func attachReq(author string, delaySeconds int32) *AttachCommitRequest {
	return &AttachCommitRequest{
		Namespace:          "default",
		Name:               crName,
		UID:                "uid-" + crName,
		Author:             author,
		GitTargetName:      crTarget,
		GitTargetNamespace: "default",
		DelaySeconds:       delaySeconds,
	}
}

// forceDue backdates the registered request's finalize deadline so the next
// serviceCommitRequests treats its grace as elapsed — deterministic without sleeping.
func forceDue(loop *branchWorkerEventLoop) {
	id := commitRequestID{Namespace: "default", Name: crName, UID: "uid-" + crName}
	loop.pendingCRs[id].finalizeAt = time.Now().Add(-time.Millisecond)
}

func outcome(t *testing.T, w *BranchWorker) (FinalizeResult, bool) {
	t.Helper()
	return w.LookupCommitRequestOutcome("default", crName, "uid-"+crName)
}

// TestEnqueueAttach_Success verifies an attach lands on the worker's event queue.
func TestEnqueueAttach_Success(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}

	req := attachReq("alice", 0)
	w.EnqueueAttach(req)

	item := <-w.eventQueue
	require.NotNil(t, item.Attach)
	assert.Same(t, req, item.Attach)
	assert.Nil(t, item.Request)
}

// TestEnqueueAttach_QueueFullIsDropped verifies a saturated queue drops the attach
// (the controller re-sends on its next poll) rather than blocking.
func TestEnqueueAttach_QueueFullIsDropped(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	w.eventQueue <- WorkItem{} // saturate

	w.EnqueueAttach(attachReq("alice", 0))
	assert.Zero(t, w.inflightItems.Load(), "a dropped attach must not leak an inflight count")
}

// TestEnqueueAttach_Nil is a no-op and must not panic.
func TestEnqueueAttach_Nil(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), eventQueue: make(chan WorkItem, 1)}
	w.EnqueueAttach(nil)
	assert.Empty(t, w.eventQueue)
}

// TestAttach_NoOpenWindow verifies an attach (delaySeconds 0) with nothing pending
// resolves NoOpenWindow — the author pressed save with no edits, not an error.
func TestAttach_NoOpenWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, attachReq("alice", 0))

	res, ok := outcome(t, worker)
	require.True(t, ok, "a 0-grace attach with no window must resolve immediately")
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
	assert.Equal(t, "main", res.Branch)
}

// TestAttach_CommitsOpenWindow verifies a 0-grace attach onto an already-open
// same-author window finalizes it with the request's message and reports the SHA
// (UC1, the "Save" button).
func TestAttach_CommitsOpenWindow(t *testing.T) {
	worker, _, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{
			configMapTargetEvent("explicit-a", "alice", "team-a"),
			configMapTargetEvent("explicit-b", "alice", "team-a"),
		},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow, "events should accumulate in an open window")

	const message = "save: increase checkout API memory"
	req := attachReq("alice", 0)
	req.Message = message
	serviceAttach(loop, req)

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	require.NotEmpty(t, res.SHA)
	assert.Nil(t, loop.openWindow, "the open window must be finalized")
	require.Len(t, loop.pendingWrites, 1)

	// The reported SHA must match local HEAD and carry the attached message verbatim.
	repo, err := gogit.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, ref.Hash().String(), res.SHA)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Equal(t, message, commit.Message, "the attached message must be used verbatim")
	assert.Equal(t, "alice", commit.Author.Name)
}

// TestAttach_EmptyMessageUsesGeneratedMessage verifies an attach with no message
// falls back to the generated grouped-commit message.
func TestAttach_EmptyMessageUsesGeneratedMessage(t *testing.T) {
	worker, _, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("solo", "bob", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	serviceAttach(loop, attachReq("bob", 0)) // no Message

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)

	repo, err := gogit.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.NotEmpty(t, commit.Message)
	assert.Equal(t, "bob", commit.Author.Name)
}

// TestAttach_CollectGraceJoinsLaterWindow pins UC2: an attach that arrives before
// the work (no window yet) parks for the grace, attaches when the same-author
// window opens, and finalizes the collected window at the deadline.
func TestAttach_CollectGraceJoinsLaterWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	// Attach first, with a non-zero grace, before any window exists.
	req := attachReq("alice", 60)
	req.Message = "bundle save"
	serviceAttach(loop, req)
	_, resolved := outcome(t, worker)
	require.False(t, resolved, "the request must park, not resolve, while no window exists")

	// The work arrives during the grace and opens a window; it must attach.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("late", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	loop.serviceCommitRequests()
	require.NotNil(t, loop.openWindow)
	require.NotNil(t, loop.openWindow.pendingCR, "the opened window must carry the attached request")
	assert.Equal(t, "bundle save", loop.openWindow.pendingMessage)

	// Grace elapses → the collected window is finalized as one commit.
	forceDue(loop)
	loop.serviceCommitRequests()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Nil(t, loop.openWindow)
}

// TestAttach_ForeignWindowIsNotStolen verifies an attach for a different author
// parks (never finalizes another author's window) and resolves NoOpenWindow once
// its grace elapses, leaving the foreign window open.
func TestAttach_ForeignWindowIsNotStolen(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("cm", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	serviceAttach(loop, attachReq("bob", 60)) // bob, not alice
	require.Nil(t, loop.openWindow.pendingCR, "bob's attach must not claim alice's window")

	forceDue(loop)
	loop.serviceCommitRequests()

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome, "another author's save must not finalize alice's window")
	require.NotNil(t, loop.openWindow, "alice's window must be left open")
	assert.Equal(t, "alice", loop.openWindow.Author)
}

// TestAttach_IdempotentReSendKeepsFirstDeadline verifies a re-sent attach (same
// identity) does not reset the finalize deadline or duplicate the registration.
func TestAttach_IdempotentReSendKeepsFirstDeadline(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	serviceAttach(loop, attachReq("alice", 60))
	id := commitRequestID{Namespace: "default", Name: "save", UID: "uid-save"}
	require.Contains(t, loop.pendingCRs, id)
	firstDeadline := loop.pendingCRs[id].finalizeAt

	serviceAttach(loop, attachReq("alice", 300)) // larger grace, re-send
	require.Len(t, loop.pendingCRs, 1, "a re-send must not duplicate the registration")
	assert.Equal(t, firstDeadline, loop.pendingCRs[id].finalizeAt, "the first deadline must be kept")
}

// TestAttach_FinalizeFailureResolvesFailed verifies that when the attached
// window's commit fails (unreachable remote) the request resolves with an error.
func TestAttach_FinalizeFailureResolvesFailed(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{URL: "file:///nonexistent/gitops-reverser-repo.git"},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil, 0)
	worker.ctx = ctx
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("cm", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	serviceAttach(loop, attachReq("alice", 0))

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.Error(t, res.Err, "an unreachable remote must resolve the request with an error")
	assert.Nil(t, loop.openWindow, "a failed finalize still drops the broken window")
}

// TestFinalizeOpenWindow_ReturnsCommittedFlag verifies the boolean contract of
// finalizeOpenWindow: false when there is nothing to finalize, true otherwise.
func TestFinalizeOpenWindow_ReturnsCommittedFlag(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	assert.False(t, loop.finalizeOpenWindow(), "no open window → false")

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("cm", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	assert.True(t, loop.finalizeOpenWindow(), "open window finalized → true")
	assert.Nil(t, loop.openWindow)
}
