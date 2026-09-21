// SPDX-License-Identifier: Apache-2.0

package git

// What happens when somebody deletes the branch the worker writes to.
//
// A deleted branch IS the remote moving, but the push reported it as an untyped error, so the
// worker took the fallback probe instead of the replay. That probe reads
// refs/remotes/origin/<branch>, and SmartFetch cannot prune a ref it builds no refspec for, so it
// answered with the hash the branch held before the deletion. That matched the cycle's root, the
// worker concluded the remote had not moved, and no replay followed — after which the retained
// writes made every later cycle skip its head-of-cycle fetch too. The worker pushed the same
// doomed commits until it was restarted.
//
// The unconditional prefetch used to hide this: it re-rooted the cycle on the default branch
// before the push, so the push simply re-created the branch. These tests are what keep that
// recovery once the prefetch is conditional.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.3 and §1.5.

import (
	"context"
	"os/exec"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitInRepo runs one git command against the bare repository behind the test server, so a branch
// can be created or deleted out of band the way an operator would from the Git host's UI.
func gitInRepo(t *testing.T, repoDir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// giveRemoteADefaultBranchBesidesMain adds a second branch and points the remote's HEAD at it, so
// deleting main still leaves SmartFetch something to fall back to. That is the realistic shape:
// the repository keeps working, only the branch this GitTarget writes to is gone.
func giveRemoteADefaultBranchBesidesMain(t *testing.T, repoDir string) {
	t.Helper()
	gitInRepo(t, repoDir, "branch", "trunk", "main")
	gitInRepo(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
}

// TestPush_DeletedRemoteBranchIsTreatedAsMovement pins the classification. The advertisement not
// carrying the cycle's root branch has to reach the worker as a RemoteMovedError, because that is
// the only failure shape the replay path acts on.
func TestPush_DeletedRemoteBranchIsTreatedAsMovement(t *testing.T) {
	f := newLedgerFixture(t, "branch-deleted-typed", true)
	f.createLedgerTarget("team-a", nil)
	giveRemoteADefaultBranchBesidesMain(t, f.repoDir)
	f.publish("prime")

	f.commit(false, "after-the-delete")
	gitInRepo(t, f.repoDir, "update-ref", "-d", "refs/heads/main")

	// Push once, without the replay, to read the error the remote produces.
	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	repo, err := openWorkerRepo(f.worker, provider.Spec.URL)
	require.NoError(t, err)
	err = PushAtomic(f.worker.ctx, repo, f.worker.pushCycleRootHash,
		plumbing.NewBranchReferenceName("main"), nil)

	var moved *RemoteMovedError
	require.ErrorAs(t, err, &moved, "a branch that is gone is the remote moving, not an opaque failure")
	assert.True(t, moved.Missing, "and it must say the branch was absent rather than at another hash")
	assert.Equal(t, plumbing.ZeroHash, moved.Advertised,
		"zero is what the remote is advertising for a branch it does not have")
	require.False(t, moved.Expected.IsZero(), "the cycle was rooted on a real commit")
	assert.NotEqual(t, moved.Expected, moved.Advertised,
		"expected and advertised must differ: that inequality is what makes "+
			"remoteMovedDuringPush answer yes and the replay run")
}

// TestFetchRemoteBranchHash_ReportsZeroWhenTheBranchIsGone is the second half, and it matters on
// its own: any push that fails without an advertisement takes this probe, so it must not answer
// from a remote-tracking ref that nothing could refresh.
func TestFetchRemoteBranchHash_ReportsZeroWhenTheBranchIsGone(t *testing.T) {
	f := newLedgerFixture(t, "branch-deleted-probe", true)
	f.createLedgerTarget("team-a", nil)
	giveRemoteADefaultBranchBesidesMain(t, f.repoDir)
	f.publish("prime")

	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	repo, err := openWorkerRepo(f.worker, provider.Spec.URL)
	require.NoError(t, err)

	branch := plumbing.NewBranchReferenceName("main")
	before, err := fetchRemoteBranchHash(context.Background(), repo, branch, nil)
	require.NoError(t, err)
	require.False(t, before.IsZero(), "the branch is there, so the probe reports where it is")

	gitInRepo(t, f.repoDir, "update-ref", "-d", "refs/heads/main")

	after, err := fetchRemoteBranchHash(context.Background(), repo, branch, nil)
	require.NoError(t, err)
	assert.Equal(t, plumbing.ZeroHash, after,
		"the stale remote-tracking ref survives a prune that has no refspec for it; "+
			"reporting its hash would claim a deleted branch is still where it was")
}

// TestPush_DeletedRemoteBranchDoesNotStrandRetainedWrites is the operator-visible claim: deleting
// the branch costs one replay, not a worker that pushes the same doomed commits forever.
//
// The fetch before the deletion is what makes this faithful, and it took a while to see. A push
// does NOT advance refs/remotes/origin/<branch>, so straight after a publication that ref is
// already behind the cycle's root hash — and the stale answer the probe gives then happens to
// differ from the root, so the worker replays for the wrong reason and recovers by luck. Only
// when the last thing to touch that ref was a FETCH does the stale hash EQUAL the root, which is
// when the probe reports "unchanged" and the cycle stalls with nothing left to correct it.
//
// Either fix alone is enough here — the typed error and the honest probe both make the replay
// run — which is deliberate: this test states the outcome an operator can see, and the two tests
// above pin the mechanisms separately.
func TestPush_DeletedRemoteBranchDoesNotStrandRetainedWrites(t *testing.T) {
	f := newLedgerFixture(t, "branch-deleted-recovers", true)
	f.createLedgerTarget("team-a", nil)
	giveRemoteADefaultBranchBesidesMain(t, f.repoDir)
	f.publish("prime")
	require.True(t, f.worker.baseTrusted(), "a successful push leaves the base trusted")

	// Drop trust so the next cycle fetches, which leaves refs/remotes/origin/main exactly at the
	// remote tip the cycle then roots on. That equality is the stranding condition.
	f.worker.invalidateBase("test: force the cycle to fetch first")
	f.commit(false, "written-after-the-fetch")
	f.push()
	f.worker.invalidateBase("test: force the cycle to fetch first")
	f.commit(false, "written-after-the-delete")

	gitInRepo(t, f.repoDir, "update-ref", "-d", "refs/heads/main")

	// The push is now the only thing that can notice the branch is gone.
	f.push()

	// The branch is back, at a commit that carries the write.
	out, err := exec.Command("git", "-C", f.repoDir, "rev-parse", "refs/heads/main").CombinedOutput()
	require.NoError(t, err, "the push must have re-created the branch: %s", out)

	names, err := exec.Command("git", "-C", f.repoDir, "ls-tree", "-r", "--name-only", "refs/heads/main").
		CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(names), "written-after-the-delete",
		"the replay re-plans the retained write onto the new base rather than losing it")
}

// openWorkerRepo opens the worker's on-disk checkout, which several of these tests reach into to
// drive one push or one probe directly.
func openWorkerRepo(w *BranchWorker, url string) (*gogit.Repository, error) {
	return gogit.PlainOpen(w.repoPathForRemote(url))
}
