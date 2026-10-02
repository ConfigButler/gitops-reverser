// SPDX-License-Identifier: Apache-2.0

package git

// A retained write that the remote now refuses, met during a replay.
//
// A write is accepted against the tree it was first committed on. If the remote moves before the
// write is published, the write is replayed onto the new tip, and the new tip can hold content that
// makes the write's folder unacceptable: somebody else turned it into a kustomize root the writer
// does not support, say. Retrying cannot help, because the content is not going to change by
// itself. Aborting the whole replay used to keep every write behind the refused one waiting
// with it, indefinitely. A refusal met during a replay now settles only its own entry, as a refusal
// at first commit does: it is reported, its save fails, and it leaves the log. The writes after it
// are replayed and published.
//
// See docs/design/gittarget-branch-worker-log.md, step 4.

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// replayRefusalTargetB is a second GitTarget on the same branch, in a folder nobody contends, so
// its write is one a refusal in the first target's folder has no reason to touch.
const replayRefusalTargetB = "target-b"

// newReplayRefusalFixture builds a ledger fixture with two targets on one branch, primed with one
// published commit, and an event loop waiting out its push cooldown.
func newReplayRefusalFixture(t *testing.T, slug string) (*ledgerFixture, *branchWorkerEventLoop) {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.createLedgerTarget("team-a", nil)
	createPlainGitTarget(t, f.worker, replayRefusalTargetB, "team-b")
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	t.Cleanup(loop.stopTimers)
	return f, loop
}

// retainTwoWrites commits a write into each target's folder, in that order, and keeps both in the
// log. The first carries a save, so its outcome can be read off the request.
func retainTwoWrites(t *testing.T, loop *branchWorkerEventLoop) {
	t.Helper()
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("refused-on-replay", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.NotNil(t, loop.openWindow)
	req := attachReq("alice", 0)
	req.GitTargetName = ledgerTargetName
	req.Message = "save: lands in a folder that is refused before it is pushed"
	serviceAttach(loop, req)
	require.Len(t, loop.pendingWrites, 1)
	require.NotNil(t, loop.pendingWrites[0].CommitRequest)

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("kept-behind-it", "bob", replayRefusalTargetB)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 2)
	require.True(t, loop.checkoutCurrent(), "both writes are committed locally")
}

// refuseFirstTargetsFolder makes the remote refuse every write into team-a from now on: another
// writer turns it into a kustomize root using a feature the writer does not support. Pushing it
// also moves the remote, so the worker's next push is rejected.
func refuseFirstTargetsFolder(f *ledgerFixture) {
	f.contend("team-a/kustomization.yaml", hardKustomizeYAML)
}

// assertOnlyTheRefusedWriteWasDropped checks the outcome both replay paths must reach.
func assertOnlyTheRefusedWriteWasDropped(
	t *testing.T, f *ledgerFixture, loop *branchWorkerEventLoop, refusals *[]capturedRefusal,
) {
	t.Helper()
	names := remoteFileNames(t, f.repoDir)
	assert.Contains(t, names, "kept-behind-it", "the write behind the refused one is published")
	assert.NotContains(t, names, "refused-on-replay", "the refused write is not")
	assert.Contains(t, names, "team-a/kustomization.yaml", "and the other writer's commit is kept")
	assert.Empty(t, loop.pendingWrites, "nothing is left in the log to block the writes after it")
	assert.False(t, loop.retry.pending(), "nothing is owed, so no retry is scheduled")

	require.Len(t, *refusals, 1, "the refusal is reported on the target's status, once")
	assert.Equal(t, itypes.NewResourceReference(ledgerTargetName, "default"), (*refusals)[0].target)

	res, resolved := f.worker.LookupCommitRequestOutcome("default", crName, "uid-"+crName)
	require.True(t, resolved, "the refused write's save is answered rather than held")
	require.Error(t, res.Err, "and it fails: its write will never land")
	assert.Contains(t, res.Err.Error(), "kustomization.yaml", "naming what refused it")
}

// TestReplayRefusal_OnTheContentionPathDropsOnlyItsOwnEntry is the replay the push cycle runs when
// its push is rejected because the remote moved.
func TestReplayRefusal_OnTheContentionPathDropsOnlyItsOwnEntry(t *testing.T) {
	f, loop := newReplayRefusalFixture(t, "replay-refusal-contention")
	refusals := captureRefusals(f.worker)
	retainTwoWrites(t, loop)
	refuseFirstTargetsFolder(f)

	loop.pushPending()

	assertOnlyTheRefusedWriteWasDropped(t, f, loop, refusals)
}

// TestReplayRefusal_OnTheLoopsRebuildDropsOnlyItsOwnEntry is the replay materialize runs when the
// checkout no longer holds the retained writes, before anything is pushed.
func TestReplayRefusal_OnTheLoopsRebuildDropsOnlyItsOwnEntry(t *testing.T) {
	f, loop := newReplayRefusalFixture(t, "replay-refusal-rebuild")
	refusals := captureRefusals(f.worker)
	retainTwoWrites(t, loop)
	refuseFirstTargetsFolder(f)
	// A write that failed part-way and could not be undone: the checkout must be rebuilt from the
	// remote before anything is committed on it or pushed.
	f.worker.markWorktreeDirty("test: a failed write left leftovers")

	loop.pushPending()

	assertOnlyTheRefusedWriteWasDropped(t, f, loop, refusals)
}

// TestReplayRefusal_AnotherFailureStillHoldsEveryWrite: only a refusal is a final answer about one
// write. Any other failure during the same replay says nothing about the write it hit, so the
// attempt is abandoned and every write is kept for the retry, the refused one included.
func TestReplayRefusal_AnotherFailureStillHoldsEveryWrite(t *testing.T) {
	f, loop := newReplayRefusalFixture(t, "replay-refusal-other-failure")
	refusals := captureRefusals(f.worker)
	retainTwoWrites(t, loop)
	refuseFirstTargetsFolder(f)

	restoreAPI := failGitTargetReads(t, f.worker, errors.New("etcdserver: request timed out"))
	loop.pushPending()

	require.Len(t, loop.pendingWrites, 2, "an aborted replay settles nothing")
	assert.Empty(t, *refusals, "and reports no refusal it did not finish deciding")
	_, resolved := f.worker.LookupCommitRequestOutcome("default", crName, "uid-"+crName)
	assert.False(t, resolved, "the save stays held")
	assert.True(t, loop.retry.pending(), "the retry is scheduled")

	restoreAPI()
	loop.pushPending()

	assertOnlyTheRefusedWriteWasDropped(t, f, loop, refusals)
}

// TestReplayRefusal_AResyncAnsweredEarlierIsReportedNotAnsweredAgain: a committed resync has already
// told its caller what it found. When a replay later refuses it, nobody is waiting for it any more,
// so its refusal is reported on the target, the way a live write's is.
func TestReplayRefusal_AResyncAnsweredEarlierIsReportedNotAnsweredAgain(t *testing.T) {
	f, loop := newReplayRefusalFixture(t, "replay-refusal-resync")
	f.worker.mapper = configMapMapper()
	refusals := captureRefusals(f.worker)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{desiredCM("reconciled-then-refused", "blue")},
		GitTargetName:      ledgerTargetName,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	loop.handleQueueItem(WorkItem{Resync: req})
	result := <-req.Result
	require.NoError(t, result.Err)
	require.Equal(t, 1, result.Stats.Created)
	require.Len(t, loop.pendingWrites, 1, "the resync is retained as a local commit")

	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("kept-behind-it", "bob", replayRefusalTargetB)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 2)

	refuseFirstTargetsFolder(f)
	loop.pushPending()

	names := remoteFileNames(t, f.repoDir)
	assert.Contains(t, names, "kept-behind-it", "the write behind the refused resync is published")
	assert.NotContains(t, names, "reconciled-then-refused", "the refused resync is not")
	assert.Empty(t, loop.pendingWrites)
	require.Len(t, *refusals, 1, "the refusal is reported on the target's status")
	assert.Empty(t, req.Result, "and the caller, answered already, is not answered a second time")
}
