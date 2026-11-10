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

// The worker reports the name with ParentUnborn from every look at the remote: the fetch, a
// refresh, and a push, which cannot see HEAD and keeps what discovery read.
func TestBranchWorker_AnEmptyRepositoryReportsItsUnbornHeadOverV2(t *testing.T) {
	f := newRealServerNewBranch(t, "unborn-head-v2", func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
	})
	f.sim.protocolV2.Store(true)

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	o := f.observed()
	assert.Equal(t, ParentUnborn, o.ParentState)
	assert.Equal(t, "trunk", o.ParentBranch)
	assert.Empty(t, o.ParentCommit)

	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	require.NoError(t, f.loop.refreshFromRemote(provider))
	assert.Equal(t, ParentUnborn, f.observed().ParentState, "the refresh agrees")
	assert.Equal(t, "trunk", f.observed().ParentBranch)

	noOpWrite(f.loop)
	o = f.observed()
	assert.Equal(t, ObservedByPush, o.By)
	assert.Equal(t, ParentUnborn, o.ParentState)
	assert.Equal(t, "trunk", o.ParentBranch, "a push keeps the name discovery read")
}

// Knowing the name changes no behavior: the first write is still a root commit on the write branch,
// and the branch HEAD names is not created.
func TestBranchWorker_AnEmptyRepositoryStillBootstrapsOverV2(t *testing.T) {
	f := newRealServerNewBranch(t, "unborn-bootstrap-v2", func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
	})
	f.sim.protocolV2.Store(true)
	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	require.Equal(t, "trunk", f.observed().ParentBranch)

	liveWrite(f.loop, "cm1")
	tip := f.ref("feature")
	require.False(t, tip.IsZero())
	assert.Empty(t, f.parentOf(tip), "a root commit")
	assert.True(t, f.ref("trunk").IsZero(), "trunk is not created")
}

// Over v0 the same repository is Unborn with no name: the server did not say, so nothing is guessed.
func TestBranchWorker_AnEmptyRepositoryNamesNoBranchOverV0(t *testing.T) {
	f := newRealServerNewBranch(t, "unborn-head-v0", func(repoDir string) {
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/trunk")
	})

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	assert.Equal(t, ParentUnborn, f.observed().ParentState)
	assert.Empty(t, f.observed().ParentBranch)
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
