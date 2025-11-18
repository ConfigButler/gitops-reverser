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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAtomicPush_BasicPush tests the basic atomic push functionality.
func TestAtomicPush_BasicPush(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository (server)
	serverPath := filepath.Join(tempDir, "server.git")
	err := os.MkdirAll(serverPath, 0750)
	require.NoError(t, err)
	serverRepo, err := git.PlainInit(serverPath, true) // bare=true
	require.NoError(t, err)

	// Create local repository
	localPath := filepath.Join(tempDir, "local")
	localRepo, err := git.PlainInit(localPath, false)
	require.NoError(t, err)

	// Add remote
	_, err = localRepo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"file://" + serverPath},
	})
	require.NoError(t, err)

	// Set HEAD to main
	err = setHead(localRepo, "main")
	require.NoError(t, err)

	// Create initial commit
	worktree, err := localRepo.Worktree()
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(localPath, "test.txt"), []byte("hello"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("test.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Test atomic push
	ctx := context.Background()
	err = push(ctx, localRepo, nil)
	require.NoError(t, err)

	// Verify branch exists on server
	ref, err := serverRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotNil(t, ref)
}

// TestAtomicPush_DetectsMissingBranch tests that push detects when remote branch doesn't exist.
func TestAtomicPush_DetectsMissingBranch(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository with main branch
	serverPath := filepath.Join(tempDir, "server.git")
	createBareRepo(t, serverPath)

	// Create initial commit on main
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "file.txt", "content")

	// Use PrepareBranch to properly set up local repo with feature branch
	localPath := filepath.Join(tempDir, "local")
	_, err := PrepareBranch(context.Background(), "file://"+serverPath, localPath, "feature", nil)
	require.NoError(t, err)

	// Open repo and create a commit on feature
	localRepo, err := git.PlainOpen(localPath)
	require.NoError(t, err)

	worktree, err := localRepo.Worktree()
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(localPath, "feature.txt"), []byte("feature content"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("feature.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Feature commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Test atomic push - should succeed (creating new branch based on main)
	ctx := context.Background()
	err = push(ctx, localRepo, nil)
	require.NoError(t, err)
}

// TestAtomicPush_PreventsDivergedBranch tests that push prevents creating diverged branches.
func TestAtomicPush_PreventsDivergedBranch(t *testing.T) {
	tempDir := t.TempDir()

	// Create bare remote repository
	serverPath := filepath.Join(tempDir, "server.git")
	createBareRepo(t, serverPath)

	// Create initial commit on main
	simulateClientCommitOnDisk(t, "file://"+serverPath, "main", "initial.txt", "initial")

	// Create local clone with feature branch
	localPath := filepath.Join(tempDir, "local")
	localRepo, err := git.PlainClone(localPath, false, &git.CloneOptions{
		URL:   "file://" + serverPath,
		Depth: 1,
	})
	require.NoError(t, err)

	// Create and push feature branch
	worktree, err := localRepo.Worktree()
	require.NoError(t, err)

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	})
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(localPath, "feature.txt"), []byte("feature"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("feature.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Feature commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// First push should succeed
	ctx := context.Background()
	err = push(ctx, localRepo, nil)
	require.NoError(t, err)

	// Simulate feature branch being merged and deleted on server
	simulateSimpleMerge(t, "file://"+serverPath, "feature", "main")

	// Create another commit locally (still on old feature branch)
	err = os.WriteFile(filepath.Join(localPath, "another.txt"), []byte("another"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("another.txt")
	require.NoError(t, err)

	_, err = worktree.Commit("Another commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Push should fail (parent not on remote - branch was merged and deleted)
	err = push(ctx, localRepo, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent commit not on remote")
}
