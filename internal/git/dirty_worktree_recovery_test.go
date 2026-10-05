// SPDX-License-Identifier: Apache-2.0

package git

// Recovery from a worktree a failed write left dirty, at every loop path that commits.
//
// The head-of-cycle fetch used to launder the worktree as a side effect (§5). Now that a trusted,
// clean base skips it, the cleanup has to be deliberate — and commitPendingWrites cannot perform
// it once work is retained, because a reset is exactly what would destroy the local commits those
// writes already produced. The loop's materialize resets and REPLAYS instead, and its commit
// refuses a checkout that is not the projection of the retained writes, so a path that skipped
// materialize fails rather than committing leftovers.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.5.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// leftoverPath is the file a half-finished write staged before it died.
//
// It sits OUTSIDE the folder the committing target owns, which is the case that actually reaches
// Git: a batch can carry writes for several targets, and the acceptance gate only scans the folder
// of the target it is planning for. A leftover inside team-a/ would be refused as a foreign file,
// so the write would be dropped rather than polluted — a different, louder failure.
const leftoverPath = "team-b/leftover-from-a-failed-write.yaml"

// stageLeftoverFromAFailedWrite reproduces what executePendingWrites leaves behind when it fails
// part-way through a batch: a file on disk, added to the index, and the dirty flag set.
//
// It stages a REAL file rather than only setting the flag. The flag alone would pass a recovery
// that resets nothing, which is the failure this test exists to catch: the assertion that matters
// is whether the leftover reaches a commit, and only a real index entry can answer that.
func stageLeftoverFromAFailedWrite(t *testing.T, worker *BranchWorker) {
	t.Helper()

	_, err := worker.getGitProvider(worker.ctx)
	require.NoError(t, err)
	repoPath := worker.repoPath()

	repo, err := gogit.PlainOpen(repoPath)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	full := filepath.Join(repoPath, filepath.FromSlash(leftoverPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o600))
	_, err = worktree.Add(leftoverPath)
	require.NoError(t, err)

	worker.markWorktreeDirty("test: a write failed part-way through its batch")
}

// headTreeContains reports whether the checkout's current HEAD commit carries the given path.
func headTreeContains(t *testing.T, repoPath, path string) bool {
	t.Helper()

	repo, err := gogit.PlainOpen(repoPath)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	_, err = commit.File(path)
	if errors.Is(err, object.ErrFileNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

// TestAtomicWrite_DoesNotCommitAFailedWritesLeftovers is the §3.1 interleaving driven through the
// ATOMIC path, which had no recovery at all.
//
// handleAtomicRequest finalizes the open window first, and that finalize is where recovery used to
// be reached — but finalizeOpenWindowWithReason returns at its first line when no window is open,
// which is the ordinary case for an atomic write. So the atomic planned on a worktree still
// holding the failed write's staged file and committed it under its own author.
func TestAtomicWrite_DoesNotCommitAFailedWritesLeftovers(t *testing.T) {
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")

	_, err := worker.getGitProvider(worker.ctx)
	require.NoError(t, err)
	repoPath := worker.repoPath()

	// The cooldown is held, so write A commits locally and stays retained. That retention is what
	// makes commitPendingWrites' own reset unavailable.
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	// 1. Write A commits and is retained.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("write-a", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1)

	// 2. Write B fails part-way through its batch, leaving its file staged.
	stageLeftoverFromAFailedWrite(t, worker)

	// 3. An atomic write C arrives with no window open.
	require.Nil(t, loop.openWindow)
	loop.handleAtomicRequest(&WriteRequest{
		Events:             []Event{configMapEvent("write-c", "carol", "")},
		CommitMode:         CommitModeAtomic,
		GitTargetName:      "team-a",
		GitTargetNamespace: "default",
	})
	loop.endWake(0)

	require.Len(t, loop.pendingWrites, 2, "C committed: A is retained alongside it")
	assert.False(t, headTreeContains(t, repoPath, leftoverPath),
		"C's commit must not carry the file a failed write left staged")
	assert.True(t, headTreeContains(t, repoPath, "team-a/default/configmaps/write-a.yaml"),
		"the replay must rebuild A: a recovery resets, and re-plans rather than discarding work")
	assert.False(t, worker.worktreeDirty(), "the reset inside the recovery clears the flag")
}

// TestEveryLoopCommitPathRecoversADirtyWorktree enumerates the loop's commit entry points against
// one dirty fixture.
//
// It is written as a table on purpose. The commit guard turns a path that skips materialize into a
// failed write rather than a laundered one, but only this table proves each path RECOVERS: a new
// entry point that forgot to materialize would fail every write while the worktree is dirty.
func TestEveryLoopCommitPathRecoversADirtyWorktree(t *testing.T) {
	cases := []struct {
		name   string
		commit func(t *testing.T, loop *branchWorkerEventLoop)
	}{
		{
			name: "live commit window",
			commit: func(t *testing.T, loop *branchWorkerEventLoop) {
				t.Helper()
				loop.handleQueueItem(WorkItem{Request: &WriteRequest{
					Events:     []Event{configMapTargetEvent("from-window", "bob", "team-a")},
					CommitMode: CommitModePerEvent,
				}})
				require.True(t, loop.finalizeOpenWindow())
			},
		},
		{
			name: "atomic request",
			commit: func(t *testing.T, loop *branchWorkerEventLoop) {
				t.Helper()
				loop.handleAtomicRequest(&WriteRequest{
					Events:             []Event{configMapEvent("from-atomic", "carol", "")},
					CommitMode:         CommitModeAtomic,
					GitTargetName:      "team-a",
					GitTargetNamespace: "default",
				})
				loop.endWake(0)
			},
		},
		{
			name: "resync",
			commit: func(t *testing.T, loop *branchWorkerEventLoop) {
				t.Helper()
				scope := ResyncScopeFor(
					schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
				resultCh := make(chan ResyncResult, 1)
				loop.handleResyncRequest(&ResyncRequest{
					GitTargetName:      "team-a",
					GitTargetNamespace: "default",
					Scope:              &scope,
					Result:             resultCh,
				})
				loop.endWake(0)
				require.NoError(t, (<-resultCh).Err)
			},
		},
		{
			name: "empty commit recording a CommitRequest",
			commit: func(t *testing.T, loop *branchWorkerEventLoop) {
				t.Helper()
				serviceAttach(loop, commitEmptyReq("alice", "save: nothing changed"))
				forceDue(loop)
				loop.endWake(0)
				require.Len(t, loop.pendingWrites, 2, "the record was made")
			},
		},
		{
			name: "empty commit for a refused write",
			commit: func(t *testing.T, loop *branchWorkerEventLoop) {
				t.Helper()
				target := itypes.NewResourceReference("team-a", "default")
				loop.commitRefusalTouch(refusalKeyFor(target, "configmaps"), "refused", "observation")
				loop.endWake(0)
				require.Len(t, loop.pendingWrites, 2, "the empty commit was made")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker, _, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")

			_, err := worker.getGitProvider(worker.ctx)
			require.NoError(t, err)
			repoPath := worker.repoPath()

			loop := newBranchWorkerEventLoop(worker, time.Hour)
			loop.lastPushAt = time.Now()
			defer loop.stopTimers()

			// Retained work, so commitPendingWrites cannot reset for us.
			loop.handleQueueItem(WorkItem{Request: &WriteRequest{
				Events:     []Event{configMapTargetEvent("retained", "alice", "team-a")},
				CommitMode: CommitModePerEvent,
			}})
			require.True(t, loop.finalizeOpenWindow())
			require.Len(t, loop.pendingWrites, 1)

			stageLeftoverFromAFailedWrite(t, worker)
			tc.commit(t, loop)

			assert.False(t, headTreeContains(t, repoPath, leftoverPath),
				"this path committed a failed write's leftovers: it must recover first")
			assert.False(t, worker.worktreeDirty(),
				"a recovery resets, and a reset clears the flag")
		})
	}
}

// TestDirtyWorktree_AFailedWriteIsUndoneWithoutAFetch pins the local undo. A batch whose second
// write fails after the first has committed and staged its file used to leave the worktree dirty,
// and the next commit then fetched and reset from the remote to clear it. The checkout is the
// projection of the retained writes, so the commit the batch started on is all the cleanup needs:
// the failed batch leaves no commit, no file and no doubt behind, and the next commit costs no
// round trip.
func TestDirtyWorktree_AFailedWriteIsUndoneWithoutAFetch(t *testing.T) {
	f := newLedgerFixture(t, "failed-write-undone-locally", true)
	f.publish("prime")

	repo, err := gogit.PlainOpen(f.worker.repoPath())
	require.NoError(t, err)
	start := localHead(repo)

	committed, err := f.worker.buildGroupedPendingWrite(f.worker.ctx,
		[]Event{configMapEvent("committed-then-undone", "alice", "team-a")})
	require.NoError(t, err)
	secret := configMapEvent("unencrypted", "alice", "team-a")
	secret.Identifier.Resource = "secrets"
	secret.Object.SetKind("Secret")
	failing, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, []Event{secret})
	require.NoError(t, err)

	err = f.worker.commitPendingWrites([]PendingWrite{*committed, *failing})
	require.ErrorContains(t, err, "secret encryption is required")

	assert.False(t, f.worker.worktreeDirty(), "the failed batch was undone locally")
	assert.True(t, f.worker.baseTrusted(), "the checkout is back where the trusted base left it")
	assert.Equal(t, start, localHead(repo), "the first write's commit is undone with the batch")
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	status, err := worktree.Status()
	require.NoError(t, err)
	assert.True(t, status.IsClean(), "no leftover of the failed batch survives: %s", status)

	before := f.mark()
	f.commit("after-the-failure")
	assert.Zero(t, f.mark().since(before).connections(), "the next commit plans on the checkout as it is")
}
