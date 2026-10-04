// SPDX-License-Identifier: Apache-2.0

package git

// Work decided while the parent branch is missing is an obligation, not an observation: it stays in
// the log until it is published, so a transient failure after the parent reappears cannot strand it,
// and it is driven by the worker on its own deadline, whatever the refresh configuration. These pin
// the obligation, that nothing decided meanwhile is dropped, and the probe budget.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// testClock is a clock the test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func useTestClock(w *BranchWorker) *testClock {
	c := &testClock{now: time.Now()}
	w.clock = c.Now
	return c
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// failNextSync makes the next sync the worker attempts fail as a network error would.
func failNextSync(t *testing.T) {
	t.Helper()
	original := syncToRemoteFn
	failed := false
	syncToRemoteFn = func(ctx context.Context, repo *gogit.Repository, target plumbing.ReferenceName,
		parent string, auth []gitclient.Option) (*PullReport, error) {
		if !failed {
			failed = true
			return nil, errors.New("connection reset by peer")
		}
		return original(ctx, repo, target, parent, auth)
	}
	t.Cleanup(func() { syncToRemoteFn = original })
}

// Test 1. Writes retained while the parent is missing are published once it exists, through the
// worker's own deadline, even when the first attempt after the parent reappears fails. No cluster
// edit is involved, and it works with the refresher on or off.
func TestParentRecovery_RetainedWritesLandAfterAFailedFirstAttempt(t *testing.T) {
	for name, viaRefresh := range map[string]bool{"refresh off": false, "refresh on": true} {
		t.Run(name, func(t *testing.T) {
			f := newRealServerNewBranch(
				t,
				"recovery-retained-"+map[bool]string{false: "timer", true: "refresh"}[viaRefresh],
				func(repoDir string) {
					simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
					gitIn(t, repoDir, "branch", "release", "main")
				},
			)
			clock := useTestClock(f.worker)
			f.worker.SetParentBranch("release")
			require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
			deadline := func() {
				if viaRefresh {
					f.loop.handleRefreshRequest(&RefreshRequest{Target: refreshTarget(), MaxAge: time.Nanosecond})
					return
				}
				f.loop.runParentProbe() // what the probe timer does
			}

			f.loop.lastPushAt = time.Now()
			liveWrite(f.loop, "cm1")
			require.Len(t, f.loop.pendingWrites, 1)
			gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release") // the parent goes away
			f.loop.pushPending()
			require.Len(t, f.loop.pendingWrites, 1, "kept")
			open, found := f.worker.ParentRecovery()
			require.True(t, open)
			require.False(t, found)

			if viaRefresh {
				before := f.mark()
				deadline()
				assert.Zero(t, f.mark().since(before).connections(), "a refresh tick before the deadline costs nothing")
			}

			release := simulateClientCommitOnDisk(t, f.repoDir, "release", "RELEASE.md", "back\n")
			failNextSync(t)
			clock.advance(retryMaxBackoff)
			deadline()

			assert.True(t, f.ref("feature").IsZero(), "the first attempt failed")
			require.Len(t, f.loop.pendingWrites, 1, "and the work is still held")
			open, found = f.worker.ParentRecovery()
			require.True(t, open, "the obligation outlives a failed attempt")
			assert.True(t, found)

			clock.advance(retryMaxBackoff)
			deadline()

			tip := f.ref("feature")
			require.False(t, tip.IsZero(), "the next deadline published it, with no cluster edit")
			assert.Equal(t, []plumbing.Hash{release}, f.parentOf(tip))
			assert.Empty(t, f.loop.pendingWrites)
			open, _ = f.worker.ParentRecovery()
			assert.False(t, open, "published: nothing is owed")
		})
	}
}

func refreshTarget() itypes.ResourceReference {
	return itypes.NewResourceReference(newBranchTarget, "default")
}

// Test 2. Writes for two targets on one worker, decided while the parent is missing, wait in the log
// with their authors and collections, and so does a resync decided meanwhile, whose caller is told
// it could not be applied yet. The probe that finds the parent publishes all of it, and nothing is
// owed after that: no snapshot has to re-derive anything.
func TestParentRecovery_WritesDecidedWhileTheParentIsMissingAreKept(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-kept-writes", seedMain(t))
	clock := useTestClock(f.worker)
	createPlainGitTarget(t, f.worker, "other-target", "other")
	f.worker.SetParentBranch("release")
	loop := f.loop

	write := func(target, name string) {
		event := configMapTargetEvent(name, "alice", target)
		loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	}
	write(newBranchTarget, "a1")
	write("other-target", "b1")
	require.Len(t, loop.pendingWrites, 2, "both are kept")
	open, _ := f.worker.ParentRecovery()
	require.True(t, open)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{desiredCM("a1", "a1"), desiredCM("a2", "blue")},
		ResourceVersion:    "1",
		GitTargetName:      newBranchTarget,
		GitTargetNamespace: "default",
		Scope: &ResyncScope{
			Collection: itypes.CollectionKey{Resource: "configmaps", Namespace: "default"},
		},
		Result: make(chan ResyncResult, 1),
	}
	loop.applyResync(req)
	loop.endWake(0)
	require.ErrorIs(t, (<-req.Result).Err, ErrParentBranchNotFound, "its caller hears it could not be applied yet")
	require.Len(t, loop.pendingWrites, 3, "and the resync waits in its place")

	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(retryMaxBackoff)
	loop.runParentProbe()

	assert.Empty(t, loop.pendingWrites, "all of it was published")
	open, _ = f.worker.ParentRecovery()
	assert.False(t, open, "and nothing is owed")
	names := gitIn(t, f.repoDir, "ls-tree", "-r", "--name-only", "refs/heads/feature")
	for _, name := range []string{"a1", "b1", "a2"} {
		assert.Contains(t, names, name)
	}
}

// Test 4. However many targets share the worker, and however often refreshes tick and live writes
// arrive, a parent that stays missing is asked about at most once per deadline of the shared
// schedule, and never fetched. That holds with work retained too: there, every write first wants to
// rebuild the retained writes, which is a fetch the deadline must hold back as well.
func TestParentRecovery_ProbeBudgetIsPerWorker(t *testing.T) {
	for name, retained := range map[string]bool{"decided writes": false, "retained writes": true} {
		t.Run(name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)
			slug := map[bool]string{false: "recovery-probe-budget", true: "recovery-probe-budget-retained"}[retained]
			f := newRealServerNewBranch(t, slug, func(repoDir string) {
				simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
				gitIn(t, repoDir, "branch", "release", "main")
			})
			clock := useTestClock(f.worker)
			createPlainGitTarget(t, f.worker, "second", "second")
			createPlainGitTarget(t, f.worker, "third", "third")
			targets := []string{newBranchTarget, "second", "third"}

			if retained {
				f.worker.SetParentBranch("release")
				require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
				f.loop.lastPushAt = time.Now()
				liveWrite(f.loop, "first")
				require.Len(t, f.loop.pendingWrites, 1)
				gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release")
				f.loop.pushPending() // the push that finds the parent gone
				require.False(t, f.loop.checkoutCurrent(), "the retained writes wait for a rebuild")
			} else {
				f.worker.SetParentBranch("missing")
				liveWrite(f.loop, "first") // the one fetch that finds the parent missing
			}
			open, _ := f.worker.ParentRecovery()
			require.True(t, open)

			fetches := totalFetches(t, reader, f.worker)
			before := f.mark()
			probes := 0
			const window = 10 * time.Minute
			for elapsed := time.Duration(0); elapsed < window; elapsed += 2 * time.Second {
				clock.advance(2 * time.Second)
				if f.loop.probeDue() {
					probes++
					f.loop.runParentProbe() // the timer
				}
				for _, target := range targets {
					f.loop.handleRefreshRequest(&RefreshRequest{
						Target: itypes.NewResourceReference(target, "default"), MaxAge: time.Nanosecond,
					})
					event := configMapTargetEvent("cm-"+target, "alice", target)
					f.loop.handleQueueItem(
						WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}},
					)
				}
			}

			// 10s, then doubling to the 5m cap: deadlines at 10s, 30s, 70s, 150s and 310s.
			assert.Equal(t, 5, probes)
			assert.LessOrEqual(t, f.mark().since(before).connections(), int64(probes),
				"one advertisement per deadline, whatever else happens")
			assert.Equal(t, fetches, totalFetches(t, reader, f.worker), "and no fetch at all")
		})
	}
}

// A second outage while the first outage's writes still wait to be published. The write decided
// during it is kept with them, and the obligation stays open until all of it is published.
func TestParentRecovery_ASecondOutageKeepsItsWritesToo(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-second-outage", func(dir string) {
		simulateClientCommitOnDisk(t, dir, "main", "README.md", "main\n")
	})
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")

	liveWrite(f.loop, "cm1") // kept: release does not exist
	gitIn(t, f.repoDir, "branch", "release", "main")
	failNextPushes(t, 1)
	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe() // finds the parent; the push fails
	require.True(t, f.ref("feature").IsZero())
	require.Len(t, f.loop.pendingWrites, 1)

	gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release") // the second outage
	liveWrite(f.loop, "cm2")
	require.Len(t, f.loop.pendingWrites, 2, "the write decided during it is kept with the first")
	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe() // the push finds the parent gone again
	require.Len(t, f.loop.pendingWrites, 2)
	open, found := f.worker.ParentRecovery()
	require.True(t, open)
	require.False(t, found)

	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe()

	assert.Empty(t, f.loop.pendingWrites)
	files := gitIn(t, f.repoDir, "ls-tree", "-r", "--name-only", "refs/heads/feature")
	assert.Contains(t, files, "cm1")
	assert.Contains(t, files, "cm2")
	open, _ = f.worker.ParentRecovery()
	assert.False(t, open)
}

// Test 5. An unresolved default branch, once repaired, recovers through the same path: the probe
// finds it and publishes the write kept meanwhile.
func TestParentRecovery_ARepairedRemoteHeadPublishesTheKeptWrite(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-dangling-head", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/master")
	})
	clock := useTestClock(f.worker)

	liveWrite(f.loop, "cm1")
	assert.True(t, f.ref("feature").IsZero())
	require.Len(t, f.loop.pendingWrites, 1, "kept")

	gitIn(t, f.repoDir, "symbolic-ref", "HEAD", "refs/heads/main")
	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe()

	assert.False(t, f.ref("feature").IsZero(), "published")
	open, _ := f.worker.ParentRecovery()
	assert.False(t, open)
}

func TestParentRecovery_AParentChangeMakesTheProbeDue(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()
	liveWrite(loop, "cm1")
	require.False(t, loop.probeDue())
	require.True(t, f.worker.awaitingParentProbe())

	f.worker.SetParentBranch("other")

	assert.True(t, loop.probeDue())
	assert.False(t, f.worker.awaitingParentProbe(), "a new parent is looked for at once")
}

// A CommitRequest that rides a write held back by a missing parent stays held through the whole
// recovery, however long it takes, and resolves Committed when the work lands. This runs the real
// event loop on its own goroutine; the clock stands in for a recovery far longer than the bound the
// controller used to fail requests on (attachTimeout + maxDuration + 120s).
func TestParentRecovery_AHeldCommitRequestIsCommittedAfterALongRecovery(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-held-commit-request", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "branch", "release", "main")
	})
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	vanished := false
	beforePushAdmission = func() {
		if !vanished { // the parent disappears right before the first push
			vanished = true
			gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release")
		}
	}
	t.Cleanup(func() { beforePushAdmission = nil })
	require.NoError(t, f.worker.Start(context.Background()))
	t.Cleanup(f.worker.Stop)

	require.True(t, f.worker.Enqueue(configMapTargetEvent("cm1", "alice", newBranchTarget)))
	req := standbyRequest()
	f.worker.EnqueueAttach(req)
	phase := func() CommitRequestPhase {
		return f.worker.LookupCommitRequestPhase(req.Namespace, req.Name, req.UID)
	}
	require.Eventually(t, func() bool {
		open, _ := f.worker.ParentRecovery()
		return open && phase() == PhaseWaitingForPush
	}, 10*time.Second, 20*time.Millisecond, "the write is held while the parent is missing")

	clock.advance(10 * time.Minute) // far past attachTimeout + maxDuration + 120s
	f.worker.EnqueueRefresh(&RefreshRequest{Target: refreshTarget(), MaxAge: time.Nanosecond})
	require.Never(t, func() bool {
		_, resolved := outcome(t, f.worker)
		return resolved
	}, 300*time.Millisecond, 20*time.Millisecond, "a held request is never failed for taking long")
	assert.True(t, phase().Held())

	release := simulateClientCommitOnDisk(t, f.repoDir, "release", "RELEASE.md", "back\n")
	clock.advance(retryMaxBackoff)
	f.worker.EnqueueRefresh(&RefreshRequest{Target: refreshTarget(), MaxAge: time.Nanosecond})

	require.Eventually(t, func() bool {
		_, resolved := outcome(t, f.worker)
		return resolved
	}, 10*time.Second, 20*time.Millisecond)
	res, _ := outcome(t, f.worker)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	tip := f.ref("feature")
	assert.Equal(t, res.Commit, tip.String())
	assert.Equal(t, []plumbing.Hash{release}, f.parentOf(tip))
}

// A save that reached no window and asked for its message to be recorded waits for a missing parent
// like any other decided write, and is committed once the parent exists.
func TestParentRecovery_AnEmptySaveWaitsForTheParent(t *testing.T) {
	f := newRealServerNewBranch(t, "parent-recovery-empty-save", seedMain(t))
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")

	req := attachReq("alice", 0)
	req.GitTargetName = newBranchTarget
	req.CommitEmpty = true
	req.Message = "empty save"
	serviceAttach(f.loop, req)

	_, resolved := outcome(t, f.worker)
	require.False(t, resolved, "held, not failed")
	require.Len(t, f.loop.pendingWrites, 1)

	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(retryMaxBackoff)
	f.loop.runParentProbe()

	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	require.NoError(t, res.Err)
	assert.NotEmpty(t, res.Commit, "the message is recorded in Git")
	open, _ := f.worker.ParentRecovery()
	assert.False(t, open)
}

// An obligation with nothing to publish would never close, so a parent failure that left nothing in
// the log opens none. Found in review of #413, when a failed empty save left the target
// RecoveringParentBranch with nothing owed.
func TestParentRecovery_NothingOwedOpensNoObligation(t *testing.T) {
	w := &BranchWorker{}
	loop := newBranchWorkerEventLoop(w, time.Second)
	defer loop.stopTimers()

	loop.noteParentUnavailable(ErrParentBranchNotFound)

	open, _ := w.ParentRecovery()
	assert.False(t, open)
}

// Recovery ends when nothing is owed, not only when a push lands. A resync held while the parent
// was missing can find nothing to change once it is back, and leave the log empty without a push:
// recovery and its retry then stayed open, and intake stayed paused for good.
func TestParentRecovery_EndsWhenTheHeldWorkNeedsNoCommit(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-noop-resync", seedMain(t))
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	req := &ResyncRequest{GitTargetName: newBranchTarget, GitTargetNamespace: "default",
		Result: make(chan ResyncResult, 1)}
	f.loop.handleQueueItem(resyncItem(req))
	require.ErrorIs(t, (<-req.Result).Err, ErrParentBranchNotFound)
	require.Len(t, f.loop.pendingWrites, 1)
	require.True(t, f.loop.recovery.active)
	f.worker.branchBufferMaxBytes = 1
	f.loop.publishLoopState(0)
	require.NotNil(t, f.worker.IntakePaused())

	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(retryMaxBackoff)
	fireRetry(f.loop)

	require.Empty(t, f.loop.pendingWrites, "the resync had nothing to change")
	assert.False(t, f.loop.recovery.active, "nothing is owed, so there is nothing to recover")
	assert.False(t, f.loop.retry.pending(), "or to retry")
	assert.Nil(t, f.worker.IntakePaused(), "and intake reopens")
}
