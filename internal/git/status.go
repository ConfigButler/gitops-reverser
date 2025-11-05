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
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// BranchStatus contains information about a branch in a repository.
type BranchStatus struct {
	// BranchExists indicates if the branch exists on the remote
	BranchExists bool
	// LastCommitSHA is the SHA of the latest commit (HEAD if branch doesn't exist, branch HEAD if it does)
	LastCommitSHA string
}

// GetBranchStatus checks if a branch exists and returns its status.
// It clones/opens the repository, fetches latest changes, and checks branch existence.
// Returns HEAD SHA if branch doesn't exist, branch SHA if it does.
func GetBranchStatus(
	repoURL string,
	branch string,
	auth transport.AuthMethod,
	workDir string,
) (*BranchStatus, error) {
	// Build path for this repo/branch combination
	repoPath := filepath.Join(workDir, "status-check")

	// Clone or open existing repository
	repo, err := Clone(repoURL, repoPath, auth)
	if err != nil {
		return nil, fmt.Errorf("failed to clone repository: %w", err)
	}

	// Fetch latest changes from remote
	fetchOpts := &git.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		Force:      true,
		Depth:      1, // Shallow fetch for speed
	}

	// Fetch (ignore already up-to-date error)
	_ = repo.Fetch(fetchOpts) // Non-fatal: continue with local state if fetch fails

	status := &BranchStatus{}

	// Check if branch exists on remote
	remoteBranchRef := plumbing.NewRemoteReferenceName("origin", branch)
	ref, err := repo.Reference(remoteBranchRef, true)

	if err == nil {
		// Branch exists on remote
		status.BranchExists = true
		status.LastCommitSHA = ref.Hash().String()
		return status, nil
	}

	// Branch doesn't exist, get HEAD SHA as fallback
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		status.BranchExists = false

		// Try to get HEAD
		headRef, headErr := repo.Head()
		if headErr == nil {
			status.LastCommitSHA = headRef.Hash().String()
		} else {
			// Repository might be empty or have no commits
			status.LastCommitSHA = ""
		}

		return status, nil
	}

	// Some other error occurred
	return nil, fmt.Errorf("failed to check branch reference: %w", err)
}
