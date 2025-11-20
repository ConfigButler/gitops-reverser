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

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SmartFetch performs a network sync and returns the best available LOCAL branch reference.
// It prioritizes the target branch but always fetches the default branch as a safety net.
//
// Return values (example with target="refs/heads/feature"):
// - "refs/heads/feature", nil: Target found on remote, fetched, ready to checkout.
// - "refs/heads/main", nil:    Target missing on remote, fell back to default branch.
// - "", nil:                   No valid branches found (empty repo).
func SmartFetch(
	ctx context.Context,
	repo *git.Repository,
	target plumbing.ReferenceName, // e.g. "refs/heads/feature" or "HEAD"
	auth transport.AuthMethod,
) (plumbing.ReferenceName, error) {
	remoteName := "origin"
	remote, err := repo.Remote(remoteName)
	if err != nil {
		return "", fmt.Errorf("failed to get remote %s: %w", remoteName, err)
	}

	// 1. Audit: List refs
	refs, err := listRemoteRefs(remote, auth)
	if err != nil {
		return "", err
	}
	if len(refs) == 0 {
		return "", nil
	}

	// 2. Analyze: Find default branch and check target existence
	defaultFull, defaultShort, targetExists := analyzeRemoteRefs(ctx, refs, target.String())

	// 3. Plan: Build RefSpecs based on analysis
	refSpecs := buildSmartRefSpecs(remoteName, defaultFull, defaultShort, target, targetExists)

	// Determine Result (The return value)
	var result plumbing.ReferenceName
	switch {
	case targetExists:
		result = target
	case defaultFull != "":
		result = plumbing.ReferenceName(defaultFull)
	default:
		return "", nil
	}

	// 4. Execute: Fetch
	if len(refSpecs) > 0 {
		err = repo.Fetch(&git.FetchOptions{
			RemoteName: remoteName,
			Auth:       auth,
			RefSpecs:   refSpecs,
			Depth:      1,
			Force:      true,
			Prune:      true,
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return "", fmt.Errorf("smart fetch failed: %w", err)
		}
	}

	// 5. Repair: Fix local symbolic HEAD
	repairRemoteSymbolicHead(repo, remoteName, defaultShort)

	return result, nil
}

func listRemoteRefs(remote *git.Remote, auth transport.AuthMethod) ([]*plumbing.Reference, error) {
	refs, err := remote.List(&git.ListOptions{Auth: auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, nil // Valid state, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list remote refs: %w", err)
	}
	return refs, nil
}

// analyzeRemoteRefs scans the reference list to find the default branch and check if the target exists.
func analyzeRemoteRefs(ctx context.Context, refs []*plumbing.Reference, targetFullStr string) (string, string, bool) {
	logger := log.FromContext(ctx)

	var defaultFull, defaultShort string
	var targetExists bool

	// Map existing refs for O(1) lookup validation
	existingRefs := make(map[string]bool, len(refs))
	for _, ref := range refs {
		existingRefs[ref.Name().String()] = true
	}

	for _, ref := range refs {
		name := ref.Name().String()

		// Check for Default Branch (HEAD)
		if name == "HEAD" && ref.Type() == plumbing.SymbolicReference {
			target := ref.Target().String()
			if existingRefs[target] {
				defaultFull = target
				defaultShort = cleanBranchName(ref.Target().Short())
			} else {
				logger.Info("Remote HEAD is broken (points to missing ref)", "target", target)
			}
		}

		// Check for Target
		if name == targetFullStr {
			targetExists = true
		}
	}

	return defaultFull, defaultShort, targetExists
}

func buildSmartRefSpecs(
	remoteName, defaultFull, defaultShort string,
	target plumbing.ReferenceName,
	targetExists bool,
) []config.RefSpec {
	var refSpecs []config.RefSpec

	// A. Always fetch Default (Safety Net)
	if defaultFull != "" {
		spec := config.RefSpec(fmt.Sprintf("+%s:refs/remotes/%s/%s", defaultFull, remoteName, defaultShort))
		refSpecs = append(refSpecs, spec)
	}

	// B. Fetch Target (If valid and different)
	if targetExists {
		targetFullStr := target.String()
		if defaultFull != targetFullStr {
			spec := config.RefSpec(fmt.Sprintf("+%s:refs/remotes/%s/%s", targetFullStr, remoteName, target.Short()))
			refSpecs = append(refSpecs, spec)
		}
	}
	return refSpecs
}

func repairRemoteSymbolicHead(repo *git.Repository, remoteName, defaultShort string) {
	if defaultShort == "" {
		return
	}
	symRef := plumbing.NewSymbolicReference(
		plumbing.NewRemoteReferenceName(remoteName, "HEAD"),
		plumbing.NewRemoteReferenceName(remoteName, defaultShort),
	)
	_ = repo.Storer.SetReference(symRef)
}

// cleanBranchName handles the edge case where .Short() returns "origin/main" instead of "main".
func cleanBranchName(name string) string {
	if len(name) > 7 && name[:7] == "origin/" {
		return name[7:]
	}
	return name
}
