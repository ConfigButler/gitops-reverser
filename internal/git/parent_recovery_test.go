// SPDX-License-Identifier: Apache-2.0

package git

// Work a missing parent branch held back is an obligation, not an observation: it stays open until
// the work is published, so a transient failure after the parent reappears cannot strand it, and
// it is driven by the worker on its own deadline, whatever the refresh configuration. These pin
// the obligation, its per-scope bookkeeping, and the probe budget.

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
			clock.advance(parentProbeMaxBackoff)
			deadline()

			assert.True(t, f.ref("feature").IsZero(), "the first attempt failed")
			require.Len(t, f.loop.pendingWrites, 1, "and the work is still held")
			open, found = f.worker.ParentRecovery()
			require.True(t, open, "the obligation outlives a failed attempt")
			assert.True(t, found)

			clock.advance(parentProbeMaxBackoff)
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

// Test 2. Writes dropped for two targets on one worker. Each scope clears only when a resync of it
// is published; a resync that committed but is not pushed yet does not clear it, and a target
// whose resync failed is asked again on the next deadline.
func TestParentRecovery_DroppedWritesClearPerScopeWhenPublished(t *testing.T) {
	f := newNewBranchFixture(t, nil)
	clock := useTestClock(f.worker)
	createPlainGitTarget(t, f.worker, "other-target", "other")
	f.worker.SetParentBranch("release")
	loop := newBranchWorkerEventLoop(f.worker, 0)
	defer loop.stopTimers()

	a := itypes.NewResourceReference(newBranchTarget, "default")
	b := itypes.NewResourceReference("other-target", "default")
	collectionA := itypes.CollectionKey{Resource: "configmaps", Namespace: "default"}
	collectionB := itypes.CollectionKey{Resource: "configmaps", Namespace: "default", LabelSelector: "team=b"}
	write := func(target string, collection itypes.CollectionKey, name string) {
		event := configMapTargetEvent(name, "alice", target)
		event.SourceCollection = collection
		loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	}
	resync := func(target itypes.ResourceReference, collection itypes.CollectionKey, name string) error {
		scope := ResyncScope{Collection: collection}
		req := &ResyncRequest{
			Desired:            []manifestanalyzer.DesiredResource{desiredCM(name, "blue")},
			ResourceVersion:    "1",
			GitTargetName:      target.Name,
			GitTargetNamespace: target.Namespace,
			Scope:              &scope,
			Result:             make(chan ResyncResult, 1),
		}
		loop.applyResync(req)
		return (<-req.Result).Err
	}

	write(newBranchTarget, collectionA, "a1")
	write("other-target", collectionB, "b1")
	assert.Empty(t, loop.pendingWrites, "both were dropped")
	assert.Len(t, loop.recovery.scopes, 2)

	f.pushToRelease("RELEASE.md", "release\n")
	clock.advance(parentProbeMaxBackoff)
	loop.runParentProbe()
	assert.Equal(t, uint64(1), f.worker.SnapshotRequestSeq(a))
	assert.Equal(t, uint64(1), f.worker.SnapshotRequestSeq(b))

	loop.lastPushAt = time.Now() // cooldown: A's resync commits, and its push waits
	require.NoError(t, resync(a, collectionA, "a1"))
	require.Len(t, loop.pendingWrites, 1)
	assert.Contains(t, loop.recovery.scopes, recoveryScope{target: a, collection: collectionA},
		"a resync that is not pushed yet does not clear its scope")

	failNextSync(t)
	require.Error(t, resync(b, collectionB, "b1"))

	clock.advance(parentProbeMaxBackoff)
	loop.runParentProbe() // publishes A's commit, and asks for B again

	assert.Empty(t, loop.pendingWrites)
	assert.NotContains(t, loop.recovery.scopes, recoveryScope{target: a, collection: collectionA}, "published")
	assert.Equal(t, uint64(1), f.worker.SnapshotRequestSeq(a), "A is not asked again")
	assert.Equal(t, uint64(2), f.worker.SnapshotRequestSeq(b), "B is")

	require.NoError(t, resync(b, collectionB, "b1"))
	loop.pushPending()
	open, _ := f.worker.ParentRecovery()
	assert.False(t, open, "every scope is published")
}

// Test 4. However many targets share the worker, and however often refreshes tick and live writes
// arrive, a parent that stays missing is asked about at most once per deadline of the shared
// schedule, and never fetched. That holds with work retained too: there, every write first wants to
// rebuild the retained writes, which is a fetch the deadline must hold back as well.
func TestParentRecovery_ProbeBudgetIsPerWorker(t *testing.T) {
	for name, retained := range map[string]bool{"dropped writes": false, "retained writes": true} {
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

// A second outage while a recovery snapshot waits for its push. The write dropped during it was
// made after that snapshot was taken, so publishing the snapshot must not settle its scope: the
// worker asks for another one, and the obligation stays open until that is published.
func TestParentRecovery_ASecondOutageKeepsTheWorkDroppedInIt(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-second-outage", func(dir string) {
		simulateClientCommitOnDisk(t, dir, "main", "README.md", "main\n")
	})
	clock := useTestClock(f.worker)
	f.worker.SetParentBranch("release")
	collection := itypes.CollectionKey{Resource: "configmaps", Namespace: "default"}
	write := func(name string) {
		event := configMapTargetEvent(name, "alice", newBranchTarget)
		event.SourceCollection = collection
		f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
	}

	write("cm1") // dropped: release does not exist
	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(parentProbeMaxBackoff)
	f.loop.runParentProbe()
	seq := f.worker.SnapshotRequestSeq(refreshTarget())
	require.Equal(t, uint64(1), seq)

	f.loop.lastPushAt = time.Now() // the snapshot commits, and its push waits for the cooldown
	req := &ResyncRequest{
		Desired:         []manifestanalyzer.DesiredResource{desiredCM("cm1", "blue")},
		ResourceVersion: "1", GitTargetName: newBranchTarget, GitTargetNamespace: "default",
		Scope:  &ResyncScope{Collection: collection},
		Result: make(chan ResyncResult, 1),
	}
	f.loop.applyResync(req)
	require.NoError(t, (<-req.Result).Err)
	require.Len(t, f.loop.recovery.awaitingPush, 1)

	gitIn(t, f.repoDir, "update-ref", "-d", "refs/heads/release") // the second outage
	f.loop.pushPending()
	write("cm2") // dropped during it
	require.Len(t, f.loop.pendingWrites, 1, "cm2 was dropped; only the snapshot is retained")

	gitIn(t, f.repoDir, "branch", "release", "main")
	clock.advance(parentProbeMaxBackoff)
	f.loop.runParentProbe()

	require.False(t, f.ref("feature").IsZero(), "the snapshot was published")
	open, _ := f.worker.ParentRecovery()
	assert.True(t, open, "cm2 still needs a snapshot")
	assert.Greater(t, f.worker.SnapshotRequestSeq(refreshTarget()), seq, "and the worker asks for one")
}

// Test 5. An unresolved default branch, once repaired, recovers through the same path: the probe
// finds it and asks for the snapshot the dropped write needs.
func TestParentRecovery_ARepairedRemoteHeadAsksForTheSnapshot(t *testing.T) {
	f := newRealServerNewBranch(t, "recovery-dangling-head", func(repoDir string) {
		simulateClientCommitOnDisk(t, repoDir, "main", "README.md", "main\n")
		gitIn(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/master")
	})
	clock := useTestClock(f.worker)

	liveWrite(f.loop, "cm1")
	assert.True(t, f.ref("feature").IsZero())

	gitIn(t, f.repoDir, "symbolic-ref", "HEAD", "refs/heads/main")
	clock.advance(parentProbeMaxBackoff)
	f.loop.runParentProbe()

	assert.Equal(t, uint64(1), f.worker.SnapshotRequestSeq(refreshTarget()))
	open, found := f.worker.ParentRecovery()
	assert.True(t, open, "open until the snapshot is published")
	assert.True(t, found)
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
	clock.advance(parentProbeMaxBackoff)
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
