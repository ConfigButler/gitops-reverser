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
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetBranchStatus_BranchExists(t *testing.T) {
	// Create a test repository
	repoPath := filepath.Join(t.TempDir(), "test-repo")
	repo, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	// Create initial commit
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	testFile := filepath.Join(repoPath, "test.txt")
	err = os.WriteFile(testFile, []byte("test content"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("test.txt")
	require.NoError(t, err)

	commit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	require.NoError(t, err)

	// Create a branch
	branchRef := plumbing.NewBranchReferenceName("feature/test")
	ref := plumbing.NewHashReference(branchRef, commit)
	err = repo.Storer.SetReference(ref)
	require.NoError(t, err)

	// Create remote reference for the branch
	remoteRef := plumbing.NewRemoteReferenceName("origin", "feature/test")
	remoteRefObj := plumbing.NewHashReference(remoteRef, commit)
	err = repo.Storer.SetReference(remoteRefObj)
	require.NoError(t, err)

	// Test GetBranchStatus
	workDir := t.TempDir()
	status, err := GetBranchStatus(repoPath, "feature/test", nil, workDir)

	require.NoError(t, err)
	assert.True(t, status.BranchExists)
	assert.Equal(t, commit.String(), status.LastCommitSHA)
}

func TestGetBranchStatus_BranchDoesNotExist(t *testing.T) {
	// Create a test repository
	repoPath := filepath.Join(t.TempDir(), "test-repo")
	repo, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	// Create initial commit
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	testFile := filepath.Join(repoPath, "test.txt")
	err = os.WriteFile(testFile, []byte("test content"), 0600)
	require.NoError(t, err)

	_, err = worktree.Add("test.txt")
	require.NoError(t, err)

	commit, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	require.NoError(t, err)

	// Don't create the branch - test non-existent branch
	workDir := t.TempDir()
	status, err := GetBranchStatus(repoPath, "nonexistent-branch", nil, workDir)

	require.NoError(t, err)
	assert.False(t, status.BranchExists)
	assert.Equal(t, commit.String(), status.LastCommitSHA, "Should return HEAD SHA when branch doesn't exist")
}

func TestGetBranchStatus_EmptyRepository(t *testing.T) {
	// Create an empty test repository (no commits)
	repoPath := filepath.Join(t.TempDir(), "test-repo")
	_, err := git.PlainInit(repoPath, false)
	require.NoError(t, err)

	// Test GetBranchStatus on empty repo
	workDir := t.TempDir()
	status, err := GetBranchStatus(repoPath, "main", nil, workDir)

	require.NoError(t, err)
	assert.False(t, status.BranchExists)
	assert.Empty(t, status.LastCommitSHA, "Empty repository should have empty SHA")
}
