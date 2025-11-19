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
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TODO We should move this as well
func CheckWriteAccess(ctx context.Context, path string, repoUrl string, auth transport.AuthMethod) error {
	logger := log.FromContext(ctx)
	repo, err := initializeCleanRepository(path, logger)
	if err != nil {
		return err
	}

	// Ensure the remote origin is set correctly
	if err := ensureRemoteOrigin(ctx, repo, repoUrl); err != nil {
		return fmt.Errorf("failed to ensure remote origin: %w", err)
	}

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
	refs, err := session.AdvertisedReferences() // So in that case it WILL return the head value?! In case of GH it does not... But this is a great idea to validate that we have write access as well!
	if err != nil {
		return fmt.Errorf("failed to get advertised references: %w", err)
	}

	return fmt.Errorf("failed to get advertised references: %s", refs.Head)
}

// PushAtomic performs an atomic PushAtomic operation in a single network session.
// It checks if the remote branch is not touched before pushing to prevent creating diverged branches.
// An explcit error is returned if it failed: I don't plan to use these, we can always retry...
func PushAtomic(
	ctx context.Context,
	repo *git.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName, // we need this to check if the rootbranch is still set the roothash: could be HEAD or the feature branch for now, but perhaps more in the future
	auth transport.AuthMethod,
) error {
	logger := log.FromContext(ctx)
	branch, localHash, err := GetCurrentBranch(repo)
	if err != nil {
		return fmt.Errorf("failed to get current branch: %w", err)
	}

	branchName := branch.Short()

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

	// Determine the "old" hash for the push command and validate state
	var oldHash plumbing.Hash
	remoteHash, found := refs.References[branch.String()]

	if found {
		// Branch exists on remote
		oldHash = remoteHash

		// Check if we are already up2date
		if localHash == remoteHash {
			logger.Info("remote already up2date", "branch", branchName, "hash", localHash)
			return nil
		}

		// Check if the remoteHash is what we based our work on
		if rootHash != remoteHash {
			logger.Info("Remote branch not in expected state", "branch", branchName)
			return fmt.Errorf("remote received unknown updates")
		}
	} else {
		// Branch does NOT exist on remote

		// Is the intent to create an orphaned/root bracnh?
		if rootHash.IsZero() {
			// Use ZeroHash as "old" in Git push protocol to create new root branch
			oldHash = plumbing.ZeroHash
			logger.Info("Remote branch not found: pushing new orphan branch", "branch", branchName)
		} else {
			return fmt.Errorf("remote went missing")
		}
	}

	// Phase 3: Calculate packfile using revlist and push in same session
	// Use revlist.Objects to calculate objects to send
	// Pass localHash as 'ignore' (start) and parentHash as 'limit' (stop)
	var objectsToSend []plumbing.Hash
	if rootHash.IsZero() {
		// Creating new branch - send all reachable objects from localHash
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, nil)
	} else {
		// Updating existing branch - send objects between parentHash and localHash
		// revlist.Objects(storer, commits to traverse, commits to stop at)
		objectsToSend, err = revlist.Objects(repo.Storer, []plumbing.Hash{localHash}, []plumbing.Hash{rootHash})
	}
	if err != nil {
		return fmt.Errorf("failed to calculate objects using revlist: %w", err)
	}

	logger.Info("Calculated objects to send using revlist", "count", len(objectsToSend), "from", rootHash, "to", localHash)

	// Create packfile
	packfileData, err := createPackfile(repo, objectsToSend)
	if err != nil {
		return fmt.Errorf("failed to create packfile: %w", err)
	}

	// Create reference update request
	req := packp.NewReferenceUpdateRequest()
	req.Capabilities.Set(capability.ReportStatus)
	req.Packfile = packfileData

	// Use oldHash (either remoteHash or ZeroHash) as the expected "old" value
	// This tells Git what we expect the current state to be
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

	encoder := packfile.NewEncoder(&buf, repo.Storer, false)

	// Encode the list of object hashes
	_, err := encoder.Encode(objects, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to encode packfile: %w", err)
	}

	return io.NopCloser(&buf), nil
}
