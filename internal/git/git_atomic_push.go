// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// getPushSession opens a single receive-pack session for pushing.
//
// go-git v6 replaced v5's transport.NewEndpoint + client.NewClient + NewReceivePackSession with
// transport.ParseURL + client.New(opts...).Handshake. The property PushAtomic depends on is
// unchanged and now explicit in the interface: one Session serves both GetRemoteRefs (the
// advertisement) and Push, so the remote state we validate against is read on the same connection
// we then write to.
func getPushSession(
	ctx context.Context,
	repo *git.Repository,
	auth []gitclient.Option,
) (transport.Session, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return nil, fmt.Errorf("failed to get remote: %w", err)
	}

	// go-git's own config validation rejects a remote with no URL, but a hand-edited or truncated
	// .git/config can still present one, and indexing it would panic rather than fail.
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return nil, errors.New("remote origin has no URL configured")
	}

	endpoint, err := transport.ParseURL(urls[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote URL: %w", err)
	}

	session, err := gitclient.New(auth...).Handshake(ctx, &transport.Request{
		URL:     endpoint,
		Command: transport.ReceivePackService,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create receive-pack session: %w", err)
	}

	return session, nil
}

// advertisedHashes indexes a v6 advertisement by reference name. v5 handed back a
// map[string]plumbing.Hash directly; v6's transport.RemoteRefs carries a []*plumbing.Reference so
// that fields can be added without breaking the interface, so the lookup is built here.
func advertisedHashes(refs *transport.RemoteRefs) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(refs.References))
	for _, ref := range refs.References {
		if ref.Type() == plumbing.HashReference {
			out[ref.Name()] = ref.Hash()
		}
	}
	return out
}

// validatePushState checks if the push can proceed based on remote state.
func validatePushState(
	ctx context.Context,
	session transport.Session,
	repo *git.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName,
) (plumbing.Hash, plumbing.Hash, error) {
	logger := log.FromContext(ctx)

	branch, localHash, err := GetCurrentBranch(repo)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get current branch: %w", err)
	}

	branchName := branch.Short()

	// Phase 1: Get advertised references (remote state) on this same session.
	remoteRefs, err := session.GetRemoteRefs(ctx, nil)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get advertised references: %w", err)
	}
	refs := advertisedHashes(remoteRefs)

	// Determine the "old" hash for the push command and validate state
	var oldHash = plumbing.ZeroHash
	remoteHash, found := refs[branch]
	currentRootHash, rootFound := refs[rootBranch]
	if !rootFound && !rootHash.IsZero() {
		return plumbing.ZeroHash, plumbing.ZeroHash, errors.New("remote went missing")
	}

	if found {
		// Target branch exists on remote
		oldHash = remoteHash

		// Check if we are already up2date
		if localHash == remoteHash {
			logger.Info("remote already up2date", "branch", branchName, "hash", localHash)
			return plumbing.ZeroHash, plumbing.ZeroHash, nil // special signal for up-to-date
		}

		// Check if the remoteHash is what we based our work on
		if rootHash != currentRootHash {
			logger.Info("Remote branch not in expected state", "branch", branchName)
			return plumbing.ZeroHash, plumbing.ZeroHash, errors.New("remote received unknown updates")
		}
	}

	return oldHash, localHash, nil
}

// performPush executes the packfile creation and push operation.
func performPush(
	ctx context.Context,
	session transport.Session,
	repo *git.Repository,
	rootHash, localHash, oldHash plumbing.Hash,
	branch plumbing.ReferenceName,
	logger logr.Logger,
) error {
	// Phase 3: Calculate packfile using revlist and push in same session
	// Use revlist.Objects to calculate objects to send
	// Pass localHash as 'ignore' (start) and parentHash as 'limit' (stop)
	var objectsToSend []plumbing.Hash
	var err error
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

	logger.Info(
		"Calculated objects to send using revlist",
		"count",
		len(objectsToSend),
		"from",
		rootHash,
		"to",
		localHash,
	)

	// Create packfile
	packfileData, err := createPackfile(repo, objectsToSend)
	if err != nil {
		return fmt.Errorf("failed to create packfile: %w", err)
	}

	// Build the push request. The compare-and-swap that makes this push atomic is unchanged from
	// v5: packp.Command carries the Old hash we expect the ref to be at, and the server refuses the
	// update if it has moved. v6 takes the same *packp.Command type, and negotiates report-status
	// itself in buildUpdateRequests, so the capability no longer has to be set by hand.
	//
	// Atomic asks for the receive-pack `atomic` capability when the server offers it. We send a
	// single command, so it changes nothing today; it is set because the guarantee this function
	// promises is exactly what the capability names, and it becomes load-bearing the moment a
	// second command is added.
	req := &transport.PushRequest{
		Packfile: packfileData,
		Commands: []*packp.Command{{
			Name: branch,
			Old:  oldHash,
			New:  localHash,
		}},
		Atomic: true,
	}

	// Push on the same session the advertisement was read from.
	logger.Info("Sending packfile via receive-pack", "objects", len(objectsToSend))
	if err := session.Push(ctx, repo.Storer, req); err != nil {
		// v6's SendPack decodes report-status and returns the per-command rejection as this error,
		// so a refused compare-and-swap arrives here rather than in a separate status struct.
		logger.Error(err, "push rejected or failed", "ref", branch)
		return fmt.Errorf("push failed for ref %s: %w", branch, err)
	}

	logger.Info("Push successful via single session", "branch", branch.Short(), "from", oldHash, "to", localHash)
	return nil
}

// PushAtomic performs an atomic PushAtomic operation in a single network session.
// It checks if the remote branch is not touched before pushing to prevent creating diverged branches.
// An explcit error is returned if it failed: I don't plan to use these, we can always retry...
func PushAtomic(
	ctx context.Context,
	repo *git.Repository,
	rootHash plumbing.Hash,
	rootBranch plumbing.ReferenceName, // only pushes if this branch is in exact same state, e.g. refs/heads/main (HEAD not allowed since a ReceivePackSession never returns it)
	auth []gitclient.Option,
) error {
	if !rootBranch.IsBranch() {
		return errors.New("rootBranch is not a branch")
	}

	logger := log.FromContext(ctx)

	session, err := getPushSession(ctx, repo, auth)
	if err != nil {
		return err
	}
	defer session.Close()

	oldHash, localHash, err := validatePushState(ctx, session, repo, rootHash, rootBranch)
	if err != nil {
		return err
	}
	if oldHash.IsZero() && localHash.IsZero() {
		// Special signal for up-to-date
		return nil
	}

	branch, _, err := GetCurrentBranch(repo)
	if err != nil {
		return fmt.Errorf("failed to get current branch: %w", err)
	}

	return performPush(ctx, session, repo, rootHash, localHash, oldHash, branch, logger)
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
