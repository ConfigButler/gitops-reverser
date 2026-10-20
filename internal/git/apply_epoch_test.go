// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// F3 (docs/design/applier-echoes.md): a resync never decides from a snapshot collected while Git was
// ahead of the cluster, or across an apply. These drive the worker's side deterministically.

// epochLoop is a loop whose applier covers alpha, with alpha's initial snapshots published, and the
// GitTargets it was asked to snapshot again.
func epochLoop(t *testing.T) (*branchWorkerEventLoop, *recordingRequester, *[]itypes.ResourceReference) {
	t.Helper()
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	asked := &[]itypes.ResourceReference{}
	loop.w.resyncRecheck = func(target itypes.ResourceReference) { *asked = append(*asked, target) }
	loop.followRecordedOperations(time.Now())
	loop.refreshInitialSnapshots()
	require.True(t, loop.initialSnapshotsPublished("team-a/alpha"))
	return loop, requester, asked
}

// alphaSnapshot is a snapshot of alpha whose collection started at start.
func alphaSnapshot(start *ApplyState) *ResyncRequest {
	return &ResyncRequest{GitTargetName: "alpha", GitTargetNamespace: "team-a", CollectionStart: start,
		Result: make(chan ResyncResult, 1)}
}

func discarded(t *testing.T, req *ResyncRequest) bool {
	t.Helper()
	select {
	case res := <-req.Result:
		return errors.Is(res.Err, ErrResyncDiscarded)
	default:
		return false
	}
}

// TestApplyEpoch_ASnapshotCollectedAcrossAnApplyIsDiscarded: the collection starts while an apply is
// out, the apply completes, and the snapshot arrives with nothing pending. It holds a mix of before
// and after the apply, so it is discarded, and asked for again: by then nothing is ahead.
func TestApplyEpoch_ASnapshotCollectedAcrossAnApplyIsDiscarded(t *testing.T) {
	loop, requester, asked := epochLoop(t)
	before := loop.w.ApplyState()
	require.False(t, before.GitAhead)

	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	loop.publishApplyState()
	during := loop.w.ApplyState()
	assert.True(t, during.GitAhead, "an attempt is out")
	assert.Greater(t, during.Epoch, before.Epoch, "the send raised the epoch")

	report(sent[0], ApplySucceeded)
	loop.serviceAppliers()
	loop.publishApplyState()
	after := loop.w.ApplyState()
	require.False(t, after.GitAhead, "nothing is pending once the apply completed")
	assert.Greater(t, after.Epoch, during.Epoch, "the completion raised it again")

	started := before
	req := alphaSnapshot(&started)
	loop.handleResyncRequest(req)
	assert.True(t, discarded(t, req), "collected across the apply")
	loop.rerequestSnapshots()
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference("alpha", "team-a")}, *asked)

	whileAhead := alphaSnapshot(&during)
	loop.handleResyncRequest(whileAhead)
	assert.True(t, discarded(t, whileAhead), "collected while Git was ahead")
}

// TestApplyEpoch_ASnapshotArrivingWhileGitIsAheadWaitsForTheApply: a snapshot that arrives while a
// ForeignCommit is pending is discarded, and asked for again only once the apply answered it.
func TestApplyEpoch_ASnapshotArrivingWhileGitIsAheadWaitsForTheApply(t *testing.T) {
	loop, requester, asked := epochLoop(t)
	observeForeignCommit(loop.w)

	req := alphaSnapshot(nil)
	loop.handleResyncRequest(req)
	assert.True(t, discarded(t, req))
	loop.rerequestSnapshots()
	assert.Empty(t, *asked, "Git is still ahead")

	loop.serviceAppliers()
	sent := requester.taken()
	require.Len(t, sent, 1)
	loop.rerequestSnapshots()
	assert.Empty(t, *asked, "the apply is out")

	report(sent[0], ApplySucceeded)
	loop.serviceAppliers()
	loop.rerequestSnapshots()
	assert.Len(t, *asked, 1, "asked for again once the apply completed")
}

// TestApplyEpoch_InitialSnapshotsAreExempt: a GitTarget's snapshots before its initial snapshots are
// published are its baseline, and are never held for an apply: S2 asks for no apply before they
// are in Git, so holding them would wait on itself.
func TestApplyEpoch_InitialSnapshotsAreExempt(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	delivered := false
	loop.w.initialSnapshots = func(itypes.ResourceReference) bool { return delivered }
	loop.followRecordedOperations(time.Now())
	observeForeignCommit(loop.w)

	req := alphaSnapshot(&ApplyState{GitAhead: true})
	assert.Empty(t, loop.snapshotStale(req), "not published yet: exempt")

	delivered = true
	loop.refreshInitialSnapshots()
	assert.Equal(t, snapshotAheadAtStart, loop.snapshotStale(req), "published: F3 applies")
}

// TestApplyEpoch_ABranchWithNoTriggerNeverWaits: nothing applies its commits.
func TestApplyEpoch_ABranchWithNoTriggerNeverWaits(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", false))
	loop.followRecordedOperations(time.Now())
	observeForeignCommit(loop.w)
	assert.Empty(t, loop.snapshotStale(alphaSnapshot(&ApplyState{GitAhead: true})))
	assert.False(t, loop.w.ApplyState().GitAhead)
}

// TestApplyEpoch_SnapshotsAreHeldNoLongerThanTheLimit: an apply that keeps failing holds a
// GitTarget's snapshots for snapshotHoldLimit at most, from the first one discarded; the next is used.
func TestApplyEpoch_SnapshotsAreHeldNoLongerThanTheLimit(t *testing.T) {
	loop, _, _ := epochLoop(t)
	now := time.Now()
	loop.w.clock = func() time.Time { return now }
	observeForeignCommit(loop.w)

	loop.handleResyncRequest(alphaSnapshot(nil))
	require.Contains(t, loop.snapshotHeldSince, "team-a/alpha")
	assert.Equal(t, snapshotAheadOnArrival, loop.snapshotStale(alphaSnapshot(nil)))

	now = now.Add(snapshotHoldLimit)
	assert.Empty(t, loop.snapshotStale(alphaSnapshot(nil)), "held for the limit: used anyway")
}

// TestApplyEpoch_InitialSnapshotsWaitForTheWorkersResyncs: delivered by the streams is not enough; the
// worker must not hold a resync of the GitTarget, queued or pending.
func TestApplyEpoch_InitialSnapshotsWaitForTheWorkersResyncs(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	loop.followRecordedOperations(time.Now())
	loop.pendingWrites = []PendingWrite{
		{Kind: PendingWriteResync, GitTargetName: "alpha", GitTargetNamespace: "team-a"},
	}
	loop.refreshInitialSnapshots()
	assert.False(t, loop.initialSnapshotsPublished("team-a/alpha"), "a resync of it is not published yet")

	loop.pendingWrites = nil
	loop.refreshInitialSnapshots()
	assert.True(t, loop.initialSnapshotsPublished("team-a/alpha"))
}
