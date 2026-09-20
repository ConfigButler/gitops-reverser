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
// See docs/design/inbound-push-notification.md §3.1 and §5.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedResetFixture builds a bare remote with one commit and a local clone of it.
func seedResetFixture(t *testing.T) (string, string) {
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
	return repoPath, remoteURL
}

// TestReset_DiscardsEveryKindOfLeftover is the claim §5 makes, tested against each way a failed
// write can leave the worktree.
//
// A half-finished executePendingWrites can leave any of these: a modified tracked file, a brand
// new file it staged before dying, a brand new file it had not staged yet, and a whole directory
// it created for a new placement.
func TestReset_DiscardsEveryKindOfLeftover(t *testing.T) {
	repoPath, remoteURL := seedResetFixture(t)

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

	_ = remoteURL
}
