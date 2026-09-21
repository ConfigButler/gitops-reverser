// SPDX-License-Identifier: Apache-2.0

package git

// The base-trust state machine from docs/design/inbound-push-notification.md §3 and §3.1.
//
// Nothing reads these flags to make a decision yet and the setter is hard-wired off, so none of
// this can change behavior — which is exactly why the tests are written now. The hard part of the
// change is finding every place trust must be dropped, and that is reviewable on its own while
// the suite still passes for the old reasons. When the flip lands, these tests are what say the
// transitions were already right.
//
// The machine is live: commitPendingWrites now skips its fetch on a trusted, clean base. These
// tests pin each transition directly, so a regression names the transition that broke rather than
// showing up as a round-trip count moving in the golden ledger.

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

// TestBaseTrust_SetterRoundTrips is the smallest statement that the machine is live.
func TestBaseTrust_SetterRoundTrips(t *testing.T) {
	w := newMetricsTestWorker()

	w.setBaseTrusted(true)
	assert.True(t, w.baseTrusted())

	w.setBaseTrusted(false)
	assert.False(t, w.baseTrusted())
}

// TestBaseTrust_NewWorkerStartsUntrustedAndClean pins the starting state: nothing has looked at
// the remote, and nothing has written.
func TestBaseTrust_NewWorkerStartsUntrustedAndClean(t *testing.T) {
	w := newMetricsTestWorker()
	assert.False(t, w.baseTrusted(), "nothing has looked at the remote")
	assert.False(t, w.worktreeDirty(), "nothing has written")
}

// TestBaseTrust_GainedOnlyByAResetThatLandedOnTheTargetBranch walks the gain point. A fetch that
// fell back to the default branch, or found the branch unborn, leaves a worktree that is not
// based on the target branch, and §3 requires both to stay untrusted.
func TestBaseTrust_GainedOnlyByAResetThatLandedOnTheTargetBranch(t *testing.T) {
	cases := []struct {
		name   string
		report *PullReport
		want   bool
	}{
		{
			name:   "reset onto the target branch",
			report: &PullReport{ExistsOnRemote: true, HEAD: BranchInfo{Sha: "abc", ShortName: "main"}},
			want:   true,
		},
		{
			name:   "fell back to the remote's default branch",
			report: &PullReport{ExistsOnRemote: false, HEAD: BranchInfo{Sha: "abc", ShortName: "main"}},
			want:   false,
		},
		{
			name:   "branch is unborn",
			report: &PullReport{ExistsOnRemote: true, HEAD: BranchInfo{ShortName: "main", Unborn: true}},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMetricsTestWorker()
			w.updateBranchMetadataFromPullReport(tc.report)
			assert.Equal(t, tc.want, w.baseTrusted())
		})
	}
}

// TestBaseTrust_AResetAlwaysClearsTheDirtyFlag is the other half of the gain point. Every call
// site of updateBranchMetadataFromPullReport is a PrepareBranch or a syncToRemote, and both paths
// inside syncToRemote leave a clean worktree — checkoutAndReset passes Force, makeHeadUnborn
// clears the index and the tree. So the dirty flag clears even where trust is not gained.
func TestBaseTrust_AResetAlwaysClearsTheDirtyFlag(t *testing.T) {
	for _, report := range []*PullReport{
		{ExistsOnRemote: true, HEAD: BranchInfo{Sha: "abc", ShortName: "main"}},
		{ExistsOnRemote: false, HEAD: BranchInfo{ShortName: "main", Unborn: true}},
	} {
		w := newMetricsTestWorker()
		w.markWorktreeDirty("a write failed part-way")
		require.True(t, w.worktreeDirty())

		w.updateBranchMetadataFromPullReport(report)
		assert.False(t, w.worktreeDirty(), "a reset discards whatever the worktree held")
	}
}

// TestBaseTrust_FetchingTheRemoteHashDoesNotGrantTrust is the distinction the whole design rests
// on. fetchRemoteBranchHash fetches without resetting: it learns where the remote is without
// making the worktree match, so it must not call updateBranchMetadataFromPullReport and must not
// grant trust.
func TestBaseTrust_FetchingTheRemoteHashDoesNotGrantTrust(t *testing.T) {
	f := newLedgerFixture(t, "trust-hash-fetch", true)
	f.commit(false, "mine")
	f.contend("OUTSIDE.md", "from-another-writer\n")

	repo, err := gogit.PlainOpen(f.worker.repoPathForRemote(f.sim.RepoURL))
	require.NoError(t, err)

	// Pretend an earlier reset had established trust. Seeding the atomic directly is what makes
	// this test independent of the hard-wiring: what it watches for is the flag being CLEARED by
	// a call that has no business touching it.
	f.worker.baseTrustedState.Store(true)

	_, err = fetchRemoteBranchHash(f.worker.ctx, repo, plumbing.NewBranchReferenceName("main"), nil)
	require.NoError(t, err)

	assert.True(t, f.worker.baseTrusted(),
		"a hash fetch neither grants nor revokes trust: it does not touch the worktree at all")
}

// TestBaseTrust_LostOnEveryPushFailure covers the §3 row that matters most, because a push that
// died mid-upload leaves the remote in a state nobody observed.
func TestBaseTrust_LostOnEveryPushFailure(t *testing.T) {
	f := newLedgerFixture(t, "trust-push-fail", true)
	f.commit(false, "mine")

	originalPush := pushAtomicFn
	pushAtomicFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.Hash,
		_ plumbing.ReferenceName, _ []gitclient.Option,
	) error {
		return errors.New("dial tcp: connection reset by peer")
	}
	defer func() { pushAtomicFn = originalPush }()

	originalFetch := fetchRemoteBranchHashFn
	fetchRemoteBranchHashFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ []gitclient.Option,
	) (plumbing.Hash, error) {
		return f.worker.pushCycleRootHash, nil
	}
	defer func() { fetchRemoteBranchHashFn = originalFetch }()

	f.worker.baseTrustedState.Store(true)
	require.Error(t, f.worker.pushPendingCommits(f.pending))

	assert.False(t, f.worker.baseTrusted(), "a failed push leaves the remote state unknown")
}

// TestBaseTrust_SuccessfulPushDoesNotLaunderADirtyWorktree is the interleaving from §3.1, and the
// reason one flag is not enough. Collapsing the two would make this sequence forget that a write
// failed, and the next cycle would commit its leftovers under an unrelated author.
func TestBaseTrust_SuccessfulPushDoesNotLaunderADirtyWorktree(t *testing.T) {
	f := newLedgerFixture(t, "trust-dirty-survives-push", true)

	// 1. Write A commits and is retained.
	f.commit(false, "write-a")

	// 2. Write B fails part-way through executePendingWrites. Its window is dropped; A stays.
	f.worker.markWorktreeDirty("write B failed part-way")

	// 3. The push timer fires and A pushes successfully.
	f.push()

	assert.True(t, f.worker.baseTrusted(),
		"the pushed commits are the remote tip, so the base is at the remote tip")
	assert.True(t, f.worktreeStillDirty(),
		"a push says where the remote is and says nothing about B's leftovers; "+
			"only a reset may clear this")
}

// TestBaseTrust_LostWhenSomebodySaysTheRemoteMoved covers the forced-recheck entry points. Both
// re-establish trust through the sync that follows, so the observable effect is that the flag
// ends up reflecting the fetch rather than what it was before.
func TestBaseTrust_LostWhenSomebodySaysTheRemoteMoved(t *testing.T) {
	f := newLedgerFixture(t, "trust-forced-recheck", true)
	f.publish("prime")

	syncFailed := errors.New("sync failed")
	original := syncToRemoteFn
	syncToRemoteFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ []gitclient.Option,
	) (*PullReport, error) {
		return nil, syncFailed
	}
	defer func() { syncToRemoteFn = original }()

	f.commit(false, "retained")

	f.worker.baseTrustedState.Store(true)
	err := f.worker.refreshRemoteAndRebuildPendingWrites(f.worker.ctx, f.pending, fetchReasonForcedRecheck)
	require.ErrorIs(t, err, syncFailed)

	assert.False(t, f.worker.baseTrusted(),
		"somebody said the remote moved and the fetch that would have proved otherwise failed")
}

// TestBaseTrust_LostWhenAWriteFails ties the two flags together at the one event that sets both.
func TestBaseTrust_LostWhenAWriteFails(t *testing.T) {
	w := newMetricsTestWorker()
	w.baseTrustedState.Store(true)

	w.invalidateBase("execute pending writes failed")
	w.markWorktreeDirty("execute pending writes failed")

	assert.False(t, w.baseTrusted())
	assert.True(t, w.worktreeDirty())
}

// worktreeStillDirty reads the flag through the fixture, so the §3.1 assertion reads as the
// English claim it is making.
func (f *ledgerFixture) worktreeStillDirty() bool { return f.worker.worktreeDirty() }
