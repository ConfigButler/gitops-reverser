// SPDX-License-Identifier: Apache-2.0

package git

// An empty repository is one whose advertisement carries no hash refs at all. These pin what the
// worker does at the edges of that definition, against canonical git over HTTP: a repository that
// stops being empty while work is planned on it (§4.2), and one that is not empty but whose
// default branch does not resolve (§4.3). Neither may ever produce an orphan branch.

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// realServerNewBranch is a worker for the absent write branch `feature`, against a real server.
type realServerNewBranch struct {
	*ledgerFixture

	loop *branchWorkerEventLoop
}

func newRealServerNewBranch(t *testing.T, slug string, seed func(repoDir string)) *realServerNewBranch {
	t.Helper()
	f := newLedgerFixtureOnBranch(t, slug, false, "feature")
	if seed != nil {
		seed(f.repoDir)
	}
	f.worker.mapper = configMapMapper()
	createGitTarget(t, f.worker, newBranchTarget, newBranchFolder,
		&configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneAlways})
	loop := newBranchWorkerEventLoop(f.worker, 0)
	t.Cleanup(loop.stopTimers)
	return &realServerNewBranch{ledgerFixture: f, loop: loop}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// ref is where a branch is on the remote, or zero when the remote does not carry it.
func (f *realServerNewBranch) ref(name string) plumbing.Hash {
	f.t.Helper()
	repo, err := gogit.PlainOpen(f.repoDir)
	require.NoError(f.t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

func (f *realServerNewBranch) parentOf(commit plumbing.Hash) []plumbing.Hash {
	f.t.Helper()
	repo, err := gogit.PlainOpen(f.repoDir)
	require.NoError(f.t, err)
	c, err := repo.CommitObject(commit)
	require.NoError(f.t, err)
	return c.ParentHashes
}

func (f *realServerNewBranch) observed() RemoteObservation {
	f.t.Helper()
	o, known := f.worker.LastRemoteObservation()
	require.True(f.t, known)
	return o
}

// tagOnly gives the remote a tag on a commit no branch carries: not empty, and no default branch.
func tagOnly(t *testing.T, repoDir string) {
	t.Helper()
	tree := gitIn(t, repoDir, "mktree")
	commit := gitIn(t, repoDir, "-c", "user.name=Outside", "-c", "user.email=o@example.com",
		"commit-tree", tree, "-m", "tagged")
	gitIn(t, repoDir, "tag", "v1", commit)
}

// branchOnDisk creates a branch with one README commit on the bare repository directly, which works
// whatever the repository holds already.
func branchOnDisk(t *testing.T, repoDir, branch, content string) plumbing.Hash {
	t.Helper()
	cmd := exec.Command("git", "-C", repoDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	require.NoError(t, err)
	mk := exec.Command("git", "-C", repoDir, "mktree")
	mk.Stdin = strings.NewReader("100644 blob " + strings.TrimSpace(string(out)) + "\tREADME.md\n")
	tree, err := mk.Output()
	require.NoError(t, err)
	commit := gitIn(t, repoDir, "-c", "user.name=Outside", "-c", "user.email=o@example.com",
		"commit-tree", strings.TrimSpace(string(tree)), "-m", "outside")
	gitIn(t, repoDir, "update-ref", "refs/heads/"+branch, commit)
	return plumbing.NewHash(commit)
}

// standbyRequest waits for a window and closes it at once: the write it attaches to is committed
// and, in a cooldown, retained.
func standbyRequest() *AttachCommitRequest {
	req := attachReq("alice", time.Hour)
	req.GitTargetName = newBranchTarget
	req.MaxDuration = 0
	return req
}

// §4.2 test 1. The work was planned on an empty repository; main appears before publication. The
// push must not create feature as an orphan: it is refused at the advertisement, and the replay
// builds on main.
func TestBranchWorker_AnEmptyRepositoryThatGainsABranchIsNotOrphaned(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newRealServerNewBranch(t, "empty-gains-main", nil)

	f.loop.lastPushAt = time.Now() // cooldown: the write is retained
	req := standbyRequest()
	serviceAttach(f.loop, req)
	liveWrite(f.loop, "cm1")
	require.Len(t, f.loop.pendingWrites, 1)
	contention := fetchCount(t, reader, f.worker, fetchReasonContention)

	main := simulateClientCommitOnDisk(t, f.repoDir, "main", "README.md", "main appeared\n")
	f.loop.pushPending()

	tip := f.ref("feature")
	require.False(t, tip.IsZero(), "the write was published")
	assert.Equal(t, []plumbing.Hash{main}, f.parentOf(tip), "on main's tip, not as a root commit")
	assert.NotEmpty(t, gitIn(t, f.repoDir, "merge-base", "feature", "main"))
	assert.Contains(t, gitIn(t, f.repoDir, "ls-tree", "-r", "--name-only", "feature"), cmFile("cm1"))
	assert.Equal(t, contention+1, fetchCount(t, reader, f.worker, fetchReasonContention), "exactly one replay")
	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
}

// §4.2 test 2. The same with a write that changes nothing: the branch stays absent.
func TestBranchWorker_AnEmptyRepositoryThatGainsABranchKeepsANoOpAbsent(t *testing.T) {
	f := newRealServerNewBranch(t, "empty-gains-main-noop", nil)

	f.loop.lastPushAt = time.Now()
	deleted := configMapTargetEvent("never-written", "alice", newBranchTarget)
	deleted.Operation = "DELETE"
	deleted.Object = nil
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{deleted}, CommitMode: CommitModePerEvent}})
	require.Len(t, f.loop.pendingWrites, 1)

	simulateClientCommitOnDisk(t, f.repoDir, "main", "README.md", "main appeared\n")
	f.loop.pushPending()

	assert.Empty(t, f.loop.pendingWrites, "settled")
	assert.True(t, f.ref("feature").IsZero(), "nothing to publish: the branch stays absent")
}

// §4.2 test 3. Only a tag appears: not empty, and no default branch to start from. Nothing is
// orphaned, the work stays retained and the parent is reported missing, until a branch exists.
func TestBranchWorker_AnEmptyRepositoryThatGainsOnlyATagWaits(t *testing.T) {
	f := newRealServerNewBranch(t, "empty-gains-tag", nil)

	f.loop.lastPushAt = time.Now()
	liveWrite(f.loop, "cm1")
	require.Len(t, f.loop.pendingWrites, 1)

	tagOnly(t, f.repoDir)
	f.loop.pushPending()

	assert.True(t, f.ref("feature").IsZero(), "no orphan")
	assert.Len(t, f.loop.pendingWrites, 1, "the work is retained")
	o := f.observed()
	assert.Equal(t, ParentMissing, o.ParentState)
	assert.Empty(t, o.ParentRequested)

	main := branchOnDisk(t, f.repoDir, "main", "a branch at last\n")
	pastProbeDeadline(f.worker)
	f.loop.runParentProbe()
	tip := f.ref("feature")
	require.False(t, tip.IsZero())
	assert.Equal(t, []plumbing.Hash{main}, f.parentOf(tip))
}

// §4.2 test 4 and §4.3 test 4. A truly empty repository is still Unborn, and the first push still
// creates the root commit.
func TestBranchWorker_ATrulyEmptyRepositoryStillBootstraps(t *testing.T) {
	f := newRealServerNewBranch(t, "empty-throughout", nil)
	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	assert.Equal(t, ParentUnborn, f.observed().ParentState)

	liveWrite(f.loop, "cm1")

	tip := f.ref("feature")
	require.False(t, tip.IsZero())
	assert.Empty(t, f.parentOf(tip), "a root commit")
}

// §4.3 test 1. HEAD names a branch the repository does not carry. That is not an empty repository,
// so nothing may start an orphan there; the fetch and the refresh both report the parent missing.
func TestBranchWorker_ADanglingRemoteHeadIsMissingNotUnborn(t *testing.T) {
	f := newRealServerNewBranch(t, "dangling-head", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/master")
	})

	err := f.worker.ensureRepositoryInitialized(f.worker.ctx)
	require.ErrorIs(t, err, ErrDefaultBranchUnresolved)
	o := f.observed()
	assert.Equal(t, ParentMissing, o.ParentState, "the fetch: not Unborn")
	assert.Empty(t, o.ParentRequested)
	// Canonical git advertises neither a HEAD that resolves to nothing nor its symref, so the name
	// it dangles at (master) never reaches the client: the status says the remote has no default
	// branch. A server that does send the symref gets the name in the message.
	assert.Empty(t, o.ParentBranch)

	provider, err := f.worker.getGitProvider(f.worker.ctx)
	require.NoError(t, err)
	require.NoError(t, f.loop.refreshFromRemote(provider))
	assert.Equal(t, ParentMissing, f.observed().ParentState, "the refresh agrees")

	liveWrite(f.loop, "cm1")
	assert.True(t, f.ref("feature").IsZero(), "no orphan")
	assert.True(t, f.loop.lastPushAt.IsZero(), "nothing was pushed")

	// §4.3 test 3: repairing HEAD is enough.
	gitIn(t, f.repoDir, "symbolic-ref", "HEAD", "refs/heads/main")
	pastProbeDeadline(f.worker)
	liveWrite(f.loop, "cm2")
	tip := f.ref("feature")
	require.False(t, tip.IsZero())
	assert.Equal(t, []plumbing.Hash{f.ref("main")}, f.parentOf(tip))
}

// §4.3 test 2. A detached HEAD is resolved the way go-git resolves it, which this pins: master when
// it is at HEAD's commit, otherwise the alphabetically first branch that is.
func TestBranchWorker_ADetachedRemoteHeadFollowsGoGitsResolution(t *testing.T) {
	for name, tc := range map[string]struct {
		branches []string
		want     string
	}{
		"master wins":        {branches: []string{"trunk", "master"}, want: "master"},
		"first by name":      {branches: []string{"trunk", "develop"}, want: "develop"},
		"one branch at HEAD": {branches: []string{"trunk"}, want: "trunk"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealServerNewBranch(t, "detached-"+strings.ReplaceAll(name, " ", "-"), func(repoDir string) {
				first := simulateClientCommitOnDisk(t, repoDir, tc.branches[0], "README.md", "x\n")
				for _, b := range tc.branches[1:] {
					gitIn(t, repoDir, "branch", b, first.String())
				}
				gitIn(t, repoDir, "update-ref", "--no-deref", "HEAD", first.String())
			})

			require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
			o := f.observed()
			assert.Equal(t, ParentFound, o.ParentState)
			assert.Equal(t, tc.want, o.ParentBranch, "status names the branch go-git chose")
		})
	}
}
