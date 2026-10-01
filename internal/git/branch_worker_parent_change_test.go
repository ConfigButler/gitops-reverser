// SPDX-License-Identifier: Apache-2.0

package git

// A parent change reaches a live worker from the reconcile goroutine while the event loop may be in
// the middle of a fetch or a push. These pin the rules that make that safe: every operation uses
// one snapshot of the parent configuration, trust is scoped to the snapshot's generation, and a
// push is admitted only when its root was chosen under the current generation. A push already
// admitted is not recalled.

import (
	"context"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// allFetchReasons is every series a fetch can be counted under.
var allFetchReasons = []string{
	fetchReasonBootstrap, fetchReasonPublication, fetchReasonRecovery, fetchReasonContention,
	fetchReasonPushFailureProbe, fetchReasonRefresh,
}

func totalFetches(t *testing.T, reader *sdkmetric.ManualReader, worker *BranchWorker) int64 {
	t.Helper()
	var total int64
	for _, reason := range allFetchReasons {
		total += fetchCount(t, reader, worker, reason)
	}
	return total
}

// onSync runs hook after the n-th sync (1-based) the worker makes, inside that fetch.
func onSync(t *testing.T, n int, hook func()) {
	t.Helper()
	original := syncToRemoteFn
	calls := 0
	syncToRemoteFn = func(ctx context.Context, repo *gogit.Repository, target plumbing.ReferenceName,
		parent string, auth []gitclient.Option) (*PullReport, error) {
		report, err := original(ctx, repo, target, parent, auth)
		calls++
		if calls == n {
			hook()
		}
		return report, err
	}
	t.Cleanup(func() { syncToRemoteFn = original })
}

// pushRoots records the root of every push attempt, and runs hook inside the first one, before it
// reaches the remote.
func pushRoots(t *testing.T, hook func()) *[]plumbing.ReferenceName {
	t.Helper()
	original := pushAtomicFn
	var roots []plumbing.ReferenceName
	pushAtomicFn = func(ctx context.Context, repo *gogit.Repository, rootHash plumbing.Hash,
		rootBranch plumbing.ReferenceName, auth []gitclient.Option) (PushOutcome, error) {
		roots = append(roots, rootBranch)
		if len(roots) == 1 && hook != nil {
			hook()
		}
		return original(ctx, repo, rootHash, rootBranch, auth)
	}
	t.Cleanup(func() { pushAtomicFn = original })
	return &roots
}

// pastProbeDeadline moves the worker's clock past any parent-probe deadline, so a test can stand in
// for the time a held-back parent waits before it is looked for again.
func pastProbeDeadline(w *BranchWorker) {
	w.clock = func() time.Time { return time.Now().Add(time.Hour) }
}

// observations collects every observation the worker reports.
func observations(f *newBranchFixture) *[]RemoteObservation {
	var seen []RemoteObservation
	f.worker.remoteReporter = func(o RemoteObservation) { seen = append(seen, o) }
	return &seen
}

// Test 1: a change that lands inside a fetch. The fetch finishes under the old snapshot, so its
// observation names the parent it used, and the push that follows is not admitted on that root.
func TestBranchWorker_AParentChangeDuringAFetchIsNotPushedOnTheOldParent(t *testing.T) {
	t.Run("the initial fetch", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		release := f.pushToRelease("RELEASE.md", "release\n")
		seen := observations(f)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		onSync(t, 1, func() { f.worker.SetParentBranch("release") })

		liveWrite(loop, "cm1")

		tip, onRemote := f.featureOnRemote()
		require.True(t, onRemote)
		assert.Equal(t, release, f.commitParent(tip), "the first commit sits on the new parent")
		require.NotEmpty(t, *seen)
		assert.Equal(t, "main", (*seen)[0].ParentBranch, "the fetch reports the parent it actually used")
	})

	t.Run("a contention fetch", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		release := f.pushToRelease("RELEASE.md", "release\n")
		f.warmOnParent()
		f.pushToMain(map[string]string{"MOVED.md": "main moved\n"}) // the push is refused: contention
		seen := observations(f)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		onSync(t, 1, func() { f.worker.SetParentBranch("release") })

		liveWrite(loop, "cm1")

		tip, onRemote := f.featureOnRemote()
		require.True(t, onRemote)
		assert.Equal(t, release, f.commitParent(tip))
		require.NotEmpty(t, *seen)
		assert.Equal(t, "main", (*seen)[0].ParentBranch, "the contention fetch reports the parent it used")
	})
}

// Test 2, the review's reproduction: a change inside a no-op push that settles as PushNoBranch.
// The push sets trust after the change, and the remembered parent is the old one; neither may
// root the next real write.
func TestBranchWorker_AParentChangeDuringANoOpPushIsTakenByTheNextWrite(t *testing.T) {
	f := newNewBranchFixture(t, map[string]string{cmFile("cm1"): cmDocument("cm1")})
	release := f.pushToRelease("RELEASE.md", "release\n")
	f.warmOnParent()
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	roots := pushRoots(t, func() { f.worker.SetParentBranch("release") })

	liveWrite(loop, "cm1") // already in Git: settles without creating the branch
	_, onRemote := f.featureOnRemote()
	require.False(t, onRemote)
	require.Len(t, *roots, 1)

	loop.lastPushAt = time.Time{}
	liveWrite(loop, "cm2")

	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	assert.Equal(t, release, f.commitParent(tip), "the next real write lands on the new parent")
}

// Test 3: a change just before admission, with retained writes. No push leaves on the old root.
func TestBranchWorker_AParentChangeBeforeAdmissionReplaysOntoTheNewParent(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	release := f.pushToRelease("RELEASE.md", "release\n")
	f.warmOnParent()
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	loop.lastPushAt = time.Now() // cooldown: the write is retained
	liveWrite(loop, "cm1")
	require.Len(t, loop.pendingWrites, 1)

	changed := false
	beforePushAdmission = func() {
		if !changed {
			changed = true
			f.worker.SetParentBranch("release")
		}
	}
	t.Cleanup(func() { beforePushAdmission = nil })
	roots := pushRoots(t, nil)

	loop.pushPending()

	require.Empty(t, loop.pendingWrites, "the replay published")
	assert.NotContains(t, *roots, plumbing.NewBranchReferenceName("main"), "no push left on the old root")
	tip, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	assert.Equal(t, release, f.commitParent(tip))
}

// Test 4: a change after admission is in flight. The push that creates the branch from the old
// parent is not recalled; the branch then exists, and an existing branch does not follow its
// parent, so later writes build on it with no attempt to re-root it.
func TestBranchWorker_AParentChangeAfterAdmissionIsNotRecalled(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newNewBranchFixture(t, nil)
	f.pushToRelease("RELEASE.md", "release\n")
	mainTip := f.warmOnParent()
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	roots := pushRoots(t, func() { f.worker.SetParentBranch("release") })

	liveWrite(loop, "cm1")

	first, onRemote := f.featureOnRemote()
	require.True(t, onRemote)
	assert.Equal(t, mainTip, f.commitParent(first), "the admitted push created the branch from main")

	fetches := totalFetches(t, reader, f.worker)
	loop.lastPushAt = time.Time{}
	liveWrite(loop, "cm2")

	tip, _ := f.featureOnRemote()
	assert.Equal(t, first, f.commitParent(tip), "later writes go to the existing branch")
	assert.Equal(t, fetches, totalFetches(t, reader, f.worker), "and nothing re-reads the parent")
	assert.Len(t, *roots, 2)
	assert.Equal(t, plumbing.NewBranchReferenceName("feature"), (*roots)[1])
}

// Test 5: the new parent is missing, in cases 1 and 3. The branch is never created, the work stays
// retained, and it lands once the parent exists.
func TestBranchWorker_AParentChangeToAMissingParentKeepsTheWork(t *testing.T) {
	t.Run("during a fetch", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		onSync(t, 1, func() { f.worker.SetParentBranch("release") })

		liveWrite(loop, "cm1")

		_, onRemote := f.featureOnRemote()
		require.False(t, onRemote, "no branch on the old parent")
		require.NotEmpty(t, loop.pendingWrites, "the work is retained")

		release := f.pushToRelease("RELEASE.md", "now it exists\n")
		pastProbeDeadline(f.worker)
		loop.runParentProbe()
		tip, onRemote := f.featureOnRemote()
		require.True(t, onRemote, "the probe published the retained work")
		assert.Equal(t, release, f.commitParent(tip))
	})

	t.Run("before admission", func(t *testing.T) {
		f := newNewBranchFixture(t, nil)
		f.warmOnParent()
		loop := newBranchWorkerEventLoop(f.worker, 0)
		defer loop.stopTimers()
		loop.lastPushAt = time.Now()
		liveWrite(loop, "cm1")
		require.Len(t, loop.pendingWrites, 1)
		beforePushAdmission = func() { f.worker.SetParentBranch("release") }
		t.Cleanup(func() { beforePushAdmission = nil })

		loop.pushPending()

		_, onRemote := f.featureOnRemote()
		require.False(t, onRemote)
		require.Len(t, loop.pendingWrites, 1, "the work is retained")

		beforePushAdmission = nil
		release := f.pushToRelease("RELEASE.md", "now it exists\n")
		pastProbeDeadline(f.worker)
		loop.runParentProbe()
		tip, onRemote := f.featureOnRemote()
		require.True(t, onRemote)
		assert.Equal(t, release, f.commitParent(tip))
	})
}

// Test 6: a parent cannot move an existing write branch, so a change costs it nothing: no fetch,
// and the next push goes to the write branch.
func TestBranchWorker_AParentChangeCostsAnExistingWriteBranchNothing(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newNewBranchFixture(t, nil)
	f.pushToRelease("RELEASE.md", "release\n")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	liveWrite(loop, "cm1")
	first, onRemote := f.featureOnRemote()
	require.True(t, onRemote)

	require.True(t, f.worker.SetParentBranch("release"))
	fetches := totalFetches(t, reader, f.worker)
	roots := pushRoots(t, nil)
	loop.lastPushAt = time.Time{}
	liveWrite(loop, "cm2")

	assert.Equal(t, fetches, totalFetches(t, reader, f.worker), "zero extra fetches")
	require.Len(t, *roots, 1, "one push")
	assert.Equal(t, plumbing.NewBranchReferenceName("feature"), (*roots)[0])
	tip, _ := f.featureOnRemote()
	assert.Equal(t, first, f.commitParent(tip))
}
