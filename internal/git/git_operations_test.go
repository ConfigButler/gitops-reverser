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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func skipIfPublicNetworkUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}

	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		t.Skipf("skipping public connectivity test because network timed out: %v", err)
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "temporary failure in name resolution") ||
		strings.Contains(msg, "context deadline exceeded") {
		t.Skipf("skipping public connectivity test because network is unavailable: %v", err)
	}
}

// TestMain initializes the controller-runtime logger before running tests.
// This prevents "log.SetLogger(...) was never called" warnings when code uses log.FromContext().
func TestMain(m *testing.M) {
	// Initialize controller-runtime logger for all tests
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Run all tests
	os.Exit(m.Run())
}

func TestCheckRepo_ConnectivityAndMetadata(t *testing.T) {
	// Test CheckRepo with a local repository that has commits
	tempDir := t.TempDir()

	// Create a repository with commits and multiple branches
	repoPath := filepath.Join(tempDir, "test-repo")
	repo, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create initial commit on main
	err = os.WriteFile(filepath.Join(repoPath, "file.txt"), []byte("content"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("file.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Create a feature branch
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	})
	require.NoError(t, err)

	// Checkout back to main so HEAD is on main
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	})
	require.NoError(t, err)

	// Test CheckRepo on this repository
	ctx := context.Background()
	repoURL := "file://" + repoPath
	repoInfo, err := CheckRepo(ctx, repoURL, nil)

	require.NoError(t, err)
	assert.NotNil(t, repoInfo)
	assert.Equal(t, "master", repoInfo.DefaultBranch.ShortName)
	assert.False(t, repoInfo.DefaultBranch.Unborn)
	assert.Equal(t, 2, repoInfo.RemoteBranchCount)
}

func TestCheckRepo_EmptyRepository(t *testing.T) {
	// Test CheckRepo with a truly empty repository (no commits)
	tempDir := t.TempDir()

	// Create an empty repository (bare repo with no commits)
	repoPath := filepath.Join(tempDir, "empty-repo.git")
	err := os.MkdirAll(repoPath, 0750)
	require.NoError(t, err)

	// Initialize as bare repository (simulates empty remote repo)
	_, err = git.PlainInit(repoPath, true) // true = bare
	require.NoError(t, err)

	// Test CheckRepo on this empty repository
	ctx := context.Background()
	repoURL := "file://" + repoPath
	repoInfo, err := CheckRepo(ctx, repoURL, nil)

	require.NoError(t, err)
	assert.Empty(t, repoInfo.DefaultBranch)
	assert.Equal(t, 0, repoInfo.RemoteBranchCount)
}

func TestCheckRepo_PublicConnectivity(t *testing.T) {
	// Test CheckRepo with a real repository URL
	ctx := context.Background()
	remoteURL := "https://github.com/octocat/Hello-World.git"
	repoInfo, err := CheckRepo(ctx, remoteURL, nil)
	skipIfPublicNetworkUnavailable(t, err)
	tempDir := t.TempDir()

	require.NoError(t, err)
	assert.NotNil(t, repoInfo)
	assert.Equal(t, "master", repoInfo.DefaultBranch.ShortName)
	assert.Positive(t, repoInfo.RemoteBranchCount)

	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "cool-test", nil)
	require.NoError(t, err)

	require.False(t, pullReport.ExistsOnRemote)
	require.Equal(t, "cool-test", pullReport.HEAD.ShortName) // We already report what we will push (but we didnt yet)!
	require.False(t, pullReport.HEAD.Unborn)

	// Verify repository was cloned
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	// Verify HEAD exists (not empty repo)
	localHead, err := localRepo.Storer.Reference(plumbing.HEAD)
	require.Equal(t, plumbing.SymbolicReference, localHead.Type())
	require.NoError(t, err)
	require.Equal(
		t,
		"master",
		localHead.Target().Short(),
	) // We require the local copy to be on the source branch (until we really start to commit!)
	remotes, err := localRepo.Remotes()
	require.NoError(t, err)
	require.Len(
		t,
		remotes,
		1,
	) // Verify that we only fetched 1 remote (master is used to create our feature-branch cool-test
}

func TestCheckRepo_PublicConnectivityEmpty(t *testing.T) {
	// Test CheckRepo with empty repository URL
	ctx := context.Background()
	tempDir := t.TempDir()
	remoteURL := "https://github.com/ConfigButler/empty.git"

	repoInfo, err := CheckRepo(ctx, remoteURL, nil)
	skipIfPublicNetworkUnavailable(t, err)

	require.NoError(t, err)
	assert.Empty(t, repoInfo.DefaultBranch)
	assert.Equal(t, 0, repoInfo.RemoteBranchCount)

	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "cool-test", nil)
	require.NoError(t, err)
	require.NotNil(t, pullReport)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.HEAD.Unborn)

	// Verify repository was cloned
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	// Verify HEAD is filled correctly, and ready to write events as a orphaned branch
	localHead, err := localRepo.Storer.Reference(plumbing.HEAD)
	require.NoError(t, err)
	require.Equal(t, plumbing.SymbolicReference, localHead.Type())
	require.Equal(t, "cool-test", localHead.Target().Short())
}

func TestCheckRepo_OrphanBranches(t *testing.T) {
	// Test checkRepo with a repository that has two orphan branches
	tempDir := t.TempDir()

	// Create a bare repository with two orphan branches
	repoPath := filepath.Join(tempDir, "orphan-repo.git")
	err := os.MkdirAll(repoPath, 0750)
	require.NoError(t, err)

	// Initialize as bare repository
	repo, err := git.PlainInit(repoPath, true) // true = bare
	require.NoError(t, err)

	// Create two orphan branches by directly creating branch references
	// This simulates branches that exist but have no commits

	// Create first orphan branch
	branch1Ref := plumbing.NewBranchReferenceName("feature-1")
	hash1 := plumbing.NewHash("0000000000000000000000000000000000000001") // dummy hash
	ref1 := plumbing.NewHashReference(branch1Ref, hash1)
	err = repo.Storer.SetReference(ref1)
	require.NoError(t, err)

	// Create second orphan branch
	branch2Ref := plumbing.NewBranchReferenceName("feature-2")
	hash2 := plumbing.NewHash("0000000000000000000000000000000000000002") // dummy hash
	ref2 := plumbing.NewHashReference(branch2Ref, hash2)
	err = repo.Storer.SetReference(ref2)
	require.NoError(t, err)

	err = setHeadToMain(repo)
	require.NoError(t, err)

	// Test CheckRepo on this repository
	ctx := context.Background()
	repoURL := "file://" + repoPath
	repoInfo, err := CheckRepo(ctx, repoURL, nil)

	require.NoError(t, err)
	assert.Equal(t, 2, repoInfo.RemoteBranchCount)
}

func TestPrepareBranch_CheckoutNew(t *testing.T) {
	tempDir := t.TempDir()

	// Create a bare remote repository
	remotePath := filepath.Join(tempDir, "remote")
	r := createBareRepo(t, remotePath)

	// Simulate client creating 2 initial commitss
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "stuff.txt", "This is real content")
	hashCreated := simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "anotherfile.txt", "This is also real")
	require.Equal(t, 2, countDepth(t, r, hashCreated))

	// Test PrepareBranch
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "some-branch", nil)
	require.NoError(t, err)

	// Verify repository was cloned
	rLocal, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	// Verify HEAD exists (not empty repo)
	_, err = rLocal.Head()
	require.NoError(t, err)
	require.False(t, pullReport.ExistsOnRemote)
	require.Equal(t, "some-branch", pullReport.HEAD.ShortName)
	require.Equal(t, hashCreated.String(), pullReport.HEAD.Sha)
	require.Equal(t, 1, countDepth(t, rLocal, hashCreated))
}

// countDepth will count the number of commits if you follow the parent commit. This should be one if we 'properly' get our repo.
func countDepth(t *testing.T, r *git.Repository, start plumbing.Hash) int {
	//  2. Get the commit iterator for this branch
	//     This starts from the commit HEAD points to
	i, err := r.Log(&git.LogOptions{
		From: start,
	})
	require.NoError(t, err)

	// 3. Iterate and count
	count := 0
	for {
		_, err := i.Next()
		if err != nil {
			break
		}
		count++
	}

	return count
}

func TestPrepareBranch_CheckoutDefault(t *testing.T) {
	tempDir := t.TempDir()

	// Create a bare remote repository
	remotePath := filepath.Join(tempDir, "remote")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit on someDefaultBranch
	hash := simulateClientCommitOnDisk(t, "file://"+remotePath, "mymain", "hello.txt", "")

	// Test PrepareBranch
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "mymain", nil)
	require.NoError(t, err)
	require.True(t, pullReport.ExistsOnRemote)
	require.Equal(t, "mymain", pullReport.HEAD.ShortName)
	require.Equal(t, hash.String(), pullReport.HEAD.Sha)

	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	head, err := localRepo.Head()
	require.NoError(t, err)
	branchName := head.Name().Short()
	assert.Equal(t, "mymain", branchName)
}

func TestBranchWorker_FirstCommitOnEmptyRepo(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")

	// Create empty remote repository (simulates empty remote repo)
	createBareRepo(t, serverPath)

	// Create test event
	event := createTestEvent(t, "test-pod")

	worker, err := newTestBranchWorker("file://"+serverPath, "test-repo", "main")
	require.NoError(t, err)
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	// Verify repository state
	repo, err := git.PlainOpen(worker.repoPathForRemote("file://" + serverPath))
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	branchName := head.Name().Short()
	assert.Equal(t, "main", branchName)

	// Verify file exists
	filePath := filepath.Join(worker.repoPathForRemote("file://"+serverPath), "v1/pods/default/test-pod.yaml")
	assert.FileExists(t, filePath)
}

func TestMakeHeadUnborn_CleansWorktreeIncludingTrackedFiles(t *testing.T) {
	tempDir := t.TempDir()
	repoPath := filepath.Join(tempDir, "repo")

	repo, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(repoPath, "tracked.txt"), []byte("tracked"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("tracked.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
		Create: true,
	})
	require.NoError(t, err)

	err = os.MkdirAll(filepath.Join(repoPath, "dir/sub"), 0750)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(repoPath, "dir/sub/untracked.txt"), []byte("untracked"), 0600)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(repoPath, ".sops.yaml"), []byte("dummy"), 0600)
	require.NoError(t, err)

	err = makeHeadUnborn(context.Background(), repo, plumbing.NewBranchReferenceName("main"))
	require.NoError(t, err)

	assert.DirExists(t, filepath.Join(repoPath, ".git"))
	assert.NoFileExists(t, filepath.Join(repoPath, "tracked.txt"))
	assert.NoFileExists(t, filepath.Join(repoPath, ".sops.yaml"))
	assert.NoDirExists(t, filepath.Join(repoPath, "dir"))
}

func TestBranchWorker_BranchCreationAndPush(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository
	remotePath := filepath.Join(tempDir, "remote")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit on main
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "file.txt", "hello world")

	// Clone to local
	// Create test event for new branch
	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name":      "feature-pod",
					"namespace": "default",
				},
			},
		},
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "feature-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "test-user",
		},
	}

	worker, err := newTestBranchWorker("file://"+remotePath, "test-repo", "feature")
	require.NoError(t, err)
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	// Verify local branch exists
	localRepo, err := git.PlainOpen(worker.repoPathForRemote("file://" + remotePath))
	require.NoError(t, err)

	head, err := localRepo.Head()
	require.NoError(t, err)
	assert.Equal(t, "feature", head.Name().Short())
}

func TestBranchWorker_ConflictResolution(t *testing.T) {
	// Test the worker conflict resolution path with actual Git push conflicts.
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")

	createBareRepo(t, serverPath)
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "README.md", "This is our first readme")

	worker, err := newTestBranchWorker("file://"+serverPath, "test-repo", "main")
	require.NoError(t, err)
	event := createTestEvent(t, "some-resource")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	// Simulate another client creating conflicting commit in remote after our local commit exists.
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "README.md", "This is our conflicting readme")

	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	serverRepo, err := git.PlainOpen(serverPath)
	require.NoError(t, err)
	head, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, 3, countDepth(t, serverRepo, head.Hash()))
}

func TestBranchWorker_ConcurrentOperations(t *testing.T) {
	// Test concurrent worker writes to simulate multiple GitDestinations.
	tempDir := t.TempDir()

	// Create shared bare remote repository
	remotePath := filepath.Join(tempDir, "remote.git")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "hello.txt", "init")

	// Number of concurrent operations
	numWorkers := 3
	results := make(chan error, numWorkers)

	// Run concurrent writeEvents operations
	for i := range numWorkers {
		go func(workerID int) {
			// Each worker gets its own clone
			worker, err := newTestBranchWorker(
				"file://"+remotePath,
				fmt.Sprintf("test-repo-%d", workerID),
				"main",
			)
			if err != nil {
				results <- err
				return
			}
			event := createTestEvent(t, fmt.Sprintf("pod-worker-%d", workerID))
			pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
			if err != nil {
				results <- err
				return
			}
			if err := worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false); err != nil {
				results <- err
				return
			}
			results <- worker.pushPendingCommits([]PendingWrite{*pendingWrite})
		}(i)
	}

	// Collect results
	var successCount int
	for i := range numWorkers {
		err := <-results
		if err != nil {
			t.Errorf("Worker %d failed: %v", i, err)
		} else {
			successCount++
		}
	}

	// Verify all operations succeeded (conflicts should be resolved)
	assert.Equal(t, numWorkers, successCount, "All concurrent operations should succeed")

	// Verify final repository state has all commits
	finalRepo, err := git.PlainOpen(remotePath)
	require.NoError(t, err)

	// Count commits in repository
	commits := 0
	iter, err := finalRepo.Log(&git.LogOptions{})
	require.NoError(t, err)

	err = iter.ForEach(func(_ *object.Commit) error {
		commits++
		return nil
	})
	require.NoError(t, err)

	// Should have initial commit + numWorkers commits
	assert.Equal(t, 1+numWorkers, commits, "Repository should contain all commits from concurrent operations")
}

func TestPullBranch_BranchLifecycleDetection(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "server")
	localPath := filepath.Join(tempDir, "local")

	// Create bare remote repository
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit
	hash := simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "my-file.txt", "This is cool!")

	// Clone to local
	pullReport, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "feature", nil)
	require.NoError(t, err)
	require.True(t, pullReport.IncomingChanges)
	require.False(t, pullReport.ExistsOnRemote)
	require.Equal(t, "feature", pullReport.HEAD.ShortName)

	// Check if it's there
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	// Get current HEAD
	head, err := localRepo.Head()
	require.NoError(t, err)
	assert.Equal(t, hash, head.Hash())

	// We should be able to run this check on a timer
	pullReport, err = PrepareBranch(context.Background(), "file://"+remotePath, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.IncomingChanges)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.Equal(t, hash.String(), pullReport.HEAD.Sha)

	// Verify my-file.txt exists and has the expected content
	filePath := filepath.Join(localPath, "my-file.txt")
	assert.FileExists(t, filePath)
	fileContent, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "This is cool!", string(fileContent))
}

// setHeadToMain configures HEAD reference to main.
func setHeadToMain(r *git.Repository) error {
	return setHead(r, "main")
}

func TestPullBranch_LocalEditScenario(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository
	remotePath := filepath.Join(tempDir, "remote.git")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit on main
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", "hello.txt", "")

	// Clone to local and create a feature branch
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "main", nil)
	require.NoError(t, err)
	require.True(t, pullReport.IncomingChanges)

	// Create and checkout feature branch
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	worktree, err := localRepo.Worktree()
	require.NoError(t, err)

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	})
	require.NoError(t, err)

	// Test PrepareBranch when branch the local repo contains weird stuff
	pullReport, err = PrepareBranch(context.Background(), "file://"+remotePath, localPath, "main", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.Equal(t, "main", pullReport.HEAD.ShortName)
	assert.False(t, pullReport.HEAD.Unborn)
	assert.False(
		t,
		pullReport.IncomingChanges,
	) // Interesting one: technically nothing was changed, since the 'right' branch did not got any new commits.
}

func TestPullBranch_MergeToDefaultScenario(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath
	localPath := filepath.Join(tempDir, "local")

	// Create bare remote repository
	defaultBranchname := "myuniquedefault" // most people use main but we should also support others
	serverRepo := createBareRepo(t, serverPath)
	setHead(serverRepo, defaultBranchname) // Now it's also default branch: that's what is returned as HEAD to clients

	// Simulate client creating initial commit on myuniquedefault
	simulateClientCommitOnDisk(t, remoteURL, defaultBranchname, "some-file.txt", "Some file")

	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(
		t,
		pullReport.IncomingChanges,
	) // This is the first time we start on main: so that is certainly new content

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.False(t, pullReport.IncomingChanges)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "feature")
	require.NoError(t, err)
	event := createTestEvent(t, "resource1")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)

	mergedHash := simulateSimpleMerge(t, remoteURL, "feature", defaultBranchname)

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(
		t,
		pullReport.IncomingChanges,
	) // That we merged something can change things, it's not at this level our task to fully understand if RELEVANT things changed. We only indicate that new stuff could be there.
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)
	assert.Equal(t, mergedHash.String(), pullReport.HEAD.Sha)
	assert.False(t, pullReport.HEAD.Unborn)

	// Now we do another change: and we should see that it's based upon the default branch
	event = createTestEvent(t, "resource2")
	pendingWrite, err = worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)
	assert.False(t, pullReport.HEAD.Unborn)

	// This is important: initial change + merge + single commit + merge = we must have 3 commits.
	assert.Equal(t, 3, countDepth(t, serverRepo, plumbing.NewHash(pullReport.HEAD.Sha)))
}

func TestPullBranch_DanglingHead(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath
	localPath := filepath.Join(tempDir, "local")

	// 1. Setup Remote with a specific default branch
	defaultBranchname := "myuniquedefault"
	serverRepo := createBareRepo(t, serverPath)
	setHead(serverRepo, defaultBranchname)

	// 2. Create initial content (so the repo isn't empty)
	// This creates 'myuniquedefault'
	simulateClientCommitOnDisk(t, remoteURL, defaultBranchname, "init.txt", "Initial content")

	// 3. Create the 'feature' branch
	// We need this to exist, otherwise if we delete default, there is NOTHING to fetch.
	simulateClientCommitOnDisk(t, remoteURL, "feature", "feature.txt", "Feature content")

	// 4. THE SABOTAGE: Delete 'myuniquedefault' on the server
	// This leaves HEAD pointing to 'refs/heads/myuniquedefault', which no longer exists.
	// This is a "Dangling HEAD".
	err := serverRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName(defaultBranchname))
	require.NoError(t, err)

	// Verify setup: HEAD should now be broken (resolving it fails)
	_, err = serverRepo.Head()
	require.Error(t, err, "Setup failed: HEAD should be broken/dangling")

	// 5. Run PrepareBranch targeting 'feature'
	// Expectation: SmartFetch should detect HEAD is broken, log a warning,
	// but successfully fetch 'feature' because it exists.
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err, "Tool crashed on dangling HEAD")

	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)

	// 6. Write Events
	// Should be able to work on 'feature' normally
	worker, err := newTestBranchWorker(remoteURL, "test-repo", "feature")
	require.NoError(t, err)
	event := createTestEvent(t, "resilient-change")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	// 7. Verify Persistence
	// Ensure we can sync again without issues
	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges, "local checkout should pull the worker-pushed commit")

	// 8. Final depth check on the SERVER
	// Initial (1) + Feature Commit (1) + WriteEvents (1) = 3
	// Note: We check the feature branch specifically because HEAD is broken
	featureHash, err := serverRepo.ResolveRevision(plumbing.Revision("refs/heads/feature"))
	require.NoError(t, err)
	assert.Equal(t, 3, countDepth(t, serverRepo, *featureHash))
}

func TestPullBranch_DanglingHead_NewOrphan(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath
	localPath := filepath.Join(tempDir, "local")

	// 1. Setup Remote with 'main'
	defaultBranchname := "main"
	serverRepo := createBareRepo(t, serverPath)
	setHead(serverRepo, defaultBranchname)
	simulateClientCommitOnDisk(t, remoteURL, defaultBranchname, "init.txt", "Old history")

	// 2. THE SABOTAGE: Delete 'main' on the server
	// Remote state:
	// - HEAD -> refs/heads/main
	// - refs/heads/main -> [DELETED]
	// - refs/heads/feature -> [DOES NOT EXIST]
	err := serverRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName(defaultBranchname))
	require.NoError(t, err)

	// 3. Attempt to work on a NEW feature branch
	targetBranch := "new-feature"

	// PrepareBranch logic:
	// SmartFetch sees HEAD is broken. It sees target is missing and leaves HEAD unborn.
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, targetBranch, nil)
	require.NoError(t, err)

	// Verify Report
	assert.False(t, pullReport.ExistsOnRemote, "PrepareBranch should not bootstrap or create remote branch")
	assert.True(t, pullReport.HEAD.Unborn, "Target branch should stay unborn until first write/bootstrap")
	assert.Empty(t, pullReport.HEAD.Sha)

	// 4. Write Events
	worker, err := newTestBranchWorker(remoteURL, "test-repo", targetBranch)
	require.NoError(t, err)
	event := createTestEvent(t, "orphan-resource")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	// 5. Verify Topology on Server
	// The event commit should be the root commit in the new branch.

	// Fetch the commit from the server
	serverRef, err := serverRepo.Reference(plumbing.NewBranchReferenceName(targetBranch), true)
	require.NoError(t, err, "New branch should have been pushed")

	commitObj, err := serverRepo.CommitObject(serverRef.Hash())
	require.NoError(t, err)

	// The Critical Assertion:
	assert.Equal(t, 0, commitObj.NumParents(), "First event on unborn branch should be a root commit")
	assert.Contains(t, commitObj.Message, "orphan-resource")
	assert.Contains(t, commitObj.Message, "[CREATE]")
}

func TestPullBranch_UnexpectedMergeScenario(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath
	localPath := filepath.Join(tempDir, "local")

	// Create bare remote repository
	serverRepo := createBareRepo(t, serverPath)

	// Simulate client creating initial commit on myuniquedefault
	setHeadToMain(serverRepo)
	simulateClientCommitOnDisk(t, remoteURL, "main", "some-file.txt", "Some file")

	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(
		t,
		pullReport.IncomingChanges,
	) // This is the first time we start on main: so that is certainly new content

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.False(t, pullReport.IncomingChanges)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "feature")
	require.NoError(t, err)
	event := createTestEvent(t, "resource1")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	simulateSimpleMerge(t, remoteURL, "feature", "main")

	// Now we just do a change: without calling PrepareBranch (you never new when something gets merged)
	event = createTestEvent(t, "resource2")
	pendingWrite, err = worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)
	assert.False(t, pullReport.HEAD.Unborn)
	assert.Equal(t, 3, countDepth(t, serverRepo, plumbing.NewHash(pullReport.HEAD.Sha)))
}

func TestBranchWorker_SkipsSemanticallyEquivalentManifest(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server.git")
	remoteURL := "file://" + serverPath
	localPath := filepath.Join(tempDir, "local")

	createBareRepo(t, serverPath)

	initialContent := `apiVersion: shop.example.com/v1
kind: IceCreamOrder
metadata:
  name: alice-order
  namespace: default
spec:
  customerName: Alice
  container: Cone
  scoops:
    - flavor: Vanilla
      quantity: 2
  toppings:
    - Sprinkles
`
	clientPath := filepath.Join(tempDir, "client-main")
	repo, worktree := initLocalRepo(t, clientPath, remoteURL, "main")
	require.NoError(
		t,
		os.MkdirAll(filepath.Join(clientPath, "shop.example.com/v1/icecreamorders/default"), 0o750),
	)
	createdHash := commitFileChange(
		t,
		worktree,
		clientPath,
		"shop.example.com/v1/icecreamorders/default/alice-order.yaml",
		initialContent,
	)
	err := repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+%s:%s", "refs/heads/main", "refs/heads/main")),
		},
	})
	require.NoError(t, err)
	require.NotEqual(t, plumbing.ZeroHash, createdHash)

	_, err = PrepareBranch(context.Background(), remoteURL, localPath, "main", nil)
	require.NoError(t, err)

	event := Event{
		Object: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "shop.example.com/v1",
				"kind":       "IceCreamOrder",
				"metadata": map[string]interface{}{
					"name":      "alice-order",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"container":    "Cone",
					"customerName": "Alice",
					"scoops": []interface{}{
						map[string]interface{}{
							"flavor":   "Vanilla",
							"quantity": int64(2),
						},
					},
					"toppings": []interface{}{"Sprinkles"},
				},
			},
		},
		Identifier: types.ResourceIdentifier{
			Group:     "shop.example.com",
			Version:   "v1",
			Resource:  "icecreamorders",
			Namespace: "default",
			Name:      "alice-order",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "flux",
		},
	}

	worker, err := newTestBranchWorker(remoteURL, "test-repo", "main")
	require.NoError(t, err)
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))

	repo, err = git.PlainOpen(worker.repoPathForRemote(remoteURL))
	require.NoError(t, err)
	head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, createdHash, head.Hash())
}

func TestPullBranch_WhipedRepo(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	serverPathEmpty := filepath.Join(tempDir, "server-empty")
	remoteURL := "file://" + serverPath
	remoteURLEmpty := "file://" + serverPathEmpty
	localPath := filepath.Join(tempDir, "local")

	// Create bare remote repository
	createBareRepo(t, serverPath)
	createBareRepo(t, serverPathEmpty)

	// Simulate client creating initial commit
	simulateClientCommitOnDisk(t, remoteURL, "main", "some-file.txt", "Some file")

	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)

	// This is the first time we start on main: so we do expect IncomingChanges
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.False(t, pullReport.IncomingChanges)

	// Now we just recreate main, let's see if this works
	worker, err := newTestBranchWorker(remoteURL, "test-repo", "feature")
	require.NoError(t, err)
	event := createTestEvent(t, "resource1")
	pendingWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{event})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*pendingWrite}, false))
	require.NoError(t, worker.pushPendingCommits([]PendingWrite{*pendingWrite}))

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)

	// Now execute the same with the empty remote: since no base is available, PrepareBranch keeps target branch unborn.
	pullReport, err = PrepareBranch(context.Background(), remoteURLEmpty, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges)
	assert.True(t, pullReport.HEAD.Unborn)
	assert.Empty(t, pullReport.HEAD.Sha)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)

	// Verify working copy is clean after switching to empty remote
	worktree, err := git.PlainOpen(localPath)
	require.NoError(t, err)
	wt, err := worktree.Worktree()
	require.NoError(t, err)
	status, err := wt.Status()
	require.NoError(t, err)
	assert.True(t, status.IsClean(), "Working copy should be clean after switching to empty remote")

	// No bootstrap files should be auto-created by PrepareBranch anymore.
	_, err = os.Stat(filepath.Join(localPath, "README.md"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(localPath, sopsConfigFileName))
	assert.True(t, os.IsNotExist(err))
}

// Benchmark for prepareBranch shallow clone performance.
func BenchmarkPrepareBranch_ShallowClone(b *testing.B) {
	// 1. SETUP (Do this once, outside the timer)
	tempDir := b.TempDir()
	serverPath := filepath.Join(tempDir, "server.git")
	remoteURL := "file://" + serverPath

	// Helper 1: Create Bare Repo
	// This ensures the repo exists.
	serverRepo := createBareRepo(b, serverPath) // Make sure createBareRepo accepts *testing.B or an interface

	// Helper 2: Set HEAD correctly!
	// CRITICAL FIX: Point HEAD to 'main' so it's not dangling pointing to 'master'
	err := setHead(serverRepo, "main")
	require.NoError(b, err)

	// Helper 3: Create Content
	// Reuse your existing simulator logic (assuming it takes a standard testing interface or you just ignore T)
	// Note: You might need to adapt simulateClientCommitOnDisk to accept *testing.B
	// Or just replicate the push logic here briefly:
	clientTemp := b.TempDir()
	clientRepo, _ := git.PlainInit(clientTemp, false)
	clientRepo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	w, _ := clientRepo.Worktree()
	w.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main"), Create: true, Force: true})
	os.WriteFile(filepath.Join(clientTemp, "file.txt"), []byte("bench"), 0600)
	w.Add("file.txt")
	w.Commit("bench", &git.CommitOptions{Author: &object.Signature{Name: "Bench", When: time.Now()}})
	clientRepo.Push(
		&git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{"+refs/heads/main:refs/heads/main"}},
	)

	// 2. BENCHMARK LOOP
	b.ResetTimer() // Start the clock only now
	for i := range b.N {
		// specific clone path for this iteration
		clonePath := filepath.Join(tempDir, fmt.Sprintf("worker-%d", i))

		// This is what we are measuring
		_, err := PrepareBranch(context.Background(), remoteURL, clonePath, "main", nil)

		if err != nil {
			b.Fatalf("PrepareBranch failed: %v", err)
		}
	}
}

// Benchmark for writing the first commit to an empty repository.
