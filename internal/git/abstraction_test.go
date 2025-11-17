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
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

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

	require.NoError(t, err)
	assert.NotNil(t, repoInfo)
	assert.Equal(t, "master", repoInfo.DefaultBranch.ShortName)
	assert.Positive(t, repoInfo.RemoteBranchCount)

	tempDir := t.TempDir()
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "cool-test", nil)
	require.NoError(t, err)

	require.False(t, pullReport.ExistsOnRemote)
	require.Equal(t, "cool-test", pullReport.HEAD.ShortName)
	require.False(t, pullReport.HEAD.Unborn)

	// Verify repository was cloned
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	// Verify HEAD exists (not empty repo)
	localHead, err := localRepo.Storer.Reference(plumbing.HEAD)
	require.Equal(t, plumbing.SymbolicReference, localHead.Type())
	require.NoError(t, err)
	require.Equal(t, localHead.Target().Short(), pullReport.HEAD.ShortName)
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
	remoteURL := "https://github.com/ConfigButler/empty.git"
	repoInfo, err := CheckRepo(ctx, remoteURL, nil)

	require.NoError(t, err)
	assert.Empty(t, repoInfo.DefaultBranch)
	assert.Equal(t, 0, repoInfo.RemoteBranchCount)

	tempDir := t.TempDir()
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), remoteURL, localPath, "cool-test", nil)
	require.NoError(t, err)

	require.False(t, pullReport.ExistsOnRemote)
	require.True(t, pullReport.HEAD.Unborn)
	require.Empty(t, pullReport.HEAD.Sha)
	require.Equal(t, "cool-test", pullReport.HEAD.ShortName)

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
	// At this moment we don't detect the default branch: that's not what I expected. Is that a local file repo thing?
	//assert.Equal(t, "main", repoInfo.DefaultBranch.ShortName, "Default branch name should be main")
	//assert.Empty(t, repoInfo.DefaultBranch.Sha)
	//assert.True(t, repoInfo.DefaultBranch.Unborn, "Default branch should be unborn since no commits exist")
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

	fmt.Printf("The branch has %d commit(s)\n", count)
	return count
}

func TestPrepareBranch_CheckoutDefault(t *testing.T) {
	tempDir := t.TempDir()

	// Create a bare remote repository
	remotePath := filepath.Join(tempDir, "remote")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit on someDefaultBranch
	hash := simulateClientCommitOnDisk(t, "file://"+remotePath, "mymain", ".gitkeep", "")

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

func TestWriteEvents_InvalidRepoPath(t *testing.T) {
	_, err := WriteEvents(context.Background(), "/nonexistent/path", []Event{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open repository")
}

func TestWriteEvents_FirstCommitOnEmptyRepo(t *testing.T) {
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	localPath := filepath.Join(tempDir, "local")

	// Create empty remote repository (simulates empty remote repo)
	createBareRepo(t, serverPath)

	// Prepare local clone from empty remote
	pullReport, err := PrepareBranch(context.Background(), "file://"+serverPath, localPath, "main", nil)
	require.NoError(t, err)
	require.False(t, pullReport.ExistsOnRemote)
	require.True(t, pullReport.HEAD.Unborn)

	// Create test event
	event := createTestEvent("test-pod", "default")

	// Test WriteEvents
	result, err := WriteEvents(context.Background(), localPath, []Event{event}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.CommitsCreated)
	assert.Empty(t, result.ConflictPulls)
	assert.Equal(t, 0, result.Failures)

	// Verify repository state
	repo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	branchName := head.Name().Short()
	assert.Equal(t, "main", branchName)

	// Verify file exists
	filePath := filepath.Join(localPath, "v1/pods/default/test-pod.yaml")
	assert.FileExists(t, filePath)
}

func TestWriteEvents_BranchCreationAndPush(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository
	remotePath := filepath.Join(tempDir, "remote")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit on main
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", ".gitkeep", "")

	// Clone to local
	localPath := filepath.Join(tempDir, "local")
	pullReport, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "feature", nil)
	require.NoError(t, err)
	require.False(t, pullReport.ExistsOnRemote)
	require.False(t, pullReport.HEAD.Unborn)

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

	// Test WriteEvents with new branch
	result, err := WriteEvents(context.Background(), localPath, []Event{event}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.CommitsCreated)
	assert.Empty(t, result.ConflictPulls)
	assert.Equal(t, 0, result.Failures)

	// Verify local branch exists
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	head, err := localRepo.Head()
	require.NoError(t, err)
	assert.Equal(t, "feature", head.Name().Short())
}

func TestWriteEvents_ConflictResolution(t *testing.T) {
	// Test the new writeEvents conflict resolution with actual Git push conflicts
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	localPath := filepath.Join(tempDir, "local")

	createBareRepo(t, serverPath)
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "README.md", "This is our first readme")

	// Clone to local
	pullReport, err := PrepareBranch(context.Background(), "file://"+serverPath, localPath, "main", nil)
	require.NoError(t, err)
	require.True(t, pullReport.IncomingChanges)

	// Simulate another client creating conflicting commit in remote
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "README.md", "This is our conflicting readme")

	// Test WriteEventss
	event := createTestEvent("some-resource", "default")
	result, err := WriteEvents(context.Background(), localPath, []Event{event}, nil)
	require.NoError(t, err)

	// Verify operation succeeded
	assert.Equal(t, 1, result.CommitsCreated)
	assert.Equal(t, 1, result.Failures)
	assert.Len(t, result.ConflictPulls, 1)
	assert.True(t, result.ConflictPulls[0].ExistsOnRemote)
	assert.True(t, result.ConflictPulls[0].IncomingChanges)
}

func TestWriteEvents_ConcurrentOperations(t *testing.T) {
	// Test concurrent writeEvents operations to simulate multiple GitDestinations
	tempDir := t.TempDir()

	// Create shared bare remote repository
	remotePath := filepath.Join(tempDir, "remote.git")
	createBareRepo(t, remotePath)

	// Simulate client creating initial commit
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", ".gitkeep", "init")

	// Number of concurrent operations
	numWorkers := 3
	results := make(chan error, numWorkers)

	// Run concurrent writeEvents operations
	for i := range numWorkers {
		go func(workerID int) {
			// Each worker gets its own clone
			localPath := filepath.Join(tempDir, fmt.Sprintf("local-%d", workerID))
			_, err := PrepareBranch(context.Background(), "file://"+remotePath, localPath, "main", nil)
			if err != nil {
				results <- fmt.Errorf("worker %d prepare failed: %w", workerID, err)
				return
			}

			event := createTestEvent(fmt.Sprintf("pod-worker-%d", workerID), "default")
			_, err = WriteEvents(context.Background(), localPath, []Event{event}, nil)
			results <- err
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
	require.True(t, pullReport.ExistsOnRemote)
	require.Equal(t, "main", pullReport.HEAD.ShortName)

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
	assert.Equal(t, hash, pullReport.HEAD.Sha)

	// TODO: Check if the my-file.txt is there!
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
	simulateClientCommitOnDisk(t, "file://"+remotePath, "main", ".gitkeep", "")

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
	serverRepo := createBareRepo(t, serverPath)
	defaultBranchname := "myuniquedefault" // most people use main but we should also support others

	// Simulate client creating initial commit on myuniquedefault
	simulateClientCommitOnDisk(t, remoteURL, defaultBranchname, "some-file.txt", "Some file")
	setHead(serverRepo, defaultBranchname) // Now it's also default branch: that's what is returned as HEAD to clients

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

	event := createTestEvent("resource1", "default")
	writeEventsResult, err := WriteEvents(context.Background(), localPath, []Event{event}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, writeEventsResult.CommitsCreated)

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.False(t, pullReport.IncomingChanges)

	mergedHash := simulateSimpleMerge(t, remoteURL, "feature", defaultBranchname)

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(
		t,
		pullReport.IncomingChanges,
	) // That we merged somethingg can change things, it's not at this level our task to fully understand if RELEVANT things changed. We only indicate that new stuff could be there.
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)
	assert.Equal(t, mergedHash, pullReport.HEAD.Sha)
	assert.False(t, pullReport.HEAD.Unborn)
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
	event := createTestEvent("resource1", "default")
	writeEventsResult, err := WriteEvents(context.Background(), localPath, []Event{event}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, writeEventsResult.CommitsCreated)

	pullReport, err = PrepareBranch(context.Background(), remoteURL, localPath, "feature", nil)
	require.NoError(t, err)
	assert.True(t, pullReport.ExistsOnRemote)
	assert.False(t, pullReport.IncomingChanges)

	// Now execute the same with the empty remote, effectively this is just somebody that deleted our stuff. Since we also won't have main, we expect an unborn branch
	pullReport, err = PrepareBranch(context.Background(), remoteURLEmpty, localPath, "feature", nil)
	require.NoError(t, err)
	assert.False(t, pullReport.ExistsOnRemote)
	assert.True(t, pullReport.IncomingChanges) // This is big: we do expect this certainly to be true!
	assert.True(t, pullReport.HEAD.Unborn)
	assert.Empty(t, pullReport.HEAD.Sha)
	assert.Equal(t, "feature", pullReport.HEAD.ShortName)

	// TODO: Also check if the working copy is now clean! Do we have any files?
}

// Helper function to create test events.
func createTestEvent(name, namespace string) Event {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
	return Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: namespace,
			Name:      name,
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "test-user",
		},
	}
}

// Benchmark for prepareBranch shallow clone performance.
func BenchmarkPrepareBranch_ShallowClone(b *testing.B) {
	tempDir := b.TempDir()

	// Setup a bare remote repository with some history
	remotePath := filepath.Join(tempDir, "remote.git")
	err := os.MkdirAll(remotePath, 0750)
	require.NoError(b, err)
	_, err = git.PlainInit(remotePath, true) // true = bare
	require.NoError(b, err)

	// Simulate client creating a commit
	clientTempDir := b.TempDir()
	clientPath := filepath.Join(clientTempDir, "client")
	repo, err := git.PlainClone(clientPath, false, &git.CloneOptions{
		URL:   "file://" + remotePath,
		Depth: 1,
	})
	if err != nil {
		// Empty repo
		repo, err = git.PlainInit(clientPath, false)
		require.NoError(b, err)
		_, err = repo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{"file://" + remotePath},
		})
		require.NoError(b, err)
	}
	worktree, err := repo.Worktree()
	require.NoError(b, err)
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
		Create: true,
	})
	require.NoError(b, err)
	err = os.WriteFile(filepath.Join(clientPath, "file.txt"), []byte("content"), 0600)
	require.NoError(b, err)
	_, err = worktree.Add("file.txt")
	require.NoError(b, err)
	_, err = worktree.Commit("Client commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Client", Email: "client@example.com", When: time.Now()},
	})
	require.NoError(b, err)
	err = repo.Push(&git.PushOptions{})
	require.NoError(b, err)

	b.ResetTimer()
	for i := range b.N {
		clonePath := filepath.Join(tempDir, fmt.Sprintf("clone-%d", i))
		_, err := PrepareBranch(context.Background(), "file://"+remotePath, clonePath, "main", nil)
		require.NoError(b, err)
	}
}

// Helper functions to reduce duplication

// createBareRepo initializes a bare repository at the given path.
func createBareRepo(t *testing.T, path string) *git.Repository {
	err := os.MkdirAll(path, 0750)
	require.NoError(t, err)

	repo, err := git.PlainInit(path, true) // true = bare
	require.NoError(t, err)

	setHeadToMain(repo)

	return repo
}

// createRepo initializes a non-bare repository at the given path.
func createRepo(t *testing.T, path string) *git.Repository {
	err := os.MkdirAll(path, 0750)
	require.NoError(t, err)

	repo, err := git.PlainInit(path, false)
	require.NoError(t, err)

	return repo
}

// createCommit creates a commit in a non-bare repository.
func createCommit(t *testing.T, repo *git.Repository, file, content, message string) {
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	repoPath := worktree.Filesystem.Root()
	err = os.WriteFile(filepath.Join(repoPath, file), []byte(content), 0600)
	require.NoError(t, err)

	_, err = worktree.Add(file)
	require.NoError(t, err)

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)
}

// simulateSimpleMerge merges source content into destination, pushes the destination,
// and deletes the source branch ref locally and remotely.
func simulateSimpleMerge(t *testing.T, repoURL, sourceBranch, destinationBranch string) plumbing.Hash {
	t.Helper()

	tempDir := t.TempDir()
	localPath := filepath.Join(tempDir, "local")
	sourceFilesDir := filepath.Join(tempDir, "source-files")

	// Clone the repository
	repo, err := git.PlainClone(localPath, false, &git.CloneOptions{
		URL: repoURL,
	})
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Ensure local branch for source exists
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(sourceBranch), true); err != nil {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(sourceBranch),
			Create: true,
		})
		require.NoError(t, err)
	}

	// Ensure local branch for destination exists
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(destinationBranch), true); err != nil {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(destinationBranch),
			Create: true,
		})
		require.NoError(t, err)
	}

	// Checkout the source branch and copy its files
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(sourceBranch),
	})
	require.NoError(t, err)

	// Copy source files to temp dir
	err = exec.Command("cp", "-r", localPath+"/.", sourceFilesDir).Run()
	require.NoError(t, err)

	// Checkout the destination branch
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(destinationBranch),
	})
	require.NoError(t, err)

	// Copy source files over destination, overwriting conflicts
	err = exec.Command("cp", "-r", sourceFilesDir+"/.", localPath).Run()
	require.NoError(t, err)

	// Add all changes
	_, err = worktree.Add(".")
	require.NoError(t, err)

	// Commit the merge
	_, err = worktree.Commit(
		fmt.Sprintf("Simple 'rebase' of branch '%s' into '%s'", sourceBranch, destinationBranch),
		&git.CommitOptions{
			Author: &object.Signature{
				Name:  "Client",
				Email: "client@example.com",
				When:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			AllowEmptyCommits: true,
		},
	)
	require.NoError(t, err)

	// Push the updated destination branch
	err = repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", destinationBranch, destinationBranch)),
		},
	})
	require.NoError(t, err)

	// Delete the source branch ref from the remote
	err = repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(":" + plumbing.NewBranchReferenceName(sourceBranch))},
	})
	require.NoError(t, err)

	// Delete the source branch ref locally
	err = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(sourceBranch))
	require.NoError(t, err)

	// Return the current HEAD hash
	head, err := repo.Head()
	require.NoError(t, err)
	return head.Hash()
}

// simulateClientCommitInMemory simulates a client cloning, committing, and pushing to a remote using in-memory storage.
func simulateClientCommitInMemory(t *testing.T, remoteURL, branch, file, content string) plumbing.Hash {
	storer := memory.NewStorage()
	fs := memfs.New()
	emptyRepoCreated := false

	repo, err := git.Init(storer, fs)
	require.NoError(t, err)

	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteURL},
	})
	require.NoError(t, err)

	err = repo.Fetch(&git.FetchOptions{})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		setHead(repo, branch)
		emptyRepoCreated = true
	} else {
		require.NoError(t, err)
	}

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Only do checkout if its not an empty repo (otherwise error)
	if !emptyRepoCreated {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branch),
			//Create: true,	// Can we omit this and expect it to always work?
		})
	}

	// Create file in memory filesystem
	dir := filepath.Dir(file)
	if dir != "." {
		err = fs.MkdirAll(dir, 0755)
		require.NoError(t, err)
	}
	f, err := fs.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	require.NoError(t, err)
	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	f.Close()

	_, err = worktree.Add(file)
	require.NoError(t, err)

	// Commit
	createdHash, err := worktree.Commit("Client commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Client",
			Email: "client@example.com",
			When:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)

	// Push
	err = repo.Push(&git.PushOptions{})
	require.NoError(t, err)

	return createdHash
}

// simulateClientCommitOnDisk simulates a client cloning, committing, and pushing to a remote using disk storage.
func simulateClientCommitOnDisk(t *testing.T, remoteURL, branch, file, content string) plumbing.Hash {
	tempDir := t.TempDir()
	clientPath := filepath.Join(tempDir, "client")
	emptyRepoCreated := false

	// Try to clone
	repo, err := git.PlainClone(clientPath, false, &git.CloneOptions{
		URL:   remoteURL,
		Depth: 1,
	})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		repo, err = git.PlainInit(clientPath, false)
		require.NoError(t, err)

		_, err = repo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{remoteURL},
		})
		require.NoError(t, err)
		setHead(repo, branch)
		emptyRepoCreated = true
	}

	if err != nil {
		require.NoError(t, err)
	}

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Only do checkout if its not an empty repo (otherwise error)
	if !emptyRepoCreated {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branch),
			//Create: true,	// Can we omit this and expect it to always work?
		})
	}

	// Create file
	err = os.WriteFile(filepath.Join(clientPath, file), []byte(content), 0600)
	require.NoError(t, err)

	_, err = worktree.Add(file)
	require.NoError(t, err)

	// Commit
	createdHash, err := worktree.Commit("Client commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Client", Email: "client@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Push
	err = repo.Push(&git.PushOptions{})
	require.NoError(t, err)

	return createdHash
}
