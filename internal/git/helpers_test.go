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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// initLocalRepo sets up a test workspace by cloning a remote.
// It handles:
// 1. Standard Cloning (defaults to 'main').
// 2. Empty Remote Repositories (fall back to Init + Add Remote).
// 3. Switching branches (handling "unborn" HEADs vs existing history).
// 4. Creating new branches based on Remote versions (if they exist) or local HEAD.
func initLocalRepo(
	tb testing.TB,
	localPath, remoteURL, checkoutBranch string,
) (*git.Repository, *git.Worktree) {
	tb.Helper()

	// --- 1. Clone or Init ---
	repo, err := git.PlainClone(localPath, false, &git.CloneOptions{
		URL: remoteURL,
	})

	// Handle the "Remote is empty" edge case
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		repo, err = git.PlainInit(localPath, false)
		require.NoError(tb, err)

		_, err = repo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{remoteURL},
		})
		require.NoError(tb, err)
	} else {
		// Any other error is a real failure
		require.NoError(tb, err)
	}

	worktree, err := repo.Worktree()
	require.NoError(tb, err)

	// EXIT 1: If no specific branch requested, we are done.
	if checkoutBranch == "" {
		return repo, worktree
	}

	// --- 2. Handle Empty/Unborn Repo ---
	// Check if the repo has ANY history. If 'HEAD' cannot be resolved, it's empty.
	_, err = repo.Head()
	if err != nil {
		// The repo is empty. We cannot "Checkout".
		// We must manually point the symbolic HEAD to the desired branch name.
		err = setHead(repo, checkoutBranch)
		require.NoError(tb, err)
		return repo, worktree
	}

	// --- 3. Handle Repo with History ---
	branchRefName := plumbing.NewBranchReferenceName(checkoutBranch)

	// Try A: Simple Checkout (Assuming local branch exists)
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: branchRefName,
		Force:  true,
	})

	// Try B: Local branch missing -> We must create it.
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		var startPointHash plumbing.Hash

		// Logic: Do we track an existing remote branch (origin/feature), or branch off HEAD (main)?
		remoteRefName := plumbing.NewRemoteReferenceName("origin", checkoutBranch)
		remoteRef, errRef := repo.Reference(remoteRefName, true)

		if errRef == nil {
			// Found on remote: We track origin/feature
			startPointHash = remoteRef.Hash()
		} else {
			// Not on remote: We branch off local HEAD (e.g. main)
			head, errHead := repo.Head()
			require.NoError(tb, errHead, "HEAD must exist if we passed step 2")
			startPointHash = head.Hash()
		}

		err = worktree.Checkout(&git.CheckoutOptions{
			Hash:   startPointHash,
			Branch: branchRefName,
			Create: true,
			Force:  true,
		})
	}

	require.NoError(tb, err)
	return repo, worktree
}

func commitFileChange(tb testing.TB, worktree *git.Worktree, repoFolder, file, content string) plumbing.Hash {
	tb.Helper()

	// Create file
	err := os.WriteFile(filepath.Join(repoFolder, file), []byte(content), 0600)
	require.NoError(tb, err)

	_, err = worktree.Add(file)
	require.NoError(tb, err)

	// Commit
	createdHash, err := worktree.Commit("Client commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Client", Email: "client@example.com", When: time.Now()},
	})
	require.NoError(tb, err)
	return createdHash
}

// simulateClientCommitOnDisk simulates a client cloning, committing, and pushing to a remote using disk storage.
// It creates a fresh temporary workspace for every call to ensure isolation.
func simulateClientCommitOnDisk(tb testing.TB, remoteURL, branchShort, file, content string) plumbing.Hash {
	tb.Helper()

	// 1. Use a fresh directory for this "client"
	// This ensures no leftover state from previous simulations interferes
	clientPath := filepath.Join(tb.TempDir(), fmt.Sprintf("client-%s", branchShort))

	// 2. Reuse initLocalRepo
	// This handles:
	// - Cloning (getting 'main' by default)
	// - Handling empty repos (initing if needed)
	// - creating the new branch 'branchShort' based on 'main' (if 'branchShort' doesn't exist yet)
	repo, worktree := initLocalRepo(tb, clientPath, remoteURL, branchShort)

	// 3. Commit the change
	createdHash := commitFileChange(tb, worktree, clientPath, file, content)

	// 3. Push with Explicit RefSpec (won't push new branches without it)
	refSpecStr := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchShort, branchShort)
	err := repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(refSpecStr),
		},
	})
	require.NoError(tb, err)

	return createdHash
}

// createBareRepo initializes a bare repository at the given path.
func createBareRepo(tb testing.TB, path string) *git.Repository {
	tb.Helper()

	err := os.MkdirAll(path, 0750)
	require.NoError(tb, err)

	repo, err := git.PlainInit(path, true) // true = bare
	require.NoError(tb, err)

	setHeadToMain(repo)

	return repo
}

// simulateSimpleMerge merges source content into destination, pushes the destination,
// and deletes the source branch ref locally and remotely.
func simulateSimpleMerge(tb testing.TB, repoURL, srcBranchShort, dstBranchShort string) plumbing.Hash {
	tb.Helper()

	tempDir := tb.TempDir()
	localPath := filepath.Join(tempDir, "local")
	sourceFilesDir := filepath.Join(tempDir, "source-files")

	// Clone the repository
	repo, err := git.PlainClone(localPath, false, &git.CloneOptions{
		URL: repoURL,
	})
	require.NoError(tb, err)

	worktree, err := repo.Worktree()
	require.NoError(tb, err)

	// Ensure local branch for source exists
	srcBranch := plumbing.NewBranchReferenceName(srcBranchShort)
	if _, err := repo.Reference(srcBranch, true); err != nil {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: srcBranch,
			Create: true,
		})
		require.NoError(tb, err)
	}

	// Ensure local branch for destination exists
	dstBranch := plumbing.NewBranchReferenceName(dstBranchShort)
	if _, err := repo.Reference(dstBranch, true); err != nil {
		err = worktree.Checkout(&git.CheckoutOptions{
			Branch: dstBranch,
			Create: true,
		})
		require.NoError(tb, err)
	}

	// Checkout the source branch and copy its files
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: srcBranch,
	})
	require.NoError(tb, err)

	// Create temp directory for source files
	err = os.MkdirAll(sourceFilesDir, 0750)
	require.NoError(tb, err)

	// Copy source files to temp dir (excluding .git to avoid corruption)
	err = exec.Command("rsync", "-a", "--exclude=.git", localPath+"/", sourceFilesDir+"/").Run()
	require.NoError(tb, err)

	// Checkout the destination branch
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: dstBranch,
	})
	require.NoError(tb, err)

	// Copy source files over destination, overwriting conflicts (preserving .git)
	err = exec.Command("rsync", "-a", "--exclude=.git", sourceFilesDir+"/", localPath+"/").Run()
	require.NoError(tb, err)

	// Add all changes
	_, err = worktree.Add(".")
	require.NoError(tb, err)

	// Commit the merge
	_, err = worktree.Commit(
		fmt.Sprintf("Simple 'rebase' of branch '%s' into '%s'", srcBranchShort, dstBranchShort),
		&git.CommitOptions{
			Author: &object.Signature{
				Name:  "Client",
				Email: "client@example.com",
				When:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			AllowEmptyCommits: true,
		},
	)
	require.NoError(tb, err)

	// Push the updated destination branch
	err = repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+%s:%s", dstBranch, dstBranch)),
			config.RefSpec(":" + srcBranch), // empty local source means delete
		},
	})
	require.NoError(tb, err)

	// Return the newly create commit hash
	head, err := repo.Head()
	require.NoError(tb, err)
	return head.Hash()
}

// Helper function to create test events.
func createTestEvent(tb testing.TB, name string) Event {
	tb.Helper()

	namespace := "default"
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
