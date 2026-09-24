// SPDX-License-Identifier: Apache-2.0

package git

// Repointing a GitProvider at a different repository, which is the documented way to change a
// destination: spec.url is immutable, so an operator deletes the object and creates it again with
// the new URL (see GitProviderSpec).
//
// Nothing about that reaches a live branch worker. Workers are keyed by
// (GitProvider namespace, GitProvider name, branch) and the GitTarget that owns the worker is
// untouched, so the worker survives the repoint — and it goes on writing to the repository it was
// CREATED for, because that is the one its clone, its base trust and its retained commits are
// about. The WorkerManager is what notices, on the GitTarget's next reconcile, and it replaces the
// worker rather than pointing this one somewhere new (see worker_repo_identity_test.go).
//
// The alternative, following the provider's URL on every cycle, is what produced #382: the cycle
// planned against trust earned on the old repository, skipped the fetch that would have
// established the new checkout, and every write from then on failed at "repository does not
// exist".

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

// TestRepoint_TheWorkerGoesOnWritingToItsOwnRepository is the in-window guarantee. A repointed
// GitProvider is noticed at the GitTarget's next reconcile, and until then the worker is still
// live and still reads that provider on every cycle for credentials and commit identity. What it
// must NOT take from it is the destination: writing to the new URL with the old repository's base
// trust and clone path is the failure this keying replaced.
func TestRepoint_TheWorkerGoesOnWritingToItsOwnRepository(t *testing.T) {
	f := newLedgerFixture(t, "repoint-provider", true)

	f.publish("before-the-repoint")
	require.True(t, f.worker.baseTrusted(), "a successful push leaves the base trusted")

	elsewhere := f.repointProvider(t)

	f.commit(false, "after-the-repoint")
	f.push()

	assert.Contains(t, configMapFileNames(t, f.repoDir), "after-the-repoint.yaml",
		"the write went to the repository this worker is about")
	assert.Empty(t, configMapFileNames(t, elsewhere),
		"and nothing reached the repository the provider now names: a different worker serves that")
	assert.True(t, f.worker.baseTrusted(),
		"nothing about the provider's URL invalidates what this worker knows about its own remote")
}

// TestRepoint_RetainedWritesAreUnaffected is the other half. A worker holding retained work skips
// the head-of-cycle guard entirely, because a reset would destroy the local commits those writes
// produced. That used to be the case an identity check had to be placed in front of the guard to
// catch; now there is nothing to catch, and the retained work simply finishes where it started.
func TestRepoint_RetainedWritesAreUnaffected(t *testing.T) {
	f := newLedgerFixture(t, "repoint-with-retained", true)

	f.publish("before-the-repoint")
	f.commit(false, "retained")

	elsewhere := f.repointProvider(t)

	joining := []Event{configMapEvent("joins-the-cycle", "alice", "team-a")}
	retained, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, joining)
	require.NoError(t, err)
	require.NoError(t, f.worker.commitPendingWrites([]PendingWrite{*retained}, true),
		"the local commits behind the retained writes are in this worker's own clone, where they always were")
	f.push()

	assert.Contains(t, configMapFileNames(t, f.repoDir), "joins-the-cycle.yaml")
	assert.Empty(t, configMapFileNames(t, elsewhere))
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

// configMapFileNames lists the ConfigMap documents on main in a bare repository, which is how each
// test says "the write landed HERE, and nowhere else".
func configMapFileNames(t *testing.T, bareRepoDir string) []string {
	t.Helper()

	repo, err := gogit.PlainOpen(bareRepoDir)
	require.NoError(t, err)
	ref, err := repo.Reference("refs/heads/main", true)
	if err != nil {
		// No branch at all is the honest answer for a repository nothing ever wrote to.
		return nil
	}
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	var found []string
	require.NoError(t, tree.Files().ForEach(func(file *object.File) error {
		if filepath.Ext(file.Name) == ".yaml" && filepath.Base(filepath.Dir(file.Name)) == "configmaps" {
			found = append(found, filepath.Base(file.Name))
		}
		return nil
	}))
	return found
}
