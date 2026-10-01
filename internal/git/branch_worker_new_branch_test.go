// SPDX-License-Identifier: Apache-2.0

package git

// A write branch the remote does not carry yet is created from its parent, the remote's default
// branch. These tests pin the two halves of that contract on ONE warm worker, which is the case
// the cold-worker test (TestBranchWorker_CommitAndPushRequest_NewBranchStartsFromLatestMain) cannot
// reach: a worker that already trusts its checkout of the parent plans without fetching, so only
// the push can notice that the parent moved.
//
//   - Publication checks the parent: the first commit on the new branch descends from the parent's
//     tip as the push session advertised it, not from the checkout the plan ran on.
//   - Standby is quiet: a cycle that commits nothing leaves the write branch absent on the remote.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	newBranchTarget = "target-a"
	newBranchFolder = "live"
)

// newBranchFixture is a remote whose main is seeded and whose write branch `feature` is absent,
// a worker for `feature`, and a second clone that moves main from outside.
type newBranchFixture struct {
	t        *testing.T
	worker   *BranchWorker
	server   *git.Repository
	seed     *git.Repository
	seedTree *git.Worktree
	seedPath string
	remote   string
}

func newNewBranchFixture(t *testing.T, seedFiles map[string]string) *newBranchFixture {
	t.Helper()
	ctx := context.Background()
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote.git")
	remoteURL := "file://" + remotePath
	server := createBareRepo(t, remotePath)

	seedPath := filepath.Join(tempDir, "seed")
	seed, seedTree := initLocalRepo(t, seedPath, remoteURL, "main")
	f := &newBranchFixture{
		t: t, server: server, seed: seed, seedTree: seedTree, seedPath: seedPath, remote: remoteURL,
	}
	files := map[string]string{"README.md": "seed\n"}
	for name, content := range seedFiles {
		files[name] = content
	}
	f.pushToMain(files)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := &configv1alpha3.GitProvider{Spec: configv1alpha3.GitProviderSpec{URL: remoteURL}}
	provider.Name = "test-repo"
	provider.Namespace = "default"
	require.NoError(t, k8sClient.Create(ctx, provider))

	f.worker = NewBranchWorker(
		k8sClient, logr.Discard(), "test-repo", "default", "feature",
		RepoIdentity{URL: remoteURL}, nil, BranchWorkerLimits{},
	)
	f.worker.ctx = ctx
	f.worker.mapper = configMapMapper()
	t.Cleanup(func() { _ = os.RemoveAll(f.worker.repoRootPath()) })
	createGitTargetWithPruneMode(t, f.worker, newBranchTarget, newBranchFolder, configv1alpha3.PruneAlways)
	return f
}

// pushToMain commits files to main from outside the worker and returns the new tip.
func (f *newBranchFixture) pushToMain(files map[string]string) plumbing.Hash {
	f.t.Helper()
	for name, content := range files {
		full := filepath.Join(f.seedPath, name)
		require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(f.t, os.WriteFile(full, []byte(content), 0o600))
		_, err := f.seedTree.Add(name)
		require.NoError(f.t, err)
	}
	hash, err := f.seedTree.Commit("outside", &git.CommitOptions{
		Author: &object.Signature{Name: "Outside", Email: "outside@example.com", When: time.Now()},
	})
	require.NoError(f.t, err)
	require.NoError(f.t, f.seed.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{"refs/heads/main:refs/heads/main"},
	}))
	return hash
}

// mainTip is where main is on the remote.
func (f *newBranchFixture) mainTip() plumbing.Hash {
	f.t.Helper()
	ref, err := f.server.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(f.t, err)
	return ref.Hash()
}

// featureOnRemote reports where the write branch is on the remote, if anywhere.
func (f *newBranchFixture) featureOnRemote() (plumbing.Hash, bool) {
	f.t.Helper()
	ref, err := f.server.Reference(plumbing.NewBranchReferenceName("feature"), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, false
	}
	require.NoError(f.t, err)
	return ref.Hash(), true
}

// warmOnParent is the trusted fetch/reset a new worker performs: HEAD on main, base trusted.
func (f *newBranchFixture) warmOnParent() plumbing.Hash {
	f.t.Helper()
	require.NoError(f.t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	require.True(f.t, f.worker.baseTrusted())
	branch, hash := f.localHead()
	require.Equal(f.t, plumbing.NewBranchReferenceName("main"), branch, "an absent write branch checks out its parent")
	return hash
}

// noopResync runs a completed resync that finds nothing to change.
func (f *newBranchFixture) noopResync(loop *branchWorkerEventLoop, desired ...manifestanalyzer.DesiredResource) {
	f.t.Helper()
	req := &ResyncRequest{
		Desired:            desired,
		ResourceVersion:    "1",
		GitTargetName:      newBranchTarget,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	loop.handleQueueItem(WorkItem{Resync: req})
	result := <-req.Result
	require.NoError(f.t, result.Err)
	require.Zero(f.t, result.Stats.Created+result.Stats.Updated+result.Stats.Deleted, "the resync must be a no-op")
	require.Empty(f.t, loop.pendingWrites, "a no-op resync retains nothing")
}

func (f *newBranchFixture) localHead() (plumbing.ReferenceName, plumbing.Hash) {
	f.t.Helper()
	repo, err := git.PlainOpen(f.worker.repoPath())
	require.NoError(f.t, err)
	branch, hash, err := GetCurrentBranch(repo)
	require.NoError(f.t, err)
	return branch, hash
}

// liveWrite sends one live ConfigMap event through the loop's commit window.
func liveWrite(loop *branchWorkerEventLoop, name string) {
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent(name, "alice", newBranchTarget)},
		CommitMode: CommitModePerEvent,
	}})
}

// treeHas reports whether the commit's tree carries the file.
func treeHas(t *testing.T, repo *git.Repository, commit plumbing.Hash, file string) bool {
	t.Helper()
	c, err := repo.CommitObject(commit)
	require.NoError(t, err)
	_, err = c.File(file)
	return err == nil
}

func cmFile(name string) string { return newBranchFolder + "/default/configmaps/" + name + ".yaml" }

// cmDocument is the document the worker writes for configMapTargetEvent(name, ...).
func cmDocument(name string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name +
		"\n  namespace: default\ndata:\n  key: " + name + "\n"
}

// TestBranchWorker_LiveWriteOnAbsentBranchStartsFromTheParentsCurrentTip is the defect: a warm
// worker trusts its checkout of the parent at A, the parent moves to B, and a live write must
// still create the write branch on B.
//
// The two local states differ in what the cycle records as its push root, which is what decides
// whether the push primitive's own parent check is exercised. Both are inspected rather than
// assumed.
func TestBranchWorker_LiveWriteOnAbsentBranchStartsFromTheParentsCurrentTip(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *newBranchFixture, loop *branchWorkerEventLoop)
	}{
		{
			name:  "HEAD on the parent after a trusted fetch",
			setup: func(f *newBranchFixture, _ *branchWorkerEventLoop) { f.warmOnParent() },
		},
		{
			name: "a local write branch left by a no-op resync",
			setup: func(f *newBranchFixture, loop *branchWorkerEventLoop) {
				f.noopResync(loop)
				branch, _ := f.localHead()
				require.Equal(t, plumbing.NewBranchReferenceName("feature"), branch,
					"a no-op resync prepares the local write branch before it learns there is nothing to do")
				_, onRemote := f.featureOnRemote()
				require.False(t, onRemote)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNewBranchFixture(t, nil)
			loop := newBranchWorkerEventLoop(f.worker, 0)
			defer loop.stopTimers()

			hashA := f.mainTip()
			tc.setup(f, loop)
			_, local := f.localHead()
			require.Equal(t, hashA, local, "the worker checked out A")
			require.True(t, f.worker.baseTrusted(), "and trusts it, so the next plan does not fetch")

			hashB := f.pushToMain(map[string]string{"LATEST.md": "from main at B\n"})

			// Hold the push so the cycle's recorded root can be read before it is sent.
			loop.lastPushAt = time.Now()
			liveWrite(loop, "cm1")
			require.Len(t, loop.pendingWrites, 1)
			assert.Equal(t, plumbing.NewBranchReferenceName("main"), f.worker.pushCycleRootBranch,
				"the cycle is rooted on the parent the write branch is created from")
			assert.Equal(t, hashA, f.worker.pushCycleRootHash, "planned on the stale checkout, as designed")

			loop.pushPending()
			require.Empty(t, loop.pendingWrites, "the push succeeded")

			tip, onRemote := f.featureOnRemote()
			require.True(t, onRemote, "the write created the branch")
			commit, err := f.server.CommitObject(tip)
			require.NoError(t, err)
			require.Len(t, commit.ParentHashes, 1)
			assert.Equal(t, hashB, commit.ParentHashes[0], "the new branch starts from the parent's current tip")
			assert.True(t, treeHas(t, f.server, tip, "LATEST.md"), "and carries B's change")
			assert.True(t, treeHas(t, f.server, tip, cmFile("cm1")), "and the captured edit")
		})
	}
}

// TestBranchWorker_InSyncSnapshotsOnAbsentBranchCreateNothing is standby across parent movement:
// two in-sync snapshots, with an unrelated change to main between them, push nothing at all.
func TestBranchWorker_InSyncSnapshotsOnAbsentBranchCreateNothing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newNewBranchFixture(t, nil)
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	f.noopResync(loop)
	f.pushToMain(map[string]string{"UNRELATED.md": "outside the folder\n"})
	f.noopResync(loop)

	_, onRemote := f.featureOnRemote()
	assert.False(t, onRemote, "an in-sync target creates no branch")
	assert.True(t, loop.lastPushAt.IsZero(), "and attempts no push")
	assert.Zero(t, fetchCount(t, reader, f.worker, fetchReasonContention))
}

// TestBranchWorker_ResyncOnAbsentBranchDiffsAgainstTheMovedParent is the other side: once main
// moves so the cluster differs, the snapshot diffs against the new main and creates the branch
// from it.
func TestBranchWorker_ResyncOnAbsentBranchDiffsAgainstTheMovedParent(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	f.noopResync(loop)
	moved := f.pushToMain(map[string]string{cmFile("stale"): cmDocument("stale")})

	req := &ResyncRequest{
		ResourceVersion:    "2",
		GitTargetName:      newBranchTarget,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	loop.handleQueueItem(WorkItem{Resync: req})
	result := <-req.Result
	require.NoError(t, result.Err)
	assert.Equal(t, 1, result.Stats.Deleted, "the document main gained is not in the cluster")
	loop.pushPending()
	require.Empty(t, loop.pendingWrites)

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	commit, err := f.server.CommitObject(tip)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.Hash{moved}, commit.ParentHashes)
	assert.False(t, treeHas(t, f.server, tip, cmFile("stale")))
}

// TestBranchWorker_NoDiffLiveWriteOnAbsentBranchStaysOnStandby is "keep normal deployments
// quiet". A live write the parent already holds commits nothing, and its push must not create the
// write branch merely to record that nothing changed. It still reaches the remote: "already
// present" is a claim only the advertisement can settle.
func TestBranchWorker_NoDiffLiveWriteOnAbsentBranchStaysOnStandby(t *testing.T) {
	t.Run("the parent already holds the document", func(t *testing.T) {
		f := newNewBranchFixture(t, map[string]string{cmFile("cm1"): cmDocument("cm1")})
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		f.warmOnParent()

		liveWrite(loop, "cm1")

		require.Empty(t, loop.pendingWrites, "the no-diff write was published")
		assert.False(t, loop.lastPushAt.IsZero(), "it reached the remote to confirm")
		_, onRemote := f.featureOnRemote()
		assert.False(t, onRemote, "and the write branch stays absent")
		observed, known := f.worker.LastRemoteObservation()
		require.True(t, known)
		assert.Empty(t, observed.Commit, "the push observed no write branch")
	})

	t.Run("the parent gained the document after the plan", func(t *testing.T) {
		reader, err := telemetry.InitTestExporter()
		require.NoError(t, err)

		f := newNewBranchFixture(t, nil)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		f.warmOnParent()

		loop.lastPushAt = time.Now()
		liveWrite(loop, "cm1")
		require.Len(t, loop.pendingWrites, 1)
		require.False(t, loop.pendingWrites[0].CommitSHA.IsZero(), "planned against A, the write is a commit")

		// The reconciler applied main, and main now holds exactly what the cluster holds.
		f.pushToMain(map[string]string{cmFile("cm1"): cmDocument("cm1")})
		loop.pushPending()

		require.Empty(t, loop.pendingWrites)
		_, onRemote := f.featureOnRemote()
		assert.False(t, onRemote, "replayed against the new parent, nothing is left to publish")
		assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonContention),
			"the publication check is what caught the moved parent")
	})
}

// TestBranchWorker_NewBranchPushCostsNoExtraFetchWhenTheParentHolds pins the price of the check:
// it is read from the advertisement the push session opens anyway.
func TestBranchWorker_NewBranchPushCostsNoExtraFetchWhenTheParentHolds(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newNewBranchFixture(t, nil)
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	hashA := f.warmOnParent()

	liveWrite(loop, "cm1")

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	commit, err := f.server.CommitObject(tip)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.Hash{hashA}, commit.ParentHashes)
	for _, reason := range []string{fetchReasonPublication, fetchReasonContention, fetchReasonPushFailureProbe} {
		assert.Zero(t, fetchCount(t, reader, f.worker, reason), "no %s fetch", reason)
	}
}

// TestBranchWorker_ParentMovesWhileTheWindowIsOpen covers the window between planning and
// publication from the other end: the parent moves while the commit window is still collecting.
func TestBranchWorker_ParentMovesWhileTheWindowIsOpen(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	defer loop.stopTimers()
	f.warmOnParent()

	liveWrite(loop, "cm1")
	require.NotNil(t, loop.openWindow, "the window is still collecting")
	hashB := f.pushToMain(map[string]string{"LATEST.md": "from main at B\n"})

	require.True(t, loop.finalizeOpenWindow())
	loop.maybeSchedulePush()

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	commit, err := f.server.CommitObject(tip)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.Hash{hashB}, commit.ParentHashes)
}

// TestBranchWorker_DeletedWriteBranchIsRecreatedFromTheParent is the return to standby after a
// review: the write branch is merged and deleted, main advances, and the next edit through the
// SAME worker recreates the branch on the new main with only the new edit on top.
func TestBranchWorker_DeletedWriteBranchIsRecreatedFromTheParent(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	f.warmOnParent()

	liveWrite(loop, "cm1")
	_, onRemote := f.featureOnRemote()
	require.True(t, onRemote)

	// Merged by content and deleted, the way a squash merge leaves it.
	merged := f.pushToMain(map[string]string{cmFile("cm1"): cmDocument("cm1"), "MERGED.md": "squashed\n"})
	require.NoError(t, f.server.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature")))

	loop.lastPushAt = time.Time{}
	liveWrite(loop, "cm2")
	require.Empty(t, loop.pendingWrites)

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote, "the next edit recreates the branch")
	commit, err := f.server.CommitObject(tip)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.Hash{merged}, commit.ParentHashes, "on the parent's current tip")
	assert.True(t, treeHas(t, f.server, tip, "MERGED.md"))
	assert.True(t, treeHas(t, f.server, tip, cmFile("cm2")))
}

// TestBranchWorker_NewBranchIsNotPublishedOnAnUncheckedParent covers the two ways the check can
// fail to settle: a parent that keeps moving exhausts the bounded retries, and a remote that cannot
// be reached answers nothing. Either way the work stays retained and the write branch uncreated.
func TestBranchWorker_NewBranchIsNotPublishedOnAnUncheckedParent(t *testing.T) {
	t.Run("the parent keeps moving", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		f.warmOnParent()

		// Every reset onto the remote is immediately outrun by another outside commit, so each
		// attempt is checked again and each one finds the parent moved.
		original := syncToRemoteFn
		t.Cleanup(func() { syncToRemoteFn = original })
		moves := 0
		syncToRemoteFn = func(
			ctx context.Context, repo *git.Repository, branch plumbing.ReferenceName, auth []gitclient.Option,
		) (*PullReport, error) {
			report, err := original(ctx, repo, branch, auth)
			moves++
			f.pushToMain(map[string]string{"MOVING.md": time.Now().String() + string(rune('a'+moves))})
			return report, err
		}

		loop.lastPushAt = time.Now()
		liveWrite(loop, "cm1")
		f.pushToMain(map[string]string{"FIRST.md": "moved before the first attempt\n"})
		loop.pushPending()

		assert.Len(t, loop.pendingWrites, 1, "the work is retained")
		_, onRemote := f.featureOnRemote()
		assert.False(t, onRemote, "and nothing unchecked was published")
		assert.Positive(t, moves)
	})

	t.Run("the remote cannot be reached", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		f.warmOnParent()

		original := pushAtomicFn
		t.Cleanup(func() { pushAtomicFn = original })
		pushAtomicFn = func(
			context.Context, *git.Repository, plumbing.Hash, plumbing.ReferenceName, []gitclient.Option,
		) (PushOutcome, error) {
			return PushOutcome{}, errors.New("connection refused")
		}

		liveWrite(loop, "cm1")

		assert.Len(t, loop.pendingWrites, 1, "the work is retained for retry")
		_, onRemote := f.featureOnRemote()
		assert.False(t, onRemote)
	})
}
