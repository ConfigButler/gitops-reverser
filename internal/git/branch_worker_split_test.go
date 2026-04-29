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
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// configMapEvent builds a ConfigMap CREATE event for the given name.
func configMapEvent(name, username, path string) Event {
	return Event{
		Operation: "CREATE",
		Identifier: itypes.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "configmaps",
			Namespace: "default",
			Name:      name,
		},
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"key": name,
				},
			},
		},
		UserInfo: UserInfo{Username: username},
		Path:     path,
	}
}

// setupCommitPushSplitWorker prepares a worker pointing at a freshly seeded
// remote so commit and push paths can be exercised end to end.
func setupCommitPushSplitWorker(t *testing.T) (*BranchWorker, *git.Repository, string) {
	t.Helper()
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	serverRepo := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	commitFileChange(t, seedWorktree, seedPath, "README.md", "seed\n")
	require.NoError(t, seedRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &configv1alpha1.GitProvider{
		Spec: configv1alpha1.GitProviderSpec{URL: remoteURL},
	}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	worker := NewBranchWorker(k8sClient, logr.Discard(), "test-repo", "default", "main", nil, 0)
	worker.ctx = ctx
	return worker, serverRepo, remoteURL
}

// TestCommitGroups_DoesNotPush verifies that commitGroups produces local
// commits without ever advancing the remote branch.
func TestCommitGroups_DoesNotPush(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	// First commit cycle: no pending events yet.
	require.NoError(t, worker.commitGroups([]Event{configMapEvent("first", "alice", "team-a")}, false))

	// Remote must be untouched after commitGroups; only pushPendingCommits
	// publishes work.
	afterCommitRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, initialHash, afterCommitRef.Hash(),
		"remote should not advance during commitGroups; only push publishes")

	// Local repo carries the new commit.
	localRepoPath := worker.repoPathForRemote(remoteURL)
	localRepo, err := git.PlainOpen(localRepoPath)
	require.NoError(t, err)
	localRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialHash, localRef.Hash(),
		"local main should advance with the new commit")

	// pushCycleRootHash should be set so a subsequent push can detect drift.
	assert.False(t, worker.pushCycleRootHash.IsZero(),
		"first commit of a cycle records the rootHash for push validation")
	assert.Equal(t, initialHash, worker.pushCycleRootHash,
		"recorded rootHash must match the remote tip we built upon")
}

// TestCommitGroups_AccumulatesAcrossCalls covers the multi-commit path: a
// second commitGroups call within the same push cycle (hasPending=true) must
// not call PrepareBranch, so the prior local commit is preserved.
func TestCommitGroups_AccumulatesAcrossCalls(t *testing.T) {
	worker, _, remoteURL := setupCommitPushSplitWorker(t)

	require.NoError(t, worker.commitGroups([]Event{configMapEvent("first", "alice", "team-a")}, false))
	rootAfterFirst := worker.pushCycleRootHash

	require.NoError(t, worker.commitGroups([]Event{configMapEvent("second", "bob", "team-b")}, true))

	assert.Equal(t, rootAfterFirst, worker.pushCycleRootHash,
		"hasPending=true must preserve the rootHash from the first commit")

	localRepoPath := worker.repoPathForRemote(remoteURL)
	localRepo, err := git.PlainOpen(localRepoPath)
	require.NoError(t, err)

	headRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	// Walk the parent chain — both local commits should be present without a
	// reset between them.
	commit, err := localRepo.CommitObject(headRef.Hash())
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1)

	parentCommit, err := localRepo.CommitObject(commit.ParentHashes[0])
	require.NoError(t, err)
	require.Len(t, parentCommit.ParentHashes, 1, "first commit should still chain to the seed")
}

// TestPushPendingCommits_FlushesAccumulated verifies that two commitGroups
// calls followed by a single pushPendingCommits publishes both local commits
// to the remote and clears the rootHash on success.
func TestPushPendingCommits_FlushesAccumulated(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	events1 := []Event{configMapEvent("first", "alice", "team-a")}
	events2 := []Event{configMapEvent("second", "bob", "team-b")}

	require.NoError(t, worker.commitGroups(events1, false))
	require.NoError(t, worker.commitGroups(events2, true))

	// One push publishes everything in one network operation.
	require.NoError(t, worker.pushPendingCommits(append(append([]Event{}, events1...), events2...)))

	afterPushRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialHash, afterPushRef.Hash(),
		"remote must advance after pushPendingCommits succeeds")

	commit, err := serverRepo.CommitObject(afterPushRef.Hash())
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1, "expected linear history")

	parentCommit, err := serverRepo.CommitObject(commit.ParentHashes[0])
	require.NoError(t, err)
	require.Len(t, parentCommit.ParentHashes, 1, "second-from-last commit chains to the seed")

	assert.True(t, worker.pushCycleRootHash.IsZero(),
		"successful push clears pushCycleRootHash")
	assert.Empty(t, string(worker.pushCycleRootBranch),
		"successful push clears pushCycleRootBranch")
}

// TestPushPendingCommits_ReplaysOnConflict verifies the replay path: if the
// remote moves between our last commit and our push, pushPendingCommits
// rebuilds the unpushed events on top of the new remote tip and the final
// remote contains both the contending commit and our replayed commits.
func TestPushPendingCommits_ReplaysOnConflict(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	// Build local commits but do not push yet.
	events := []Event{configMapEvent("from-operator", "alice", "team-a")}
	require.NoError(t, worker.commitGroups(events, false))

	// While we hold the local commits, an external party advances main.
	tempDir := t.TempDir()
	otherSeedPath := filepath.Join(tempDir, "other-seed")
	otherRepo, otherWorktree := initLocalRepo(t, otherSeedPath, remoteURL, "main")
	commitFileChange(t, otherWorktree, otherSeedPath, "OUTSIDE.md", "from-other-actor\n")
	require.NoError(t, otherRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	contendingRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	// Our push should hit non-FF, replay, and succeed.
	require.NoError(t, worker.pushPendingCommits(events))

	finalRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, contendingRef.Hash(), finalRef.Hash(),
		"replay should produce new commits on top of the contending tip")

	// The final commit's parent chain must include the contending commit.
	finalCommit, err := serverRepo.CommitObject(finalRef.Hash())
	require.NoError(t, err)
	require.Len(t, finalCommit.ParentHashes, 1)
	assert.Equal(t, contendingRef.Hash(), finalCommit.ParentHashes[0],
		"replayed commit should parent on the contending external commit")

	// Successful push clears push-cycle state.
	assert.True(t, worker.pushCycleRootHash.IsZero())
}

// TestEventLoop_CommitWindowZero_HonestPerEvent verifies the design's PR2
// promise: with commitWindow=0 each event arrival immediately commits to a
// local Git commit. While the push cooldown is active those local commits
// accumulate in unpushedEvents — only a successful push clears them.
func TestEventLoop_CommitWindowZero_HonestPerEvent(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	loop := newBranchWorkerEventLoop(worker, 0)
	// Pretend a push just happened so the cooldown gate is active; the
	// scheduling logic must defer the next push rather than firing it.
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("first", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})

	assert.Empty(t, loop.buffer, "commitWindow=0 must drain the buffer on every event")
	assert.Len(t, loop.unpushedEvents, 1,
		"events land in unpushedEvents immediately while the cooldown defers the push")
	assert.NotNil(t, loop.pushTimer,
		"cooldown active → a one-shot pushTimer is scheduled, not an immediate push")

	// Remote is still untouched: the local commit is honest, the push is
	// throttled by the cooldown.
	afterCommitRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, initialRef.Hash(), afterCommitRef.Hash(),
		"remote must not advance while the cooldown holds the push back")

	// The local commit must already be in place.
	localRepo, err := git.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	localRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialRef.Hash(), localRef.Hash(),
		"local main should advance with the per-event commit")

	loop.stopTimers()
}
