// SPDX-License-Identifier: Apache-2.0

package git

// The base-trust state machine clears worktreeDirty when a reset lands, on the stated grounds that
// "checkoutAndReset passes Force, which discards dirty files". These tests hold that claim to
// account against a real checkout, because the whole invariant rests on it: if a reset can leave
// a half-written file behind, then clearing the flag is a lie and the next cycle plans on top of
// somebody else's leftovers.
//
// file:// is deliberate here. These assert what a RESET does to the local worktree, which never
// reaches the server, so the compare-and-swap caveat that forces other tests onto
// startRealGitServer does not apply.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.5.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedResetFixture builds a bare remote with one commit and returns a local clone of it.
func seedResetFixture(t *testing.T) string {
	t.Helper()

	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seedRepo, seedWorktree := initLocalRepo(t, seedPath, remoteURL, "main")
	commitFileChange(t, seedWorktree, seedPath, "README.md", "seed\n")
	require.NoError(t, seedRepo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))

	repoPath := filepath.Join(tempDir, "work")
	_, err := PrepareBranch(context.Background(), remoteURL, repoPath, "main", nil)
	require.NoError(t, err)
	return repoPath
}

// TestReset_DiscardsEveryKindOfLeftover is the claim §5 makes, tested against each way a failed
// write can leave the worktree.
//
// A half-finished executePendingWrites can leave any of these: a modified tracked file, a brand
// new file it staged before dying, a brand new file it had not staged yet, and a whole directory
// it created for a new placement.
func TestReset_DiscardsEveryKindOfLeftover(t *testing.T) {
	repoPath := seedResetFixture(t)

	repo, err := gogit.PlainOpen(repoPath)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// A tracked file, modified.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("clobbered\n"), 0o600))

	// A new file, staged — the write got as far as Add before it died.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "staged-new.yaml"), []byte("kind: Half\n"), 0o600))
	_, err = worktree.Add("staged-new.yaml")
	require.NoError(t, err)

	// A new file, not staged.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "unstaged-new.yaml"), []byte("kind: Half\n"), 0o600))

	// A whole directory the write created for a new placement.
	require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "team-a", "nested"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, "team-a", "nested", "cm.yaml"), []byte("kind: ConfigMap\n"), 0o600))

	// The reset the state machine trusts.
	_, err = syncToRemote(context.Background(), repo, plumbing.NewBranchReferenceName("main"), nil)
	require.NoError(t, err)

	status, err := worktree.Status()
	require.NoError(t, err)
	assert.True(t, status.IsClean(),
		"a reset must leave the worktree clean, or clearing worktreeDirty is a lie; status was:\n%s", status)

	assert.FileExists(t, filepath.Join(repoPath, "README.md"))
	content, err := os.ReadFile(filepath.Join(repoPath, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "seed\n", string(content), "the tracked file must be back to the committed content")

	assert.NoFileExists(t, filepath.Join(repoPath, "staged-new.yaml"), "a staged new file must not survive")
	assert.NoFileExists(t, filepath.Join(repoPath, "unstaged-new.yaml"), "an unstaged new file must not survive")
	assert.NoDirExists(t, filepath.Join(repoPath, "team-a"), "a directory the write created must not survive")
}

// TestWriteAndStageFile_RemovesTheDirectoriesItCreatedWhenTheWriteFails covers the one leftover a
// reset provably cannot reach.
//
// Every case above produces a worktree status entry, which is how discardWorktreeLeftovers finds
// it. A write that dies between MkdirAll and WriteFile produces none: Git does not track empty
// directories, so the folder is invisible to status, survives the reset, and sits in the worktree
// while the state machine reports it clean. scanWorktreeSubtree walks the real filesystem, so a
// directory Git does not have is a difference somebody eventually trips over.
//
// The cleanup therefore belongs at the failure, where the set of directories this write created is
// still known, rather than being carried across the reset boundary.
//
// The write is made to fail with a name longer than NAME_MAX, which needs no permissions games and
// happens after the directories exist — which is the whole point.
func TestWriteAndStageFile_RemovesTheDirectoriesItCreatedWhenTheWriteFails(t *testing.T) {
	repoPath := seedResetFixture(t)

	repo, err := gogit.PlainOpen(repoPath)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	newDir := filepath.Join(repoPath, "team-a", "nested")
	tooLong := strings.Repeat("x", 300) + ".yaml"

	err = writeAndStageFile(worktree,
		"team-a/nested/"+tooLong, filepath.Join(newDir, tooLong), []byte("kind: ConfigMap\n"))
	require.Error(t, err, "a name past NAME_MAX cannot be written")

	assert.NoDirExists(t, newDir, "the directory this write created must not outlive it")
	assert.NoDirExists(t, filepath.Join(repoPath, "team-a"),
		"nor the parent it created on the way there")

	status, err := worktree.Status()
	require.NoError(t, err)
	assert.True(t, status.IsClean(),
		"an empty directory produces no status entry, which is exactly why a later reset "+
			"cannot clean it up; status was:\n%s", status)
}

// TestMkdirAllTrackingCreated_ReportsOnlyWhatItMade pins the half that decides what cleanup is
// allowed to touch. A directory somebody else put there is not this write's to remove.
func TestMkdirAllTrackingCreated_ReportsOnlyWhatItMade(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "existing"), 0o750))

	created, err := mkdirAllTrackingCreated(filepath.Join(root, "existing", "a", "b"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(root, "existing", "a", "b"),
		filepath.Join(root, "existing", "a"),
	}, created, "deepest first, and never the directory that was already there")

	again, err := mkdirAllTrackingCreated(filepath.Join(root, "existing", "a", "b"))
	require.NoError(t, err)
	assert.Empty(t, again, "nothing was created the second time, so nothing may be removed")

	removeCreatedDirs(created)
	assert.NoDirExists(t, filepath.Join(root, "existing", "a"))
	assert.DirExists(t, filepath.Join(root, "existing"), "and the pre-existing directory survives")
}

// TestRemoveCreatedDirs_LeavesADirectorySomethingElseFilled is the guard that keeps the cleanup
// from deleting a neighbour's work when two writes share a new folder.
func TestRemoveCreatedDirs_LeavesADirectorySomethingElseFilled(t *testing.T) {
	root := t.TempDir()
	created, err := mkdirAllTrackingCreated(filepath.Join(root, "shared", "deep"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "shared", "deep", "written-by-someone-else.yaml"), []byte("x"), 0o600))

	removeCreatedDirs(created)

	assert.DirExists(t, filepath.Join(root, "shared", "deep"),
		"a directory that is no longer empty belongs to whoever filled it")
}

// TestMkdirAllTrackingCreated_UndoesAPartialCreation covers the failure inside the creation itself,
// rather than in the write that follows it.
//
// os.MkdirAll builds ancestors before the path it was asked for, so a failure deeper down leaves
// them behind. The caller only sees an error and has no set to clean up, and an empty directory
// produces no worktree status entry, so nothing later would ever find them.
//
// Reaching that state takes a failure the PROBE cannot see coming, which is narrower than it looks:
// a component longer than the filesystem allows is one, because the stat that walks the path up
// answers ENOENT at the first absent ancestor and never evaluates the long name. MkdirAll then
// creates every ancestor and fails on the last element alone. An earlier version of this test used
// a file blocking a component instead, which stat reports as ENOTDIR, so the function returned
// before MkdirAll ran and the undo below was never exercised. That case is worth keeping, and is
// the test underneath.
func TestMkdirAllTrackingCreated_UndoesAPartialCreation(t *testing.T) {
	root := t.TempDir()

	target := filepath.Join(root, "a", "b", strings.Repeat("x", 256))

	created, err := mkdirAllTrackingCreated(target)
	require.Error(t, err, "a component longer than the filesystem allows fails the mkdir")
	assert.Empty(t, created, "a failed creation reports nothing for the caller to undo")
	assert.NoDirExists(t, filepath.Join(root, "a"),
		"and undoes every ancestor it made on the way down, not just the deepest")
}

// TestMkdirAllTrackingCreated_RefusesAPathBlockedByAFile is the other failure shape, and it fails
// EARLIER: stat answers ENOTDIR while walking the path up, so nothing is created and there is
// nothing to undo. It is here because that early return is what makes the test above the only
// place the undo runs.
func TestMkdirAllTrackingCreated_RefusesAPathBlockedByAFile(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "blocker"), []byte("x"), 0o600))

	created, err := mkdirAllTrackingCreated(filepath.Join(root, "a", "b", "blocker", "deeper"))
	require.Error(t, err, "a file where a directory component belongs is not a path we can make")
	assert.Empty(t, created, "a failed creation reports nothing for the caller to undo")
	assert.NoDirExists(t, filepath.Join(root, "a", "b", "blocker", "deeper"),
		"and leaves nothing of its own behind")
	assert.FileExists(t, filepath.Join(root, "a", "b", "blocker"),
		"while what was already there is untouched")
}

// TestRemoveCreatedDirs_WalksPastDirectoriesThatWereNeverMade is the behaviour the partial-failure
// cleanup depends on: the deepest entries may not exist, and stopping there would strand the
// ancestors that DO exist and ARE ours.
func TestRemoveCreatedDirs_WalksPastDirectoriesThatWereNeverMade(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "made", "also-made"), 0o750))

	removeCreatedDirs([]string{
		filepath.Join(root, "made", "also-made", "never-made"), // deepest first, absent
		filepath.Join(root, "made", "also-made"),
		filepath.Join(root, "made"),
	})

	assert.NoDirExists(t, filepath.Join(root, "made"),
		"an absent deepest entry must not stop the walk up")
}
