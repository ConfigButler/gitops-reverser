// SPDX-License-Identifier: Apache-2.0

package git

// A closed window is a decided write. A remote that cannot be reached when the window closes used
// to drop the window and fail the save riding it, and only a missing parent asked for a snapshot to
// re-derive what was lost. These pin that the decision now survives the outage, keeps its author
// and message, and lands through the publication retry, without a fetch per window meanwhile.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failSyncs makes every sync to the remote fail as an unreachable remote would, until restored,
// and counts the attempts.
func failSyncs(t *testing.T) (*atomic.Int32, func()) {
	t.Helper()
	original := syncToRemoteFn
	var attempts atomic.Int32
	syncToRemoteFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.ReferenceName, _ string, _ []gitclient.Option,
	) (*PullReport, error) {
		attempts.Add(1)
		return nil, errors.New("dial tcp: connection refused")
	}
	restore := func() { syncToRemoteFn = original }
	t.Cleanup(restore)
	return &attempts, restore
}

func TestDecidedWrite_SurvivesAFailedRebuildAndLandsThroughTheRetry(t *testing.T) {
	f := newLedgerFixture(t, "decided-write-survives-rebuild", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now() // the first write waits out the push cooldown
	defer loop.stopTimers()

	// One write is committed and retained, and then the checkout has to be rebuilt before anything
	// can commit on it, while the remote is unreachable.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("retained", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	f.worker.markWorktreeDirty("a write failed part-way and could not be undone")
	syncs, restoreSyncs := failSyncs(t)

	// A save's window closes during the outage.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("saved-during-outage", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	req := attachReq("alice", 0)
	req.GitTargetName = ledgerTargetName
	req.Message = "save during the outage"
	serviceAttach(loop, req)

	_, resolved := outcome(t, f.worker)
	require.False(t, resolved, "the save is held, not failed: its write can still land")
	assert.Equal(t, PhaseWaitingForPush, f.worker.LookupCommitRequestPhase("default", crName, "uid-"+crName))
	require.Len(t, loop.pendingWrites, 2, "the window is decided and kept in the log")
	assert.False(t, loop.pendingWrites[1].materialized)
	require.True(t, loop.publicationRetry.pending(), "the failure armed the publication retry")
	require.Equal(t, int32(1), syncs.Load(), "the window tried the rebuild once")

	// Another window during the outage is decided without spending a connection on the rebuild.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("also-during-outage", "bob", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	assert.Equal(t, int32(1), syncs.Load(), "a decision waits for the pending retry instead of fetching")
	require.Len(t, loop.pendingWrites, 3)

	// The remote comes back and the retry fires.
	restoreSyncs()
	firePushTimer(loop)

	res, resolved := outcome(t, f.worker)
	require.True(t, resolved)
	require.NoError(t, res.Err)
	assert.Equal(t, FinalizeCommitted, res.Outcome)
	assert.Contains(t, gitOut(t, f.repoDir, "log", "-1", "--format=%B", res.Commit), "save during the outage",
		"the save's own message reached Git")
	assert.Contains(t, gitOut(t, f.repoDir, "log", "-1", "--format=%an", res.Commit), "alice",
		"the window's own author reached Git")
	names := remoteFileNames(t, f.repoDir)
	for _, name := range []string{"retained", "saved-during-outage", "also-during-outage"} {
		assert.Contains(t, names, name)
	}
	assert.Empty(t, loop.pendingWrites)
	assert.False(t, loop.publicationRetry.pending())
}
