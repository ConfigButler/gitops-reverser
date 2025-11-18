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
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/storage"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// push performs an atomic push operation in a single network session.
// It checks if the remote branch exists before pushing to prevent creating diverged branches.
func push(
	ctx context.Context,
	repo *git.Repository,
	auth transport.AuthMethod,
) error {
	logger := log.FromContext(ctx)

	// Get current branch name and local commit
	headRef, err := repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return fmt.Errorf("failed to get HEAD reference: %w", err)
	}
	branch := headRef.Target()
	branchName := branch.Short()

	localBranchRef, err := repo.Reference(headRef.Target(), true)
	if err != nil {
		return fmt.Errorf("failed to get local branch reference: %w", err)
	}
	localHash := localBranchRef.Hash()

	// Get remote configuration
	remote, err := repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("failed to get remote: %w", err)
	}

	// Establish transport endpoint
	endpoint, err := transport.NewEndpoint(remote.Config().URLs[0])
	if err != nil {
		return fmt.Errorf("failed to create endpoint: %w", err)
	}

	// Get the transport client
	transportClient, err := client.NewClient(endpoint)
	if err != nil {
		return fmt.Errorf("failed to create transport client: %w", err)
	}

	// Create receive-pack session (single session for verification and push)
	session, err := transportClient.NewReceivePackSession(endpoint, auth)
	if err != nil {
		return fmt.Errorf("failed to create receive-pack session: %w", err)
	}
	defer session.Close()

	// Phase 1: Get advertised references (remote state)
	refs, err := session.AdvertisedReferences()
	if err != nil {
		return fmt.Errorf("failed to get advertised references: %w", err)
	}

	// Check if target branch exists on remote
	remoteHash, found := refs.References[branch.String()]

	if !found {
		// Remote branch doesn't exist - check if our parent commit exists on remote
		// If yes, we're creating a new branch from an existing commit (safe)
		// If no, we're trying to resurrect a deleted diverged branch (unsafe)
		logger.Info("Remote branch not found, checking if based on existing remote commit", "branch", branchName)

		commit, err := repo.CommitObject(localHash)
		if err != nil {
			return fmt.Errorf("failed to get local commit: %w", err)
		}

		// Check if parent commit exists on remote
		if len(commit.ParentHashes) > 0 {
			parentHash := commit.ParentHashes[0]
			parentExists := false
			for _, remoteRef := range refs.References {
				if remoteRef == parentHash {
					parentExists = true
					break
				}
			}

			if !parentExists {
				logger.Info("Parent commit not found on remote, aborting to prevent divergence",
					"branch", branchName, "parent", parentHash)
				return fmt.Errorf("remote branch %s does not exist and parent commit not on remote (may have been merged)", branchName)
			}

			logger.Info("Parent commit found on remote, allowing branch creation",
				"branch", branchName, "parent", parentHash)
		} else {
			// No parent = orphan/root commit, allow it
			logger.Info("Creating orphan branch", "branch", branchName)
		}
	} else {
		// Phase 2: Branch exists on remote - validate we have that commit locally
		_, err = repo.CommitObject(remoteHash)
		if err != nil {
			logger.Info("Remote hash not found locally, cannot calculate packfile",
				"remoteHash", remoteHash, "localHash", localHash)
			return fmt.Errorf("remote commit %s not in local shallow history: %w", remoteHash, err)
		}

		// If local == remote, we're already up to date
		if localHash == remoteHash {
			logger.Info("Already up to date", "branch", branchName, "hash", localHash)
			return nil
		}
	}

	// Phase 3: Calculate packfile using revlist and push in same session
	var oldHash plumbing.Hash
	if found {
		oldHash = remoteHash
	} else {
		oldHash = plumbing.ZeroHash // Creating new branch
	}

	// Use revlist.Objects to calculate objects to send
	// Pass localHash as 'ignore' (start) and oldHash as 'limit' (stop)
	var objectsToSend []plumbing.Hash
	if oldHash == plumbing.ZeroHash {
		// Creating new branch - send all reachable objects from localHash
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, nil)
	} else {
		// Updating existing branch - send objects between oldHash and localHash
		// revlist.Objects(storer, commits to traverse, commits to stop at)
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, []plumbing.Hash{oldHash})
	}
	if err != nil {
		return fmt.Errorf("failed to calculate objects using revlist: %w", err)
	}

	logger.Info("Calculated objects to send using revlist", "count", len(objectsToSend), "from", oldHash, "to", localHash)

	// Create packfile
	packfileData, err := createPackfile(repo, objectsToSend)
	if err != nil {
		return fmt.Errorf("failed to create packfile: %w", err)
	}

	// Create reference update request
	req := packp.NewReferenceUpdateRequest()
	req.Capabilities.Set(capability.ReportStatus)
	req.Packfile = packfileData

	cmd := &packp.Command{
		Name: branch,
		Old:  oldHash,
		New:  localHash,
	}
	req.Commands = []*packp.Command{cmd}

	// Send request via session
	logger.Info("Sending packfile via ReceivePack", "objects", len(objectsToSend))
	rs, err := session.ReceivePack(ctx, req)
	if err != nil {
		logger.Error(err, "ReceivePack failed")
		return fmt.Errorf("failed to receive pack: %w", err)
	}

	// Check for errors in response
	if err := rs.Error(); err != nil {
		logger.Error(err, "Push rejected by server")
		return fmt.Errorf("push rejected: %w", err)
	}

	// Check command status
	if len(rs.CommandStatuses) > 0 {
		status := rs.CommandStatuses[0]
		if err := status.Error(); err != nil {
			logger.Error(err, "Command status indicates failure", "ref", status.ReferenceName)
			return fmt.Errorf("push failed for ref %s: %w", status.ReferenceName, err)
		}
		logger.Info("Command status OK", "ref", status.ReferenceName)
	}

	logger.Info("Push successful via single session", "branch", branchName, "from", oldHash, "to", localHash)
	return nil
}

// createPackfile creates a packfile containing the specified objects using go-git's encoder.
func createPackfile(repo *git.Repository, objects []plumbing.Hash) (io.ReadCloser, error) {
	var buf bytes.Buffer

	storer, ok := repo.Storer.(storage.Storer)
	if !ok {
		return nil, fmt.Errorf("repository storer does not implement storage.Storer")
	}

	encoder := packfile.NewEncoder(&buf, storer, false)

	// Encode the list of object hashes
	_, err := encoder.Encode(objects, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to encode packfile: %w", err)
	}

	return io.NopCloser(&buf), nil
}
