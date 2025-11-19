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
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAtomicPush: first check our branch expectatiotns!
// First test if situation is equal to last shallowPull, we should do this by arguments
// * featurebranch is at expected parent commit -> Push new event commits
// * HEAD is at expected parent commit -> Push new event commits, also creating branch
// * If the partent commit is empty: create the new branch
// Fail all other scenarios: somebody has been moving our bases

// * Push branch at expected parent commit immediatly as another solution? Very clear that things start to work then.

// TestAtomicPush_PushOnEmpty tries to push a new commit on an totally empty repo
func TestAtomicPush_PushOnEmpty(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath

	// Create bare remote repository with HEAD set to main
	localPath := filepath.Join(tempDir, "local")
	remoteRepo := createBareRepo(t, serverPath)

	// Clone that repo and do a first commit on main
	localRepo, worktree := initLocalRepo(t, localPath, remoteURL, "main")
	createdHash := commitFileChange(t, worktree, localPath, "README.md", "This is cool")

	// New branch in empty repo, so rootHash is plumbing.ZeroHash (let's also include the assumption that we merge to main, so that we can detect if it 'updated')
	err := PushAtomic(ctx, localRepo, plumbing.ZeroHash, plumbing.HEAD, nil)
	require.NoError(t, err)

	// Verify branch exists on server
	ref, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotNil(t, ref)
	require.Equal(t, createdHash, ref.Hash())
}

func initLocalRepo(t *testing.T, localPath, remoteURL, checkoutBranch string) (*git.Repository, *git.Worktree) {
	// Create local repository
	localRepo, err := git.PlainInit(localPath, false)
	require.NoError(t, err)

	// Add remote
	_, err = localRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteURL},
	})
	require.NoError(t, err)

	err = setHead(localRepo, checkoutBranch)
	require.NoError(t, err)

	worktree, err := localRepo.Worktree()
	require.NoError(t, err)
	if checkoutBranch != "" {
		err = worktree.Pull(&git.PullOptions{
			RemoteName:    "origin",
			ReferenceName: plumbing.NewBranchReferenceName(checkoutBranch),
		})
		if !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			require.NoError(t, err)
		}
	}

	return localRepo, worktree
}

func TestAtomicPush_Push(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath

	// Create a remote with one commit
	localPath := filepath.Join(tempDir, "local")
	remoteRepo := createBareRepo(t, serverPath)
	firstCommit := simulateClientCommitOnDisk(t, remoteURL, "main", "README.md", "This is an initialized remote repo")

	// Check it out and create a commit based on that
	localRepo, worktree := initLocalRepo(t, localPath, remoteURL, "main")
	createdCommit := commitFileChange(t, worktree, localPath, "README.md", "This is cool")

	// New branch in empty repo, so rootHash is plumbing.ZeroHash (let's also include the assumption that we merge to main, so that we can detect if it 'updated')
	err := PushAtomic(ctx, localRepo, firstCommit, "main", nil)
	require.NoError(t, err)

	// Verify branch exists on server
	ref, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotNil(t, ref)
	require.Equal(t, createdCommit, ref.Hash())
}

// TestAtomicPush_DetectsMissingBranch tests that push detects when remote branch doesn't exist anymore
func TestAtomicPush_DetectsMissingBranch(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath

	// Create a remote with two commits
	localPath := filepath.Join(tempDir, "local")
	remoteRepo := createBareRepo(t, serverPath)
	simulateClientCommitOnDisk(t, remoteURL, "main", "README.md", "This is an initialized remote repo")
	featureCommit := simulateClientCommitOnDisk(t, remoteURL, "feature", "README.md", "Some change")

	// Check it out and create a commit based on that
	localRepo, worktree := initLocalRepo(t, localPath, remoteURL, "feature")
	createdCommit := commitFileChange(t, worktree, localPath, "README.md", "This is cool")
	require.Equal(t, 3, countDepth(t, localRepo, createdCommit))

	// Remove the branch on the remote (simulating that a merge has been done)
	featureBranch := plumbing.NewBranchReferenceName("feature")
	err := remoteRepo.Storer.RemoveReference(featureBranch)
	require.NoError(t, err)

	// Now we epxect this to error out
	err = PushAtomic(ctx, localRepo, featureCommit, featureBranch, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote went missing")
}

// TestAtomicPush_DetectsUpdatedRemote should check that a change is deteceted
func TestAtomicPush_DetectsUpdatedRemote(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server")
	remoteURL := "file://" + serverPath

	// Create a remote with two commits
	localPath := filepath.Join(tempDir, "local")
	createBareRepo(t, serverPath)
	firstCommit := simulateClientCommitOnDisk(t, remoteURL, "main", "README.md", "This is an initialized remote repo")

	// Check it out and create a commit based on that
	localRepo, worktree := initLocalRepo(t, localPath, remoteURL, "main")
	commitFileChange(t, worktree, localPath, "README.md", "This is cool")

	simulateClientCommitOnDisk(t, remoteURL, "main", "README.md", "Another change on remote!")

	// Now we expect this to error out
	err := PushAtomic(ctx, localRepo, firstCommit, "main", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote received unknown updates")
}
