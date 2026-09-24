// SPDX-License-Identifier: Apache-2.0

package git

// Repointing a GitProvider at a different repository, which is the documented way to change a
// destination: spec.url is immutable, so an operator deletes the object and creates it again with
// the new URL (see GitProviderSpec).
//
// Nothing about that reaches a live branch worker. Workers are keyed by
// (GitProvider namespace, GitProvider name, branch) and the GitTarget that owns the worker is
// untouched, so the worker survives the repoint. It does not follow the provider to the new
// repository: it is FOR the old one, and the WorkerManager replaces it at the GitTarget's next
// reconcile (see worker_repo_identity_test.go).
//
// Following the provider's URL on every cycle is what produced #382: the cycle planned against
// trust earned on the old repository, skipped the fetch that would have established the new
// checkout, and every write from then on failed at "repository does not exist".
//
// Until the replacement arrives the worker does NOTHING, and that is deliberate rather than
// incidental. It would otherwise be writing to a repository the operator has just pointed away
// from, with credentials read from the object that now names a different one: the replacement is
// the only thing that resolves it, and a replacement blocked by a failing Validated gate never
// comes. So the mismatch fails the cycle loudly instead.

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

// TestRepoint_TheWorkerStopsRatherThanWriteWithTheNewProvidersCredentials. A repointed
// GitProvider is noticed at the GitTarget's next reconcile, and until then the worker is still
// live and still reads that provider on every cycle for credentials, commit identity and signing.
// What it must not do is combine the two: its own repository with the replacement's credential.
func TestRepoint_TheWorkerStopsRatherThanWriteWithTheNewProvidersCredentials(t *testing.T) {
	f := newLedgerFixture(t, "repoint-provider", true)

	f.publish("before-the-repoint")
	require.True(t, f.worker.baseTrusted(), "a successful push leaves the base trusted")

	// Planned while the provider still agreed, so the refusal below is the write path's and not
	// merely the planner's: both read the GitProvider, and both have to refuse.
	events := []Event{configMapEvent("after-the-repoint", "alice", "team-a")}
	write, err := f.worker.buildGroupedPendingWrite(f.worker.ctx, events)
	require.NoError(t, err)

	elsewhere := f.repointProvider(t)

	require.Error(t, f.worker.commitPendingWrites([]PendingWrite{*write}, false),
		"the GitProvider names a repository this worker is not for")
	_, err = f.worker.buildGroupedPendingWrite(f.worker.ctx, events)
	require.Error(t, err, "and nothing new is planned against it either")
	assert.Contains(t, err.Error(), "waiting to be replaced")

	assert.NotContains(t, configMapFileNames(t, f.repoDir), "after-the-repoint.yaml",
		"nothing more goes into the repository the operator has pointed away from")
	assert.Empty(t, configMapFileNames(t, elsewhere),
		"and nothing reaches the one the provider now names: a different worker serves that")
}

// TestRepoint_WhatWasAlreadyWrittenIsUntouched is the other half of stopping. The worker refuses
// the cycle; it does not undo, re-target or lose what it had already pushed, and its own checkout
// is still the one it was always about.
func TestRepoint_WhatWasAlreadyWrittenIsUntouched(t *testing.T) {
	f := newLedgerFixture(t, "repoint-with-retained", true)

	f.publish("before-the-repoint")
	require.Contains(t, configMapFileNames(t, f.repoDir), "before-the-repoint.yaml")

	elsewhere := f.repointProvider(t)

	assert.Contains(t, configMapFileNames(t, f.repoDir), "before-the-repoint.yaml",
		"a repoint is not a retraction of what is already in the old repository")
	assert.Empty(t, configMapFileNames(t, elsewhere))
	assert.Equal(t, f.sim.RepoURL, f.worker.repo.URL,
		"and the worker still knows which repository it is for")
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
