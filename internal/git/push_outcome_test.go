// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushOutcomeRemote serves a bare repository over canonical git's CGI backend and returns its URL.
//
// It is a REAL server on purpose. go-git v6's in-process receive-pack never compares cmd.Old, so
// every compare-and-swap assertion made over a file:// remote passes vacuously; an outcome read
// off such a push would be measuring the library rather than the protocol.
func pushOutcomeRemote(t *testing.T, slug string) string {
	t.Helper()

	projectRoot := t.TempDir()
	repoDir := filepath.Join(projectRoot, slug+".git")
	createBareRepo(t, repoDir)

	// http-backend refuses receive-pack unless the repository opts in.
	out, err := exec.Command("git", "-C", repoDir, "config", "http.receivepack", "true").CombinedOutput()
	require.NoError(t, err, "git config http.receivepack: %s", out)

	return startRealGitServer(t, projectRoot, repoDir).RepoURL
}

// TestPushAtomic_ReportsWhatTheSessionLearned walks the three non-error exits in the order a
// branch lives through them: nothing there, a branch created, and the same push repeated.
//
// The point of each assertion is the Head. Before PushOutcome the caller got a bare nil and had
// to guess from the last FETCH what the push had just done, which was wrong on exactly the
// busiest targets.
func TestPushAtomic_ReportsWhatTheSessionLearned(t *testing.T) {
	ctx := context.Background()
	remoteURL := pushOutcomeRemote(t, "outcomes")
	localPath := filepath.Join(t.TempDir(), "local")
	localRepo, worktree := initLocalRepo(t, localPath, remoteURL, "main")
	branch := plumbing.NewBranchReferenceName("main")

	// 1. Nothing local and nothing on the remote. The advertisement still answered the question.
	noBranch, err := PushAtomic(ctx, localRepo, plumbing.ZeroHash, branch, nil)
	require.NoError(t, err)
	assert.Equal(t, PushNoBranch, noBranch.Kind)
	assert.Equal(t, plumbing.ZeroHash, noBranch.Head,
		"a branch does not exist without a commit, so no revision and no branch are one fact")

	// 2. The push that CREATES the branch. The server took the ref update, so the outcome names
	// the hash it took.
	created := commitFileChange(t, worktree, localPath, "README.md", "first\n")
	accepted, err := PushAtomic(ctx, localRepo, plumbing.ZeroHash, branch, nil)
	require.NoError(t, err)
	assert.Equal(t, PushAccepted, accepted.Kind)
	assert.Equal(t, created, accepted.Head)

	// 3. The same state again. Nothing is sent, and the advertisement proves where the branch is.
	upToDate, err := PushAtomic(ctx, localRepo, created, branch, nil)
	require.NoError(t, err)
	assert.Equal(t, PushUpToDate, upToDate.Kind)
	assert.Equal(t, created, upToDate.Head,
		"up to date is an observation with a revision, not an empty signal")
}

// TestPushCycle_CreatingTheBranchTakesTrust is the behaviour change the outcome buys.
//
// The old guard read branchExists from the last fetch — the only writer — so a push that created
// the branch left it false and trust was not taken, even though the server had just accepted the
// ref. The next cycle then fetched for nothing.
func TestPushCycle_CreatingTheBranchTakesTrust(t *testing.T) {
	f := newLedgerFixture(t, "push-creates-branch", false)

	f.publish("created-by-the-first-push")

	assert.True(t, f.worker.baseTrusted(),
		"the server accepted the ref update, so the worktree sits at the remote tip")
}

// TestRemoteObservation_APushRenewsIt is where "a push is a refresh" stops being a comment and
// becomes structural. The record the refresher reads must name the hash the push just made the
// tip, not whatever the last fetch happened to see.
func TestRemoteObservation_APushRenewsIt(t *testing.T) {
	f := newLedgerFixture(t, "push-renews-observation", true)

	before, ok := f.worker.LastRemoteObservation()
	require.False(t, ok, "nothing has looked at the remote yet")
	require.Empty(t, before.Revision)

	f.publish("first")
	afterFirst, ok := f.worker.LastRemoteObservation()
	require.True(t, ok)
	assert.Equal(t, ObservedByPush, afterFirst.By,
		"the push is the last thing that touched the remote, so it owns the record")
	assert.Equal(t, revParseMain(t, f.repoDir), afterFirst.Revision)

	f.publish("second")
	afterSecond, _ := f.worker.LastRemoteObservation()
	assert.Equal(t, revParseMain(t, f.repoDir), afterSecond.Revision,
		"a second push moves the revision with it")
	assert.NotEqual(t, afterFirst.Revision, afterSecond.Revision)
	assert.False(t, afterSecond.At.Before(afterFirst.At), "and renews the clock")
}
