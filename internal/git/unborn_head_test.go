// SPDX-License-Identifier: Apache-2.0

package git

// An empty repository's HEAD names a branch that has no commit yet. Protocol v2 servers report it
// (ls-refs "unborn"), v0 servers do not. These pin that the name reaches the observation when the
// server says it, that it is never guessed when the server does not, and that knowing it changes
// nothing about what the worker writes.

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// emptyRepoServedOver serves an empty bare repository whose HEAD names trunk, over v2 or v0.
func emptyRepoServedOver(t *testing.T, v2 bool) *adoSimulator {
	t.Helper()
	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, "repo.git")
	out, err := exec.Command("git", "init", "--bare", "-b", "trunk", repoDir).CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	gitIn(t, repoDir, "config", "http.receivepack", "true")
	sim := startRealGitServer(t, projectRoot, repoDir)
	sim.protocolV2.Store(v2)
	return sim
}

func TestListRemoteRefs_AnEmptyRepositoryNamesItsUnbornHeadOnlyOverV2(t *testing.T) {
	for name, tc := range map[string]struct {
		v2         bool
		want       plumbing.ReferenceName
		wantPosts  int64
		wantReport string
	}{
		// GetRemoteRefs' ls-refs, whose answer go-git discards, and ours.
		"v2 reports it": {v2: true, want: "refs/heads/trunk", wantPosts: 2, wantReport: "trunk"},
		// The v0 advertisement is the whole answer; nothing more is asked.
		"v0 does not": {v2: false, want: "", wantPosts: 0, wantReport: ""},
	} {
		t.Run(name, func(t *testing.T) {
			sim := emptyRepoServedOver(t, tc.v2)

			refs, unborn, err := listRemoteRefs(context.Background(), sim.RepoURL, nil)
			require.NoError(t, err)
			assert.Empty(t, refs)
			assert.Equal(t, tc.want, unborn)
			assert.Equal(t, tc.wantPosts, sim.uploadPackPosts.Load())

			info, err := CheckRepo(context.Background(), sim.RepoURL, nil)
			require.NoError(t, err)
			assert.Equal(t, 0, info.RemoteBranchCount)
			if tc.want == "" {
				assert.Nil(t, info.DefaultBranch, "no guess")
			} else {
				assert.Equal(t, &BranchInfo{ShortName: tc.want.Short(), Unborn: true}, info.DefaultBranch)
			}

			report, err := PrepareBranch(
				context.Background(), sim.RepoURL, filepath.Join(t.TempDir(), "clone"), "feature", nil)
			require.NoError(t, err)
			assert.True(t, report.HEAD.Unborn, "the first commit is still a root on the write branch")
			assert.Equal(t, "feature", report.HEAD.ShortName)
			assert.Equal(t, tc.wantReport, report.ParentBranch)
		})
	}
}

func TestListRemoteRefs_ARepositoryWithCommitsHasNoUnbornHeadOverV2(t *testing.T) {
	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, "repo.git")
	createBareRepo(t, repoDir)
	main := simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "x\n")
	gitIn(t, repoDir, "tag", "-a", "-m", "annotated", "v1", main.String())
	sim := startRealGitServer(t, projectRoot, repoDir)
	sim.protocolV2.Store(true)

	refs, unborn, err := listRemoteRefs(context.Background(), sim.RepoURL, nil)
	require.NoError(t, err)
	assert.Empty(t, unborn)
	assert.Equal(t, int64(1), sim.uploadPackPosts.Load(), "one ls-refs, as Remote.List makes")
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name().String())
	}
	assert.ElementsMatch(t, []string{"HEAD", "refs/heads/main", "refs/tags/v1"}, names, "peeled refs left out")
}

// allowBranches lets the fixture's GitBranch write patterns through its GitProvider.
func (f *realServerNewBranch) allowBranches(patterns ...configv1alpha3.BranchPattern) {
	f.t.Helper()
	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(f.t, err)
	provider.Spec.AllowedGitBranches = []configv1alpha3.GitProviderGitBranchGrant{{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
		BranchPatterns:     patterns,
	}}
	require.NoError(f.t, f.worker.Client.Update(f.worker.ctx, provider))
}

// emptyTrunkRepoOverV2 is an empty repository whose HEAD names trunk, served over v2, with the
// write branch feature.
func emptyTrunkRepoOverV2(t *testing.T, slug string) *realServerNewBranch {
	t.Helper()
	f := newRealServerNewBranch(t, slug, func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
	})
	f.sim.protocolV2.Store(true)
	return f
}

// assertFeatureStartsOnInitializedTrunk checks trunk is one empty root commit, and feature's first commit sits on it.
func (f *realServerNewBranch) assertFeatureStartsOnInitializedTrunk() {
	f.t.Helper()
	trunk := f.ref("trunk")
	require.False(f.t, trunk.IsZero(), "trunk is created")
	assert.Empty(f.t, f.parentOf(trunk), "trunk starts with a root commit")
	assert.Empty(f.t, gitIn(f.t, f.repoDir, "ls-tree", "-r", trunk.String()), "an empty one")
	assert.Contains(f.t, gitIn(f.t, f.repoDir, "log", "-1", "--format=%s", trunk.String()),
		"chore: initialize repository")

	tip := f.ref("feature")
	require.False(f.t, tip.IsZero(), "feature is created")
	assert.Equal(f.t, trunk.String(), gitIn(f.t, f.repoDir, "merge-base", "feature", "trunk"),
		"feature starts from trunk")
	assert.Contains(f.t, gitIn(f.t, f.repoDir, "ls-tree", "-r", "--name-only", "feature"), cmFile("cm1"))
}

// The write branch is not the default branch, and the GitBranch may write the default branch: it is
// created first with one empty commit, and the write branch starts from it.
func TestBranchWorker_AnEmptyRepositoryGetsItsDefaultBranchFirst(t *testing.T) {
	f := emptyTrunkRepoOverV2(t, "unborn-default-created")
	f.allowBranches("trunk", "feature")

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	o := f.observed()
	assert.Equal(t, ParentFound, o.ParentState, "trunk exists now")
	assert.Equal(t, "trunk", o.ParentBranch)
	assert.Equal(t, f.ref("trunk").String(), o.ParentCommit)
	assert.True(t, f.ref("feature").IsZero(), "nothing written yet")

	liveWrite(f.loop, "cm1")
	f.assertFeatureStartsOnInitializedTrunk()
}

// The GitBranch may not write the default branch: nothing is written, the repository is reported
// Unborn with the name, and the write is held. Allowing the branch is enough: the next probe creates
// it and the held write lands on it.
func TestBranchWorker_AnEmptyRepositoryWaitsUntilItsDefaultBranchIsAllowed(t *testing.T) {
	f := emptyTrunkRepoOverV2(t, "unborn-default-not-allowed")
	f.allowBranches("feature")

	err := f.worker.ensureRepositoryInitialized(f.worker.ctx)
	require.ErrorIs(t, err, ErrDefaultBranchNotAllowed)
	o := f.observed()
	assert.Equal(t, ParentUnborn, o.ParentState)
	assert.Equal(t, "trunk", o.ParentBranch)

	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	require.NoError(t, f.loop.refreshFromRemote(provider))
	assert.Equal(t, ParentUnborn, f.observed().ParentState, "the refresh agrees")
	assert.Equal(t, "trunk", f.observed().ParentBranch)

	liveWrite(f.loop, "cm1")
	assert.True(t, f.ref("trunk").IsZero(), "trunk is not created")
	assert.True(t, f.ref("feature").IsZero(), "nothing is written")
	assert.Len(t, f.loop.pendingWrites, 1, "the write is held")

	pastProbeDeadline(f.worker)
	f.loop.runParentProbe()
	assert.True(t, f.ref("trunk").IsZero(), "a probe without permission changes nothing")
	assert.Len(t, f.loop.pendingWrites, 1)

	f.allowBranches("trunk", "feature")
	pastProbeDeadline(f.worker)
	f.loop.runParentProbe()
	f.assertFeatureStartsOnInitializedTrunk()
	assert.Empty(t, f.loop.pendingWrites, "the held write landed")
}

// Somebody else creates the default branch between our advertisement and our upload. The server
// refuses our first commit, and the write branch starts from theirs instead.
func TestBranchWorker_AnEmptyRepositoryWhoseDefaultBranchAppearsDuringOurPushStartsFromIt(t *testing.T) {
	f := emptyTrunkRepoOverV2(t, "unborn-default-race")
	f.allowBranches("trunk", "feature")

	var theirs plumbing.Hash
	afterPushValidation = func() {
		afterPushValidation = nil
		theirs = branchOnDisk(t, f.repoDir, "trunk", "theirs\n")
	}
	t.Cleanup(func() { afterPushValidation = nil })

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	require.False(t, theirs.IsZero(), "trunk was created during our push")
	assert.Equal(t, theirs, f.ref("trunk"), "theirs stands")
	assert.Equal(t, ParentFound, f.observed().ParentState)
	assert.Equal(t, theirs.String(), f.observed().ParentCommit)

	liveWrite(f.loop, "cm1")
	assert.Equal(t, theirs.String(), gitIn(t, f.repoDir, "merge-base", "feature", "trunk"))
}

// The write branch IS the default branch: it starts the history, nothing else is created, and a
// push, which cannot see HEAD, keeps the name the fetch read.
func TestBranchWorker_AnEmptyRepositoryWhoseDefaultIsTheWriteBranchStartsIt(t *testing.T) {
	f := newRealServerNewBranch(t, "unborn-default-is-write-branch", func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/feature")
	})
	f.sim.protocolV2.Store(true)

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	assert.Equal(t, ParentUnborn, f.observed().ParentState)
	assert.Equal(t, "feature", f.observed().ParentBranch)

	noOpWrite(f.loop)
	assert.Equal(t, ObservedByPush, f.observed().By)
	assert.Equal(t, "feature", f.observed().ParentBranch, "a push keeps the name the fetch read")
}

// Over v0 the server does not name the default branch: nothing is guessed, and the write branch
// starts as the repository's first branch.
func TestBranchWorker_AnEmptyRepositoryOverV0StartsTheWriteBranch(t *testing.T) {
	f := newRealServerNewBranch(t, "unborn-head-v0", func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
	})

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	assert.Equal(t, ParentUnborn, f.observed().ParentState)
	assert.Empty(t, f.observed().ParentBranch)

	liveWrite(f.loop, "cm1")
	tip := f.ref("feature")
	require.False(t, tip.IsZero())
	assert.Empty(t, f.parentOf(tip), "a root commit")
	assert.True(t, f.ref("trunk").IsZero())
}

// §4.3 test 1 over v2. A repository that is not empty but whose HEAD dangles is still refused, and
// over v2 the server says what HEAD names, so the status can name it.
func TestBranchWorker_ADanglingRemoteHeadIsNamedOverV2(t *testing.T) {
	f := newRealServerNewBranch(t, "dangling-head-v2", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/master")
	})
	f.sim.protocolV2.Store(true)

	err := f.worker.ensureRepositoryInitialized(f.worker.ctx)
	require.ErrorIs(t, err, ErrDefaultBranchUnresolved)
	assert.Equal(t, ParentMissing, f.observed().ParentState)
	assert.Equal(t, "master", f.observed().ParentBranch)
}
