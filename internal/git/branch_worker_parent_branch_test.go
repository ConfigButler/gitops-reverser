// SPDX-License-Identifier: Apache-2.0

package git

// spec.parentBranch names the branch a write branch the remote does not carry is created from.
// Omitted keeps the remote's default branch, which the tests in branch_worker_new_branch_test.go
// cover; these cover a configured one: it is followed while the write branch is absent, it must
// exist for the write branch to be created, and it stops mattering once the write branch exists.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// pushToRelease commits one file to `release` from outside, creating it from main when absent.
func (f *newBranchFixture) pushToRelease(file, content string) plumbing.Hash {
	f.t.Helper()
	return simulateClientCommitOnDisk(f.t, f.remote, "release", file, content)
}

func (f *newBranchFixture) commitParent(tip plumbing.Hash) plumbing.Hash {
	f.t.Helper()
	// Opened afresh: a handle opened before the pushes can miss a packfile they added.
	server, err := gogit.PlainOpen(strings.TrimPrefix(f.remote, "file://"))
	require.NoError(f.t, err)
	commit, err := server.CommitObject(tip)
	require.NoError(f.t, err)
	require.Len(f.t, commit.ParentHashes, 1)
	return commit.ParentHashes[0]
}

func TestSmartFetchFrom_ConfiguredParent(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	remoteURL := "file://" + filepath.Join(tempDir, "remote.git")
	createBareRepo(t, filepath.Join(tempDir, "remote.git"))
	simulateClientCommitOnDisk(t, remoteURL, "main", "README.md", "main\n")
	simulateClientCommitOnDisk(t, remoteURL, "release", "RELEASE.md", "release\n")
	simulateClientCommitOnDisk(t, remoteURL, "existing", "EXISTING.md", "existing\n")

	cases := []struct {
		name, target, parent string
		want                 plumbing.ReferenceName
		wantErr              error
	}{
		{name: "omitted falls back to the default branch", target: "feature", want: "refs/heads/main"},
		{name: "a configured parent replaces the default", target: "feature", parent: "release",
			want: "refs/heads/release"},
		{name: "a missing configured parent is refused", target: "feature", parent: "typo",
			wantErr: ErrParentBranchNotFound},
		{name: "an existing target ignores a missing parent", target: "existing", parent: "typo",
			want: "refs/heads/existing"},
		{name: "a parent equal to an absent target is refused", target: "feature", parent: "feature",
			wantErr: ErrParentBranchNotFound},
		{name: "a parent equal to an existing target is that branch", target: "existing", parent: "existing",
			want: "refs/heads/existing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initLocalRepo(t, filepath.Join(t.TempDir(), "local"), remoteURL, "")
			got, err := SmartFetchFrom(ctx, repo, plumbing.NewBranchReferenceName(tc.target), tc.parent, nil)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an empty repository refuses a configured parent", func(t *testing.T) {
		emptyURL := "file://" + filepath.Join(t.TempDir(), "empty.git")
		createBareRepo(t, emptyURL[len("file://"):])
		repo, _ := initLocalRepo(t, filepath.Join(t.TempDir(), "local"), emptyURL, "")
		_, err := SmartFetchFrom(ctx, repo, plumbing.NewBranchReferenceName("main"), "main", nil)
		require.ErrorIs(t, err, ErrParentBranchNotFound)

		got, err := SmartFetchFrom(ctx, repo, plumbing.NewBranchReferenceName("main"), "", nil)
		require.NoError(t, err)
		assert.Empty(t, got, "an omitted parent keeps the empty-repository bootstrap")
	})
}

// TestBranchWorker_NewBranchStartsFromTheConfiguredParent: the write branch is created on the
// configured parent's tip as publication saw it, even when the parent moved after the checkout.
func TestBranchWorker_NewBranchStartsFromTheConfiguredParent(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	f.pushToRelease("RELEASE.md", "release at R1\n")
	f.worker.SetParentBranch("release")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	branch, _ := f.localHead()
	require.Equal(
		t,
		plumbing.NewBranchReferenceName("release"),
		branch,
		"the absent write branch checks out its parent",
	)
	r2 := f.pushToRelease("RELEASE-2.md", "release at R2\n")

	liveWrite(loop, "cm1")

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	assert.Equal(t, r2, f.commitParent(tip), "created on the configured parent's current tip")
	assert.True(t, treeHas(t, f.server, tip, "RELEASE-2.md"))
}

// TestBranchWorker_MissingConfiguredParentWritesNothingUntilItExists: a configured parent the
// remote does not carry holds the write back, starts no orphan branch, and is reported; once it is
// pushed the next write creates the branch on it, carrying the held write first.
func TestBranchWorker_MissingConfiguredParentWritesNothingUntilItExists(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	f.worker.SetParentBranch("release")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	liveWrite(loop, "cm1")

	require.Len(t, loop.pendingWrites, 1, "the write is kept")
	assert.False(t, loop.pendingWrites[0].materialized, "but nothing was committed")
	assert.True(t, loop.lastPushAt.IsZero(), "and nothing was pushed")
	_, onRemote := f.featureOnRemote()
	assert.False(t, onRemote, "no orphan branch")
	observed, known := f.worker.LastRemoteObservation()
	require.True(t, known)
	assert.Equal(t, ParentMissing, observed.ParentState)

	r1 := f.pushToRelease("RELEASE.md", "now it exists\n")
	pastProbeDeadline(f.worker) // until then a known-missing parent is not fetched again
	liveWrite(loop, "cm2")

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote, "it recovers once the parent exists")
	assert.Equal(t, r1, f.commitParent(f.commitParent(tip)), "the held cm1, then cm2, on the parent's tip")
	observed, _ = f.worker.LastRemoteObservation()
	assert.Empty(t, observed.ParentState, "the write branch exists, so no parent is reported")
}

// TestBranchWorker_ParentBranchEqualToTheWriteBranch: legal when the branch exists, and then it is
// simply that branch; refused when it does not, because nothing names where to start it.
func TestBranchWorker_ParentBranchEqualToTheWriteBranch(t *testing.T) {
	t.Run("the branch exists", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		existing := simulateClientCommitOnDisk(t, f.remote, "feature", "FEATURE.md", "feature\n")
		f.worker.SetParentBranch("feature")
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()

		liveWrite(loop, "cm1")

		tip, _ := f.featureOnRemote()
		assert.Equal(t, existing, f.commitParent(tip))
	})

	t.Run("the branch is absent", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		f.worker.SetParentBranch("feature")
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()

		liveWrite(loop, "cm1")

		_, onRemote := f.featureOnRemote()
		assert.False(t, onRemote)
		observed, _ := f.worker.LastRemoteObservation()
		assert.Equal(t, ParentMissing, observed.ParentState)
		assert.Equal(t, "feature", observed.ParentBranch)
	})

	t.Run("an empty repository", func(t *testing.T) {
		worker, server, _ := setupCommitPushSplitWorkerOnEmptyRemote(t)
		worker.mapper = configMapMapper()
		createGitTarget(t, worker, newBranchTarget, newBranchFolder,
			&configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneAlways})
		worker.SetParentBranch("main")
		loop := newBranchWorkerEventLoop(worker, 0)
		defer loop.stopTimers()

		liveWrite(loop, "cm1")

		_, err := server.Reference(plumbing.NewBranchReferenceName("main"), true)
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "an explicit parent never bootstraps")
	})
}

// TestBranchWorker_AnExistingWriteBranchDoesNotFollowItsParent is the rule kept from Flux and Argo
// CD: the parent decides where the branch starts, and nothing after that.
func TestBranchWorker_AnExistingWriteBranchDoesNotFollowItsParent(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	f.pushToRelease("RELEASE.md", "release at R1\n")
	f.worker.SetParentBranch("release")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	liveWrite(loop, "cm1")
	first, onRemote := f.featureOnRemote()
	require.True(t, onRemote)

	f.pushToRelease("RELEASE-2.md", "release at R2\n")
	loop.lastPushAt = time.Time{}
	liveWrite(loop, "cm2")

	tip, _ := f.featureOnRemote()
	assert.Equal(t, first, f.commitParent(tip), "the write branch builds on itself")
	assert.False(t, treeHas(t, f.server, tip, "RELEASE-2.md"))
}

// TestBranchWorker_SetParentBranchInvalidatesTheBase: a changed parent is re-read on the next
// cycle; an unchanged one costs nothing.
func TestBranchWorker_SetParentBranchInvalidatesTheBase(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	f.warmOnParent()

	assert.False(t, f.worker.SetParentBranch(""), "unchanged")
	assert.True(t, f.worker.baseTrusted())

	assert.True(t, f.worker.SetParentBranch("release"))
	assert.Equal(t, "release", f.worker.ParentBranch())
	assert.False(t, f.worker.baseTrusted(), "the checkout holds the old parent")
}

// TestBranchWorker_AParentChangeRebuildsRetainedWrites: writes retained during the push cooldown
// were planned against the old parent, and an invalidated base does not reach them. The change is
// taken by the event loop, which refetches and replays before the next commit or push, so the
// branch starts on the new parent, or is not created at all when the new parent is missing.
func TestBranchWorker_AParentChangeRebuildsRetainedWrites(t *testing.T) {
	for _, parent := range []string{"release", "missing"} {
		t.Run(parent, func(t *testing.T) {
			f := newNewBranchFixture(t, map[string]string{cmFile("cm1"): cmDocument("cm1")})
			release := f.pushToRelease("RELEASE.md", "release\n")
			f.warmOnParent()
			loop := newBranchWorkerEventLoop(f.worker, 0)
			defer loop.stopTimers()

			loop.lastPushAt = time.Now()
			liveWrite(loop, "cm1")
			require.Len(t, loop.pendingWrites, 1)
			require.True(t, loop.pendingWrites[0].CommitSHA.IsZero(), "a no-op is retained during the cooldown")

			require.True(t, f.worker.SetParentBranch(parent))
			liveWrite(loop, "cm2")
			loop.pushPending()

			tip, exists := f.featureOnRemote()
			if parent == "missing" {
				assert.False(t, exists, "a missing configured parent prevents branch creation")
				assert.NotEmpty(t, loop.pendingWrites, "and the retained work is kept")
				return
			}
			require.True(t, exists)
			assert.Equal(t, release, f.commitParent(tip), "the first push uses the configured parent")
		})
	}
}

// TestBranchWorker_AParentChangeSurvivesAFailedRebuild: the rebuild a parent change asks for can
// fail before it resets anything, here on reading the GitProvider. The change must still be
// pending on the next attempt; taking it at the first try would leave the retained writes planned
// on the old parent, and the retry would publish from it, or create the branch although the new
// parent is missing.
func TestBranchWorker_AParentChangeSurvivesAFailedRebuild(t *testing.T) {
	for _, parent := range []string{"release", "missing"} {
		t.Run(parent, func(t *testing.T) {
			f := newNewBranchFixture(t, nil)
			release := f.pushToRelease("RELEASE.md", "release\n")
			f.warmOnParent()
			loop := newBranchWorkerEventLoop(f.worker, 0)
			defer loop.stopTimers()

			loop.lastPushAt = time.Now()
			liveWrite(loop, "cm1")
			require.Len(t, loop.pendingWrites, 1)
			require.True(t, f.worker.SetParentBranch(parent))

			inner := f.worker.Client.(client.WithWatch)
			f.worker.Client = interceptor.NewClient(inner, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if _, ok := obj.(*configv1alpha3.GitProvider); ok {
						return errors.New("transient API failure")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			loop.pushPending()
			require.Len(t, loop.pendingWrites, 1, "a failed rebuild keeps the work")

			f.worker.Client = inner
			loop.pushPending()

			tip, exists := f.featureOnRemote()
			if parent == "missing" {
				assert.False(t, exists, "the missing new parent still prevents branch creation on the retry")
				return
			}
			require.True(t, exists)
			assert.Equal(t, release, f.commitParent(tip), "the retry starts from the new parent")
		})
	}
}

// TestBranchWorker_ObservesTheParentOfAnAbsentBranch: every way the worker looks at the remote
// records, alongside the write branch's absence, the parent it would be created from, which
// status.remote.parent publishes. With spec.parentBranch omitted that names the remote's default
// branch. Once the write branch exists, there is no parent to report.
func TestBranchWorker_ObservesTheParentOfAnAbsentBranch(t *testing.T) {
	observed := func(f *newBranchFixture) RemoteObservation {
		t.Helper()
		o, known := f.worker.LastRemoteObservation()
		require.True(t, known)
		return o
	}

	t.Run("a fetch names the default branch when the parent is omitted", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		hashA := f.warmOnParent()
		o := observed(f)
		assert.Equal(t, ObservedByFetch, o.By)
		assert.Empty(t, o.Commit)
		assert.Equal(t, ParentFound, o.ParentState)
		assert.Equal(t, "main", o.ParentBranch)
		assert.Equal(t, hashA.String(), o.ParentCommit)
	})

	t.Run("an empty repository is unborn", func(t *testing.T) {
		worker, _, _ := setupCommitPushSplitWorkerOnEmptyRemote(t)
		require.NoError(t, worker.ensureRepositoryInitialized(worker.ctx))
		o, known := worker.LastRemoteObservation()
		require.True(t, known)
		assert.Equal(t, ParentUnborn, o.ParentState)
		assert.Empty(t, o.ParentBranch)
		assert.Empty(t, o.ParentCommit)
	})

	t.Run("a push that leaves the branch absent confirms the parent", func(t *testing.T) {
		f := newNewBranchFixture(t, map[string]string{cmFile("cm1"): cmDocument("cm1")})
		hashA := f.warmOnParent()
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()

		liveWrite(loop, "cm1")

		o := observed(f)
		assert.Equal(t, ObservedByPush, o.By)
		assert.Equal(t, "main", o.ParentBranch)
		assert.Equal(t, hashA.String(), o.ParentCommit)

		loop.lastPushAt = time.Time{}
		liveWrite(loop, "cm2")
		o = observed(f)
		assert.NotEmpty(t, o.Commit, "the branch exists now")
		assert.Empty(t, o.ParentBranch, "and its parent is no longer followed")
	})

	t.Run("a configured parent, present and missing", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		f.worker.SetParentBranch("release")
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()

		liveWrite(loop, "cm1")
		o := observed(f)
		assert.Equal(t, "release", o.ParentBranch)
		assert.Empty(t, o.ParentCommit, "the parent is not on the remote")
		assert.Equal(t, ParentMissing, o.ParentState)

		r1 := f.pushToRelease("RELEASE.md", "release\n")
		// The worker holds the write, so the bootstrap path would read its own clone: fetch directly.
		require.NoError(t, f.worker.syncWithRemote(f.worker.ctx, fetchReasonForcedRecheck))
		o = observed(f)
		assert.Equal(t, "release", o.ParentBranch)
		assert.Equal(t, r1.String(), o.ParentCommit)
		assert.Equal(t, ParentFound, o.ParentState)
	})
}
