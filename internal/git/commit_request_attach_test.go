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
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
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

func attachReq(author string, closeDelaySeconds int32) *AttachCommitRequest {
	return &AttachCommitRequest{
		Namespace:          "default",
		Name:               crName,
		UID:                "uid-" + crName,
		Author:             author,
		GitTargetName:      crTarget,
		GitTargetNamespace: "default",
		CloseDelaySeconds:  closeDelaySeconds,
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

// TestAttach_NoOpenWindow verifies an attach (closeDelaySeconds 0) with nothing pending
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

	// lastPushAt left zero so the finalize's push fires immediately; the request is
	// resolved on that push (§6.5).
	loop := newBranchWorkerEventLoop(worker, time.Hour)
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
	assert.Empty(t, loop.pendingWrites, "the pushed write is cleared on success")

	// The reported SHA must match local HEAD (Committed means on the remote) and
	// carry the attached message verbatim.
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
	require.NoError(t, configv1alpha2.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha2.GitProvider{
		Spec: configv1alpha2.GitProviderSpec{URL: "file:///nonexistent/gitops-reverser-repo.git"},
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

// TestAttach_NoDiffResolvesAlreadyPresentPromptly is the §8.4 pin: a finalize whose
// events re-assert already-present state produces no diff, so the request resolves
// AlreadyPresent at finalize — promptly, never blocking on a push that never comes.
func TestAttach_NoDiffResolvesAlreadyPresentPromptly(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// First, commit the ConfigMap (no CommitRequest) and push it, so it is already
	// present in Git.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("present", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()
	require.Empty(t, loop.pendingWrites)

	// Defer any further push so we can prove the resolution does NOT wait on one.
	loop.lastPushAt = time.Now()

	// A second window re-asserts the SAME object: no diff. Attach a CommitRequest and
	// finalize it (closeDelaySeconds 0).
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("present", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	serviceAttach(loop, attachReq("alice", 0))

	res, ok := outcome(t, worker)
	require.True(t, ok, "a no-diff finalize must resolve promptly, not wait on a push")
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeAlreadyPresent, res.Outcome,
		"a finalize that produces no diff resolves AlreadyPresent")
	assert.Empty(t, res.SHA, "no commit was made, so there is no SHA")
}

// pushCompetingCommit advances the remote's main from a second clone, so a worker's
// next push conflicts and rebase-replays.
func pushCompetingCommit(t *testing.T, remoteURL string) {
	t.Helper()
	dir := t.TempDir()
	repo, worktree := initLocalRepo(t, dir, remoteURL, "main")
	commitFileChange(t, worktree, dir, "competing.txt", "from another writer\n")
	require.NoError(t, repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))
}

// TestAttach_ResyncCutOffCarriesMessageAndResolvesOnPush is the §8.3 intent-
// durability pin: a resync that cuts an Attached window before its deadline still
// commits the user's message, and the request resolves Committed once that
// carrying write is pushed — with the SHA actually on the remote.
func TestAttach_ResyncCutOffCarriesMessageAndResolvesOnPush(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	defer loop.stopTimers()

	// Open alice's window with one edit.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	// Attach with a distinctive message and a long grace, so the window is Attached
	// but its finalize deadline has not fired.
	const message = "save: intent must survive a resync"
	req := attachReq("alice", 300)
	req.Message = message
	serviceAttach(loop, req)
	require.NotNil(t, loop.openWindow.pendingCR, "the window must be attached")
	_, resolved := outcome(t, worker)
	require.False(t, resolved, "the request must not resolve before its window is finalized")

	// Before the deadline, a resync for a different type cuts the window
	// (resync-before-apply). The cut commit must carry the attached message.
	scope := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	resultCh := make(chan ResyncResult, 1)
	loop.handleResyncRequest(&ResyncRequest{
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		ScopeGVR:           &scope,
		Result:             resultCh,
	})
	require.NoError(t, (<-resultCh).Err)

	res, ok := outcome(t, worker)
	require.True(t, ok, "the cut-off commit's push must resolve the request")
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	require.NotEmpty(t, res.SHA)

	// The commit on the remote carries the user's message verbatim (not the generated
	// grouped message), and its SHA equals the reported SHA.
	ref, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, ref.Hash().String(), res.SHA, "the reported SHA must be the commit on the remote")
	commit, err := serverRepo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Equal(t, message, commit.Message, "the cut-off commit must carry the user's message verbatim")
}

// TestAttach_ConflictReplayResolvesToPostReplaySHA is the §8.3 complementary pin:
// when the push hits a conflict and rebase-replays, the request resolves to the
// post-replay commit actually on the remote — never a stale pre-rebase hash.
func TestAttach_ConflictReplayResolvesToPostReplaySHA(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	// lastPushAt set so the finalize's push is deferred (a cooldown timer): the
	// window commits locally, then we move the remote, then push explicitly.
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	const message = "save: survive a conflict replay"
	req := attachReq("alice", 0)
	req.Message = message
	serviceAttach(loop, req) // finalize now; the push is deferred by the cooldown

	require.Len(t, loop.pendingWrites, 1, "the window commit is retained, awaiting push")
	_, resolved := outcome(t, worker)
	require.False(t, resolved, "the request resolves on push, not at finalize")
	localSHA := loop.pendingWrites[0].CommitSHA
	require.False(t, localSHA.IsZero())

	// A competing writer advances the remote, so the worker's push conflicts and
	// rebase-replays onto the new base, producing a fresh commit hash.
	pushCompetingCommit(t, remoteURL)

	loop.pushPending()
	require.Empty(t, loop.pendingWrites, "a successful replayed push clears the retained writes")

	res, ok := outcome(t, worker)
	require.True(t, ok)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)

	// The reported SHA is the POST-replay commit on the remote, not the stale
	// pre-replay local hash.
	ref, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, ref.Hash().String(), res.SHA, "the reported SHA must be the commit on the remote")
	assert.NotEqual(t, localSHA.String(), res.SHA, "the SHA must be refreshed to the post-replay hash")
	commit, err := serverRepo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Equal(t, message, commit.Message, "the replayed commit keeps the user's message")
}
