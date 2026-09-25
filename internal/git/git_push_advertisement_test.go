// SPDX-License-Identifier: Apache-2.0

package git

// A rejected compare-and-swap is the remote telling us where the branch is. These tests pin that
// the worker now listens, and that it still falls back to the network when the remote said
// nothing.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.3.

import (
	"context"
	"errors"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPushAtomic_RejectionCarriesAdvertisedHash is the load-bearing one: everything else here is
// only worth having if the rejection really does arrive with the remote's own number in it.
//
// It runs over a real git server, not file://, because go-git v6's in-process receive-pack never
// compares cmd.Old — the rejection this asserts on cannot be provoked over that transport.
func TestPushAtomic_RejectionCarriesAdvertisedHash(t *testing.T) {
	f := newLedgerFixture(t, "adv-rejection", true)

	f.commit(false, "mine")
	f.contend("OUTSIDE.md", "from-another-writer\n")
	contendingHash := plumbing.NewHash(revParseMain(t, f.repoDir))

	repo, err := gogit.PlainOpen(f.worker.repoPath())
	require.NoError(t, err)

	_, err = PushAtomic(f.worker.ctx, repo, f.worker.pushCycleRootHash,
		plumbing.NewBranchReferenceName("main"), nil)

	var moved *RemoteMovedError
	require.ErrorAs(t, err, &moved, "a moved remote must be reported as RemoteMovedError")
	assert.Equal(t, contendingHash, moved.Advertised,
		"the rejection must carry the hash the push session's own advertisement named")
	assert.Equal(t, moved.Expected, f.worker.pushCycleRootHash)
	assert.Equal(t, plumbing.NewBranchReferenceName("main"), moved.Branch)
	assert.Equal(t, "remote received unknown updates", err.Error(),
		"the message operators read in the logs is unchanged")
}

// TestRunPushCycle_MovedRemote_DoesNotRefetchTheHash is the deletion itself: a rejection that
// carried an advertisement must not spend a SmartFetch learning the same number again.
func TestRunPushCycle_MovedRemote_DoesNotRefetchTheHash(t *testing.T) {
	f := newLedgerFixture(t, "adv-no-refetch", true)
	f.commit(false, "mine")
	f.contend("OUTSIDE.md", "from-another-writer\n")

	fetchCalls := 0
	restore := countFetchRemoteBranchHash(t, &fetchCalls)
	defer restore()

	require.NoError(t, f.worker.pushPendingCommits(f.pending))

	assert.Zero(t, fetchCalls,
		"the rejection already carried the remote's hash; fetching it again is two requests for nothing")

	// The replay still has to land, or the saving would be bought with a dropped write.
	remoteHash := revParseMain(t, f.repoDir)
	assert.NotEmpty(t, remoteHash)
	local, err := gogit.PlainOpen(f.worker.repoPath())
	require.NoError(t, err)
	localRef, err := local.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, localRef.Hash().String(), remoteHash, "the replayed commits must have reached the remote")
}

// TestRunPushCycle_PushWithoutAdvertisement_FallsBackToFetch keeps the slow path honest. A push
// that dies before the remote speaks — a dropped connection, an auth failure — has no hash to
// reuse, and dropping the fetch for it would leave the worker unable to tell contention from a
// transient fault.
func TestRunPushCycle_PushWithoutAdvertisement_FallsBackToFetch(t *testing.T) {
	f := newLedgerFixture(t, "adv-fallback", true)
	f.commit(false, "mine")

	// A connection that died: no advertisement, so no typed error.
	originalPush := pushAtomicFn
	pushes := 0
	pushAtomicFn = func(
		ctx context.Context,
		repo *gogit.Repository,
		rootHash plumbing.Hash,
		rootBranch plumbing.ReferenceName,
		auth []gitclient.Option,
	) (PushOutcome, error) {
		pushes++
		if pushes == 1 {
			return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
		}
		return originalPush(ctx, repo, rootHash, rootBranch, auth)
	}
	defer func() { pushAtomicFn = originalPush }()

	// The remote really did move while that connection was failing, so the fetch is the only way
	// to find out and the replay must follow.
	f.contend("OUTSIDE.md", "from-another-writer\n")

	fetchCalls := 0
	restoreFetch := countFetchRemoteBranchHash(t, &fetchCalls)
	defer restoreFetch()

	require.NoError(t, f.worker.pushPendingCommits(f.pending))

	assert.Equal(t, 1, fetchCalls,
		"a push that produced no advertisement must still learn the remote's hash from the network")
	assert.Equal(t, 2, pushes, "the cycle must have replayed and pushed again")
}

// TestAdvertisedRootHash_RejectsAHashForAnotherBranch guards the branch check. Across a retry the
// cycle's root branch can change — a new branch is rooted on the default branch until the first
// push creates it — and a hash read for a different ref is a wrong answer, not a missing one.
func TestAdvertisedRootHash_RejectsAHashForAnotherBranch(t *testing.T) {
	advertised := plumbing.NewHash("1111111111111111111111111111111111111111")
	moved := &RemoteMovedError{
		Branch:     plumbing.NewBranchReferenceName("main"),
		Expected:   plumbing.NewHash("2222222222222222222222222222222222222222"),
		Advertised: advertised,
	}

	got, ok := advertisedRootHash(moved, plumbing.NewBranchReferenceName("main"))
	require.True(t, ok)
	assert.Equal(t, advertised, got)

	_, ok = advertisedRootHash(moved, plumbing.NewBranchReferenceName("feature"))
	assert.False(t, ok, "an advertisement read for another branch must not answer for this one")

	_, ok = advertisedRootHash(errors.New("connection reset"), plumbing.NewBranchReferenceName("main"))
	assert.False(t, ok, "an untyped push failure carries no advertisement")

	// Wrapped is still found: runPushCycle sees whatever the call chain wrapped it in.
	wrapped := errors.Join(errors.New("push failed"), moved)
	_, ok = advertisedRootHash(wrapped, plumbing.NewBranchReferenceName("main"))
	assert.True(t, ok, "the type must survive wrapping")
}

// countFetchRemoteBranchHash counts calls to the slow path while leaving its behavior intact.
func countFetchRemoteBranchHash(t *testing.T, calls *int) func() {
	t.Helper()
	original := fetchRemoteBranchHashFn
	fetchRemoteBranchHashFn = func(
		ctx context.Context,
		repo *gogit.Repository,
		branch plumbing.ReferenceName,
		auth []gitclient.Option,
	) (plumbing.Hash, error) {
		*calls++
		return original(ctx, repo, branch, auth)
	}
	return func() { fetchRemoteBranchHashFn = original }
}
