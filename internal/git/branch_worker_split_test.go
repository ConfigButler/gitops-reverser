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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func configMapTargetEvent(name, username, target string) Event {
	event := configMapEvent(name, username, "")
	event.GitTargetName = target
	event.GitTargetNamespace = "default"
	return event
}

func createPlainGitTarget(t *testing.T, worker *BranchWorker, name, path string) {
	t.Helper()

	require.NoError(t, worker.Client.Create(worker.ctx, &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: configv1alpha1.GitTargetSpec{
			ProviderRef: configv1alpha1.GitProviderReference{
				Kind: "GitProvider",
				Name: worker.GitProviderRef,
			},
			Branch: worker.Branch,
			Path:   path,
		},
	}))
}

func commitsAfterHash(
	t *testing.T,
	repo *git.Repository,
	head plumbing.Hash,
	base plumbing.Hash,
) []*object.Commit {
	t.Helper()

	var newestFirst []*object.Commit
	current := head
	for current != base {
		commit, err := repo.CommitObject(current)
		require.NoError(t, err)
		require.Len(t, commit.ParentHashes, 1, "expected linear history while walking replayed commits")
		newestFirst = append(newestFirst, commit)
		current = commit.ParentHashes[0]
	}

	chronological := make([]*object.Commit, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		chronological = append(chronological, newestFirst[i])
	}
	return chronological
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

// TestCommitGroups_DoesNotPush verifies grouped pending writes can be committed
// locally without ever advancing the remote branch.
func TestCommitGroups_DoesNotPush(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	// First commit cycle: no unpushed commits yet. Two same-author events in
	// one flush should collapse to one grouped commit.
	events := []Event{
		configMapEvent("first", "alice", "team-a"),
		configMapEvent("second", "alice", "team-a"),
	}
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	// Remote must be untouched after commitPendingWrites; only pushPendingCommits
	// publishes work.
	afterCommitRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, initialHash, afterCommitRef.Hash(),
		"remote should not advance during local commit creation; only push publishes")

	// Local repo carries the new commit.
	localRepoPath := worker.repoPathForRemote(remoteURL)
	localRepo, err := git.PlainOpen(localRepoPath)
	require.NoError(t, err)
	localRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialHash, localRef.Hash(),
		"local main should advance with the new commit")

	headCommit, err := localRepo.CommitObject(localRef.Hash())
	require.NoError(t, err)
	require.Len(t, headCommit.ParentHashes, 1, "burst should produce a single grouped commit")
	assert.Equal(t, initialHash, headCommit.ParentHashes[0],
		"the grouped commit should parent directly on the seed commit")

	// pushCycleRootHash should be set so a subsequent push can detect drift.
	assert.False(t, worker.pushCycleRootHash.IsZero(),
		"first commit of a cycle records the rootHash for push validation")
	assert.Equal(t, initialHash, worker.pushCycleRootHash,
		"recorded rootHash must match the remote tip we built upon")
}

// TestCommitGroups_AccumulatesAcrossCalls covers the multi-commit path: a
// second commitPendingWrites call within the same push cycle (hasUnpushedCommits=true) must
// not call PrepareBranch, so the prior local commit is preserved.
func TestCommitGroups_AccumulatesAcrossCalls(t *testing.T) {
	worker, _, remoteURL := setupCommitPushSplitWorker(t)

	firstPendingWrite, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("first", "alice", "team-a")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*firstPendingWrite}, false))
	rootAfterFirst := worker.pushCycleRootHash

	secondPendingWrite, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("second", "bob", "team-b")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*secondPendingWrite}, true))

	assert.Equal(t, rootAfterFirst, worker.pushCycleRootHash,
		"hasUnpushedCommits=true must preserve the rootHash from the first commit")

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

// TestPushPendingCommits_FlushesAccumulated verifies that two grouped pending
// writes followed by a single pushPendingCommits publishes both local commits
// to the remote and clears the rootHash on success.
func TestPushPendingCommits_FlushesAccumulated(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	initialHash := initialRef.Hash()

	events1 := []Event{configMapEvent("first", "alice", "team-a")}
	events2 := []Event{configMapEvent("second", "bob", "team-b")}

	pendingWrite1, err := worker.buildGroupedPendingWrite(worker.ctx, events1)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite1}, false))

	pendingWrite2, err := worker.buildGroupedPendingWrite(worker.ctx, events2)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite2}, true))

	pending1, err := worker.buildGroupedPendingWrite(worker.ctx, events1)
	require.NoError(t, err)
	pending2, err := worker.buildGroupedPendingWrite(worker.ctx, events2)
	require.NoError(t, err)

	// One push publishes everything in one network operation.
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pending1, *pending2}))

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
}

// TestPushPendingCommits_ReplaysOnConflict verifies the replay path: if the
// remote moves between our last commit and our push, pushPendingCommits
// rebuilds the retained pending write on top of the new remote tip and the final
// remote contains both the contending commit and our replayed commits.
func TestPushPendingCommits_ReplaysOnConflict(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	// Build local commits but do not push yet.
	events := []Event{configMapEvent("from-operator", "alice", "team-a")}
	retainedPendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, events)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*retainedPendingWrite}, false))
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, events)
	require.NoError(t, err)

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
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

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

func TestPushPendingCommits_ReplayPreservesPendingWriteCommitOrder(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	events := [][]Event{
		{configMapEvent("alice-first", "alice", "team-a")},
		{configMapEvent("bob-second", "bob", "team-a")},
		{configMapEvent("alice-third", "alice", "team-a")},
	}
	pendingWrites := make([]PendingWrite, 0, len(events))

	for i, batch := range events {
		pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, batch)
		require.NoError(t, err)
		require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, i > 0))
		pendingWrites = append(pendingWrites, *pendingWrite)
	}

	tempDir := t.TempDir()
	otherPath := filepath.Join(tempDir, "other")
	otherRepo, otherWorktree := initLocalRepo(t, otherPath, remoteURL, "main")
	commitFileChange(t, otherWorktree, otherPath, "OUTSIDE.md", "from-other-actor\n")
	require.NoError(t, otherRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	contendingRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	require.NoError(t, worker.pushPendingCommits(pendingWrites))

	finalRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	replayed := commitsAfterHash(t, serverRepo, finalRef.Hash(), contendingRef.Hash())
	require.Len(t, replayed, len(pendingWrites))

	assert.Equal(t, "alice", replayed[0].Author.Name)
	assert.Equal(t, "[CREATE] v1/configmaps/alice-first", replayed[0].Message)
	assert.Equal(t, "bob", replayed[1].Author.Name)
	assert.Equal(t, "[CREATE] v1/configmaps/bob-second", replayed[1].Message)
	assert.Equal(t, "alice", replayed[2].Author.Name)
	assert.Equal(t, "[CREATE] v1/configmaps/alice-third", replayed[2].Message)
	assert.True(t, worker.pushCycleRootHash.IsZero())
}

func TestBranchWorker_Replay_UsesResolvedMetadata_GitTargetDeletedMidBurst(t *testing.T) {
	installFakeSOPSBinary(t)

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

	worker, err := newTestBranchWorker(
		remoteURL,
		"test-repo",
		"main",
		secretTargetObjects(t, "test-repo", "main", "")...,
	)
	require.NoError(t, err)

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "burst-secret",
					"namespace": "default",
				},
				"data": map[string]interface{}{
					"password": "ZG8tbm90LWNvbW1pdA==",
				},
			},
		},
		Identifier: itypes.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "secrets",
			Namespace: "default",
			Name:      "burst-secret",
		},
		Operation:          "CREATE",
		UserInfo:           UserInfo{Username: "alice"},
		GitTargetName:      "secret-target",
		GitTargetNamespace: "default",
	}

	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	targetMetadata := pendingWrite.findTargetMetadata("secret-target", "default")
	require.NotNil(t, targetMetadata.EncryptionConfig, "resolved encryption must be retained on the pending write")
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	require.NoError(t, worker.Client.Delete(worker.ctx, &configv1alpha1.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-target", Namespace: "default"},
	}))
	require.NoError(t, worker.Client.Delete(worker.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sops-age-key", Namespace: "default"},
	}))

	otherPath := filepath.Join(t.TempDir(), "other")
	otherRepo, otherWorktree := initLocalRepo(t, otherPath, remoteURL, "main")
	commitFileChange(t, otherWorktree, otherPath, "OUTSIDE.md", "from-other-actor\n")
	require.NoError(t, otherRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	finalRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	finalCommit, err := serverRepo.CommitObject(finalRef.Hash())
	require.NoError(t, err)
	require.Len(t, finalCommit.ParentHashes, 1, "replay should produce a fresh commit on top of the contending tip")

	verifyPath := filepath.Join(t.TempDir(), "verify")
	_, _ = initLocalRepo(t, verifyPath, remoteURL, "main")

	sopsPath := filepath.Join(verifyPath, "v1", "secrets", "default", "burst-secret.sops.yaml")
	assert.FileExists(t, sopsPath, "replay must reuse the resolved encryption metadata after the target disappears")
	assert.NoFileExists(t, filepath.Join(verifyPath, "v1", "secrets", "default", "burst-secret.yaml"))
	assert.FileExists(
		t,
		filepath.Join(verifyPath, "OUTSIDE.md"),
		"the contending remote commit should still be present",
	)

	content, err := os.ReadFile(sopsPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "sops:")
}

func TestBranchWorker_TransientPushFailure_RetriesSameLocalCommits(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	pendingWrite, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("transient", "alice", "team-a")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	localRepo, err := git.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	localRefBefore, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	remoteRefBefore, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	rootHashBefore := worker.pushCycleRootHash
	rootBranchBefore := worker.pushCycleRootBranch

	pushErr := errors.New("transient push failure")
	originalPush := pushAtomicFn
	originalFetch := fetchRemoteBranchHashFn
	originalSync := syncToRemoteFn
	t.Cleanup(func() {
		pushAtomicFn = originalPush
		fetchRemoteBranchHashFn = originalFetch
		syncToRemoteFn = originalSync
	})

	syncCalled := false
	pushAtomicFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.Hash,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) error {
		return pushErr
	}
	fetchRemoteBranchHashFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) (plumbing.Hash, error) {
		return rootHashBefore, nil
	}
	syncToRemoteFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) (*PullReport, error) {
		syncCalled = true
		return &PullReport{}, nil
	}

	err = worker.pushPendingCommits([]PendingWrite{*pendingWrite})
	require.ErrorIs(t, err, pushErr)
	assert.Equal(t, pushErr, err, "transient failures should preserve the original push error for retry handling")
	assert.False(t, syncCalled, "unchanged remote tip must not trigger replay work")
	assert.Equal(t, rootHashBefore, worker.pushCycleRootHash)
	assert.Equal(t, rootBranchBefore, worker.pushCycleRootBranch)

	localRefAfter, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(
		t,
		localRefBefore.Hash(),
		localRefAfter.Hash(),
		"transient failures must keep the local commit chain intact",
	)

	remoteRefAfter, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, remoteRefBefore.Hash(), remoteRefAfter.Hash(), "transient failures must not rewrite remote state")
}

func TestBranchWorker_PushFollowedByFetchFailure_TreatsAsTransient(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	pendingWrite, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("fetch-failure", "alice", "team-a")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	localRepo, err := git.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	localRefBefore, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	remoteRefBefore, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	rootHashBefore := worker.pushCycleRootHash
	rootBranchBefore := worker.pushCycleRootBranch

	pushErr := errors.New("push failed before remote-state fetch")
	fetchErr := errors.New("remote-state fetch failed")
	originalPush := pushAtomicFn
	originalFetch := fetchRemoteBranchHashFn
	originalSync := syncToRemoteFn
	t.Cleanup(func() {
		pushAtomicFn = originalPush
		fetchRemoteBranchHashFn = originalFetch
		syncToRemoteFn = originalSync
	})

	syncCalled := false
	pushAtomicFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.Hash,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) error {
		return pushErr
	}
	fetchRemoteBranchHashFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) (plumbing.Hash, error) {
		return plumbing.ZeroHash, fetchErr
	}
	syncToRemoteFn = func(
		_ context.Context,
		_ *git.Repository,
		_ plumbing.ReferenceName,
		_ transport.AuthMethod,
	) (*PullReport, error) {
		syncCalled = true
		return &PullReport{}, nil
	}

	err = worker.pushPendingCommits([]PendingWrite{*pendingWrite})
	require.ErrorIs(t, err, pushErr)
	assert.Equal(t, pushErr, err, "fetch failures after a push error should still surface the original push failure")
	assert.False(t, syncCalled, "replay must not start when the post-failure fetch cannot classify the error")
	assert.Equal(t, rootHashBefore, worker.pushCycleRootHash)
	assert.Equal(t, rootBranchBefore, worker.pushCycleRootBranch)

	localRefAfter, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, localRefBefore.Hash(), localRefAfter.Hash())

	remoteRefAfter, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, remoteRefBefore.Hash(), remoteRefAfter.Hash())
}

func TestBranchWorker_AtomicAndGroupedInterleaved_PreservesArrivalOrder(t *testing.T) {
	worker, serverRepo, _ := setupCommitPushSplitWorker(t)

	groupedFirst, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("grouped-first", "alice", "team-a")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*groupedFirst}, false))

	atomicRequest := &WriteRequest{
		Events:        []Event{configMapEvent("atomic-second", "reconciler", "team-a")},
		CommitMode:    CommitModeAtomic,
		CommitMessage: "atomic: second write",
	}
	atomicSecond, err := worker.buildAtomicPendingWrite(worker.ctx, atomicRequest)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*atomicSecond}, true))

	groupedThird, err := worker.buildGroupedPendingWrite(
		worker.ctx,
		[]Event{configMapEvent("grouped-third", "bob", "team-a")},
	)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*groupedThird}, true))

	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*groupedFirst, *atomicSecond, *groupedThird}))

	headRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	headCommit, err := serverRepo.CommitObject(headRef.Hash())
	require.NoError(t, err)
	require.Len(t, headCommit.ParentHashes, 1)
	assert.Equal(t, "[CREATE] v1/configmaps/grouped-third", headCommit.Message)

	atomicCommit, err := serverRepo.CommitObject(headCommit.ParentHashes[0])
	require.NoError(t, err)
	require.Len(t, atomicCommit.ParentHashes, 1)
	assert.Equal(t, "atomic: second write", atomicCommit.Message)

	firstCommit, err := serverRepo.CommitObject(atomicCommit.ParentHashes[0])
	require.NoError(t, err)
	require.Len(t, firstCommit.ParentHashes, 1)
	assert.Equal(t, "[CREATE] v1/configmaps/grouped-first", firstCommit.Message)
}

func TestBranchWorker_Replay_DropsUnitsThatBecomeNoOpAgainstNewRemoteTree(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	event := configMapEvent("already-applied", "alice", "team-a")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	localRepoPath := worker.repoPathForRemote(remoteURL)
	gitPath := filepath.ToSlash(filepath.Join(event.Path, generateFilePath(event.Identifier)))
	desiredContent, err := os.ReadFile(filepath.Join(localRepoPath, gitPath))
	require.NoError(t, err)

	otherPath := filepath.Join(t.TempDir(), "other")
	otherRepo, otherWorktree := initLocalRepo(t, otherPath, remoteURL, "main")
	fullPath := filepath.Join(otherPath, gitPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, desiredContent, 0o600))
	_, err = otherWorktree.Add(gitPath)
	require.NoError(t, err)
	externalHash, err := otherWorktree.Commit("external: already applied desired content", &git.CommitOptions{
		Author: &object.Signature{Name: "External", Email: "external@example.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, otherRepo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	finalRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, externalHash, finalRef.Hash(),
		"replay should not add a commit when the new remote tree already contains the desired content")
	assert.True(t, worker.pushCycleRootHash.IsZero())
}

// TestEventLoop_CommitWindowZero_HonestPerEvent verifies the design's PR2
// promise: with commitWindow=0 each event arrival immediately commits to a
// local Git commit. While the push cooldown is active those local commits
// accumulate in pendingWrites — only a successful push clears them.
func TestEventLoop_CommitWindowZero_HonestPerEvent(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	loop := newBranchWorkerEventLoop(worker, 0)
	// Pretend a push just happened so the cooldown gate is active; the
	// scheduling logic must defer the next push rather than firing it.
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{
			configMapEvent("first", "alice", "team-a"),
			configMapEvent("second", "alice", "team-a"),
		},
		CommitMode: CommitModePerEvent,
	}})

	assert.Nil(t, loop.openWindow, "commitWindow=0 must finalize the open window on every event")
	assert.Len(t, loop.pendingWrites, 2,
		"events land in pendingWrites immediately while the cooldown defers the push")
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

func TestEventLoop_AuthorChangeFinalizesOpenWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{
			configMapEvent("first", "alice", "team-a"),
			configMapEvent("second", "bob", "team-a"),
		},
		CommitMode: CommitModePerEvent,
	}})

	require.Len(t, loop.pendingWrites, 1)
	assert.Equal(t, "alice", loop.pendingWrites[0].Events[0].UserInfo.Username)
	require.NotNil(t, loop.openWindow)
	assert.Equal(t, "bob", loop.openWindow.Author)

	loop.stopTimers()
}

func TestEventLoop_TargetChangeFinalizesOpenWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	createPlainGitTarget(t, worker, "team-b", "team-b")

	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events: []Event{
			configMapTargetEvent("first", "alice", "team-a"),
			configMapTargetEvent("second", "alice", "team-b"),
		},
		CommitMode: CommitModePerEvent,
	}})

	require.Len(t, loop.pendingWrites, 1)
	assert.Equal(t, "team-a", loop.pendingWrites[0].Events[0].GitTargetName)
	require.NotNil(t, loop.openWindow)
	assert.Equal(t, "team-b", loop.openWindow.GitTarget)

	loop.stopTimers()
}

func TestEventLoop_RepeatedPathEditsCollapseInOpenWindow(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	first := configMapEvent("same", "alice", "team-a")
	second := configMapEvent("same", "alice", "team-a")
	second.Operation = "UPDATE"
	second.Object.Object["data"] = map[string]interface{}{"key": "latest"}

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{first, second, configMapEvent("other", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})

	require.NotNil(t, loop.openWindow)
	events := loop.openWindow.orderedEvents()
	require.Len(t, events, 2)
	assert.Equal(t, "same", events[0].Identifier.Name)
	assert.Equal(t, "latest", events[0].Object.Object["data"].(map[string]interface{})["key"])
	assert.Equal(t, "other", events[1].Identifier.Name)

	loop.finalizeOpenWindow()
	require.Len(t, loop.pendingWrites, 1)
	require.Len(t, loop.pendingWrites[0].Events, 2)

	loop.stopTimers()
}

func TestEventLoop_AtomicRequestFinalizesOpenWindowFirst(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("live", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("atomic", "reconciler", "team-a")},
		CommitMode: CommitModeAtomic,
	}})

	assert.Nil(t, loop.openWindow)
	require.Len(t, loop.pendingWrites, 2)
	assert.Equal(t, PendingWriteCommit, loop.pendingWrites[0].Kind)
	assert.Equal(t, "live", loop.pendingWrites[0].Events[0].Identifier.Name)
	assert.Equal(t, PendingWriteAtomic, loop.pendingWrites[1].Kind)
	assert.Equal(t, "atomic", loop.pendingWrites[1].Events[0].Identifier.Name)

	loop.stopTimers()
}

func TestEventLoop_ByteCapFinalizesIncludingTrippingEvent(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	worker.branchBufferMaxBytes = 1
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("oversized", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})

	assert.Nil(t, loop.openWindow)
	require.Len(t, loop.pendingWrites, 1)
	require.Len(t, loop.pendingWrites[0].Events, 1)
	assert.Equal(t, "oversized", loop.pendingWrites[0].Events[0].Identifier.Name)

	loop.stopTimers()
}

func TestEventLoop_AuthorChangeStartsFreshCommitTimer(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("first", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.commitTimer)
	firstTimer := loop.commitTimer

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("second", "bob", "team-a")},
		CommitMode: CommitModePerEvent,
	}})

	require.NotNil(t, loop.commitTimer)
	assert.NotSame(t, firstTimer, loop.commitTimer)
	require.NotNil(t, loop.openWindow)
	assert.Equal(t, "bob", loop.openWindow.Author)

	loop.stopTimers()
}

func TestEventLoop_CrossAuthorSamePathPingPongUsesOnlyAuthorBoundaries(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()

	for _, event := range []Event{
		configMapEvent("F", "alice", "team-a"),
		configMapEvent("F", "bob", "team-a"),
		configMapEvent("X", "alice", "team-a"),
		configMapEvent("F", "alice", "team-a"),
	} {
		loop.handleQueueItem(WorkItem{Request: &WriteRequest{
			Events:     []Event{event},
			CommitMode: CommitModePerEvent,
		}})
	}
	loop.finalizeOpenWindow()

	require.Len(t, loop.pendingWrites, 3)
	require.Len(t, loop.pendingWrites[0].Events, 1)
	assert.Equal(t, "alice", loop.pendingWrites[0].Events[0].UserInfo.Username)
	assert.Equal(t, "F", loop.pendingWrites[0].Events[0].Identifier.Name)
	require.Len(t, loop.pendingWrites[1].Events, 1)
	assert.Equal(t, "bob", loop.pendingWrites[1].Events[0].UserInfo.Username)
	assert.Equal(t, "F", loop.pendingWrites[1].Events[0].Identifier.Name)
	require.Len(t, loop.pendingWrites[2].Events, 2)
	assert.Equal(t, "alice", loop.pendingWrites[2].Events[0].UserInfo.Username)
	assert.Equal(t, "X", loop.pendingWrites[2].Events[0].Identifier.Name)
	assert.Equal(t, "F", loop.pendingWrites[2].Events[1].Identifier.Name)

	loop.stopTimers()
}

func TestEventLoop_AtomicRequest_RespectsCooldownAndUsesNormalPushPath(t *testing.T) {
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)

	initialRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)

	loop := newBranchWorkerEventLoop(worker, time.Second)
	loop.lastPushAt = time.Now()

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("atomic", "reconciler", "team-a")},
		CommitMode: CommitModeAtomic,
	}})

	assert.Nil(t, loop.openWindow, "atomic requests should not remain in the live-event window")
	assert.Len(t, loop.pendingWrites, 1,
		"atomic requests should join pendingWrites and wait for the normal push cycle")
	assert.NotNil(t, loop.pushTimer,
		"active cooldown should defer the push with the regular timer path")

	afterCommitRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, initialRef.Hash(), afterCommitRef.Hash(),
		"remote must not advance while cooldown defers the push")

	localRepo, err := git.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	localRef, err := localRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, initialRef.Hash(), localRef.Hash(),
		"local branch should already contain the atomic commit")

	loop.stopTimers()
}

func TestEventLoop_AtomicPushFailure_DoesNotAdvanceCooldownOrLosePendingWrite(t *testing.T) {
	worker, _, remoteURL := setupCommitPushSplitWorker(t)

	request := &WriteRequest{
		Events:     []Event{configMapEvent("atomic", "reconciler", "team-a")},
		CommitMode: CommitModeAtomic,
	}

	pendingWrite, err := worker.buildAtomicPendingWrite(worker.ctx, request)
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	loop := newBranchWorkerEventLoop(worker, time.Second)
	loop.pendingWrites = []PendingWrite{*pendingWrite}
	loop.pendingWritesBytes = pendingWrite.ByteSize

	remotePath := strings.TrimPrefix(remoteURL, "file://")
	require.NoError(t, os.RemoveAll(remotePath))

	loop.pushPending()

	assert.True(t, loop.lastPushAt.IsZero(), "failed atomic push must not advance cooldown state")
	assert.Len(t, loop.pendingWrites, 1, "failed atomic push must retain pending work for retry")
}
