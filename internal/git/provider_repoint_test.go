// SPDX-License-Identifier: Apache-2.0

package git

// Repointing a GitProvider at a different repository, which is the documented way to change a
// destination: spec.url is immutable, so an operator deletes the object and creates it again with
// the new URL (see GitProviderSpec).
//
// Nothing about that restarts the branch worker. Workers are keyed by
// (GitProvider namespace, GitProvider name, branch) and the GitTarget that owns the worker is
// untouched, so the SAME worker meets the new repository — carrying the base trust it earned
// against the old one. Each remote gets its own on-disk clone, so that trust is not merely stale,
// it is about a checkout that does not exist at the new path: the cycle skips the fetch that would
// have created it and fails on every write with "repository does not exist".

import (
	"os/exec"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// TestBaseTrust_RepointedProviderDoesNotInheritTheOldRepositorysTrust publishes to one remote,
// repoints the provider at another, and requires the next publication to land on the new one.
func TestBaseTrust_RepointedProviderDoesNotInheritTheOldRepositorysTrust(t *testing.T) {
	f := newLedgerFixture(t, "repoint-provider", true)

	f.publish("before-the-repoint")
	require.True(t, f.worker.baseTrusted(), "a successful push leaves the base trusted")

	second := f.repointProvider(t)

	// The write that follows must reach the new repository. Before trust was bound to the
	// repository it was gained against, this failed with "open repository: repository does not
	// exist" and kept failing, because nothing ever initialised the new checkout.
	f.commit(false, "after-the-repoint")
	f.push()

	assert.Equal(t, "after-the-repoint.yaml", firstConfigMapFileName(t, second),
		"the write went to the repository the provider now names")
}

// TestBaseTrust_RepointIsNoticedEvenWhileWritesAreRetained is the other half. A worker holding
// retained work skips the head-of-cycle guard entirely (a reset would destroy the local commits
// those writes produced), so the identity check must not live behind that guard: the trust has to
// be dropped on the cycle that skips it, or it would survive into the cycle that follows the
// retained work and take that one straight past its fetch as well.
//
// The retained work itself cannot be rescued, and that is not this change's business: its local
// commits are in the old clone, and the cycle fails at the open. That failure is identical before
// and after base trust existed.
func TestBaseTrust_RepointIsNoticedEvenWhileWritesAreRetained(t *testing.T) {
	f := newLedgerFixture(t, "repoint-with-retained", true)

	f.publish("before-the-repoint")
	f.commit(false, "retained")
	require.True(t, f.worker.baseTrusted())

	second := f.repointProvider(t)

	// A cycle with work in hand: the guard is skipped, so only the identity check can drop trust.
	joining := []Event{configMapEvent("joins-the-cycle", "alice", "team-a")}
	retained, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, joining)
	require.NoError(t, err)
	require.Error(t, f.worker.commitPendingWrites([]PendingWrite{*retained}, true),
		"the local commits behind the retained writes are in the repository the provider no longer names")

	assert.False(t, f.worker.baseTrusted(),
		"the provider names a different repository than the one this trust was gained against")

	// Once nothing is retained, that dropped trust is what makes the next cycle establish the new
	// checkout rather than plan against a base it never had.
	f.pending = nil
	f.publish("after-the-retained-work")
	assert.Equal(t, "after-the-retained-work.yaml", firstConfigMapFileName(t, second))
}

// repointProvider deletes the fixture's GitProvider and creates it again pointing at a second
// repository, then returns that repository's path on disk.
func (f *ledgerFixture) repointProvider(t *testing.T) string {
	t.Helper()

	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, "elsewhere.git")
	createBareRepo(t, repoDir)
	out, err := exec.Command("git", "-C", repoDir, "config", "http.receivepack", "true").CombinedOutput()
	require.NoError(t, err, "git config http.receivepack: %s", out)
	simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "a different repository\n")
	sim := startRealGitServer(t, projectRoot, repoDir)

	previous, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	require.NoError(t, f.worker.Client.Delete(f.worker.ctx, previous))

	recreated := &configv1alpha3.GitProvider{}
	recreated.Name = previous.Name
	recreated.Namespace = previous.Namespace
	recreated.Spec = configv1alpha3.GitProviderSpec{URL: sim.RepoURL}
	require.NoError(t, f.worker.Client.Create(f.worker.ctx, recreated))

	// The worker deliberately survives: this is the state the regression is about.
	require.NotNil(t, f.worker)
	return repoDir
}

// firstConfigMapFileName reads the name of the single ConfigMap document the worker wrote into the
// given bare repository, which is how each test says "the write landed HERE".
func firstConfigMapFileName(t *testing.T, bareRepoDir string) string {
	t.Helper()

	repo, err := gogit.PlainOpen(bareRepoDir)
	require.NoError(t, err)
	ref, err := repo.Reference("refs/heads/main", true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	var found string
	require.NoError(t, tree.Files().ForEach(func(file *object.File) error {
		if filepath.Ext(file.Name) == ".yaml" && filepath.Base(filepath.Dir(file.Name)) == "configmaps" {
			found = filepath.Base(file.Name)
		}
		return nil
	}))
	require.NotEmpty(t, found, "no ConfigMap document in the repository the provider now names")
	return found
}
