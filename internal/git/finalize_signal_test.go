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

// TestEnqueueFinalize_Success verifies a finalize signal lands on the worker's
// event queue as a Finalize WorkItem.
func TestEnqueueFinalize_Success(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}

	signal := &FinalizeSignal{Result: make(chan FinalizeResult, 1)}
	w.EnqueueFinalize(signal)

	item := <-w.eventQueue
	require.NotNil(t, item.Finalize)
	assert.Same(t, signal, item.Finalize)
	assert.Nil(t, item.Request)
}

// TestEnqueueFinalize_QueueFull verifies a saturated queue rejects the signal
// and the caller is notified immediately rather than blocking.
func TestEnqueueFinalize_QueueFull(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), Branch: "main", eventQueue: make(chan WorkItem, 1)}
	w.eventQueue <- WorkItem{} // saturate

	resultCh := make(chan FinalizeResult, 1)
	w.EnqueueFinalize(&FinalizeSignal{Result: resultCh})

	select {
	case res := <-resultCh:
		require.ErrorIs(t, res.Err, ErrFinalizeQueueFull)
		assert.Equal(t, "main", res.Branch)
	case <-time.After(time.Second):
		t.Fatal("expected a queue-full result, got none")
	}
}

// TestEnqueueFinalize_NilSignal is a no-op and must not panic.
func TestEnqueueFinalize_NilSignal(t *testing.T) {
	w := &BranchWorker{Log: logr.Discard(), eventQueue: make(chan WorkItem, 1)}
	w.EnqueueFinalize(nil)
	assert.Empty(t, w.eventQueue)
}

// TestFinalizeSignalReply_NilResultChannel must not panic.
func TestFinalizeSignalReply_NilResultChannel(_ *testing.T) {
	(&FinalizeSignal{}).reply(FinalizeResult{})
	(*FinalizeSignal)(nil).reply(FinalizeResult{})
}

// TestHandleFinalizeSignal_NoOpenWindow verifies that finalizing with nothing
// pending yields the terminal NoOpenWindow outcome — not an error.
func TestHandleFinalizeSignal_NoOpenWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)

	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{Result: resultCh})

	res := <-resultCh
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome)
	assert.Equal(t, "main", res.Branch)
	assert.Empty(t, res.SHA)
}

// TestHandleFinalizeSignal_CommitsOpenWindow verifies that an open window is
// finalized into a commit, the literal commit message is used, and the
// resulting SHA is reported back.
func TestHandleFinalizeSignal_CommitsOpenWindow(t *testing.T) {
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
	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{
		CommitMessage:      message,
		Author:             "alice",
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Result:             resultCh,
	})

	res := <-resultCh
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Equal(t, "main", res.Branch)
	require.NotEmpty(t, res.SHA)
	assert.Nil(t, loop.openWindow, "open window must be finalized")
	require.Len(t, loop.pendingWrites, 1)

	// The reported SHA must match the local HEAD and carry the literal message.
	repo, err := gogit.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, ref.Hash().String(), res.SHA)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Equal(t, message, commit.Message, "commit request message must be used verbatim")
	assert.Equal(t, "alice", commit.Author.Name, "commit author is the editing user")
}

// TestHandleFinalizeSignal_EmptyMessageUsesGeneratedMessage verifies that an
// empty spec.message falls back to the generated grouped-commit message.
func TestHandleFinalizeSignal_EmptyMessageUsesGeneratedMessage(t *testing.T) {
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

	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{
		Author:             "bob",
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Result:             resultCh,
	})

	res := <-resultCh
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

// TestHandleFinalizeSignal_FinalizeFailureReturnsError verifies that when the
// open window cannot be committed (here: an unreachable remote) the signal is
// answered with an error rather than a bogus terminal outcome.
func TestHandleFinalizeSignal_FinalizeFailureReturnsError(t *testing.T) {
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

	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{
		Author:             "alice",
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Result:             resultCh,
	})

	res := <-resultCh
	require.Error(t, res.Err, "an unreachable remote must surface as an error")
	assert.Empty(t, res.Outcome)
	assert.Nil(t, loop.openWindow, "a failed finalize still drops the broken window")
}

// TestHandleFinalizeSignal_DifferentAuthorLeavesWindowOpen verifies that a
// finalize signal whose author does not match the open window's author yields
// NoOpenWindow and leaves the window untouched for its real author.
func TestHandleFinalizeSignal_DifferentAuthorLeavesWindowOpen(t *testing.T) {
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

	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{
		Author:             "bob",
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
		Result:             resultCh,
	})

	res := <-resultCh
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome, "another author's save must not finalize alice's window")
	assert.NotNil(t, loop.openWindow, "alice's open window must be left open")
	assert.Equal(t, "alice", loop.openWindow.Author)
}

// TestHandleFinalizeSignal_DifferentTargetLeavesWindowOpen verifies that a
// finalize signal for a different GitTarget yields NoOpenWindow and leaves the
// open window untouched.
func TestHandleFinalizeSignal_DifferentTargetLeavesWindowOpen(t *testing.T) {
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

	resultCh := make(chan FinalizeResult, 1)
	loop.handleFinalizeSignal(&FinalizeSignal{
		Author:             "alice",
		GitTargetName:      "team-b",
		GitTargetNamespace: "default",
		Result:             resultCh,
	})

	res := <-resultCh
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeNoOpenWindow, res.Outcome, "a finalize for another target must not finalize this window")
	assert.NotNil(t, loop.openWindow, "the team-a window must be left open")
	assert.Equal(t, "team-a", loop.openWindow.GitTarget)
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
