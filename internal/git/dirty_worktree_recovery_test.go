// SPDX-License-Identifier: Apache-2.0

package git

// Recovery from a worktree a failed write left dirty, at every loop path that commits.
//
// The head-of-cycle fetch used to launder the worktree as a side effect (§5). Now that a trusted,
// clean base skips it, the cleanup has to be deliberate — and commitPendingWrites cannot perform
// it once work is retained, because a reset is exactly what would destroy the local commits those
// writes already produced. recoverDirtyWorktree resets and REPLAYS instead, which is why it lives
// on the event loop, and why every loop path that reaches commitPendingWrites has to call it.
//
// See docs/design/inbound-push-notification.md §3.1 and §5.

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

	provider, err := worker.getGitProvider(worker.ctx)
	require.NoError(t, err)
	repoPath := worker.repoPathForRemote(provider.Spec.URL)

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

	provider, err := worker.getGitProvider(worker.ctx)
	require.NoError(t, err)
	repoPath := worker.repoPathForRemote(provider.Spec.URL)

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
// It is written as a table on purpose. recoverDirtyWorktree has to be called by every path that
// reaches commitPendingWrites, and nothing in the type system says so — a fifth entry point added
// later would silently commit leftovers. Adding its row here is the cheapest way to make that
// omission fail loudly.
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
				require.NoError(t, (<-resultCh).Err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker, _, _ := setupCommitPushSplitWorker(t)
			createPlainGitTarget(t, worker, "team-a", "team-a")

			provider, err := worker.getGitProvider(worker.ctx)
			require.NoError(t, err)
			repoPath := worker.repoPathForRemote(provider.Spec.URL)

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
