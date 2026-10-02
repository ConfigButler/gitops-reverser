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
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

// A delete decided during an outage is committed on a tree the worker fetched later, so it is planned
// under the prune policy in force then, as a replay is. Found in review of #413: the deferred write
// skipped the re-read, and a target tightened to Never still lost the file.
func TestDecidedWrite_ADeferredDeleteObeysATightenedPrunePolicy(t *testing.T) {
	f := newLedgerFixture(t, "decided-write-tightened-prune", true)
	f.createLedgerTarget("team-a", nil)
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	defer loop.stopTimers()
	write := func(event Event) {
		loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent}})
		require.True(t, loop.finalizeOpenWindow())
	}

	write(configMapTargetEvent("keep-me", "alice", ledgerTargetName))
	loop.pushPending()
	require.Contains(t, remoteFileNames(t, f.repoDir), "keep-me")

	// A retained write and a checkout to rebuild, while the remote is unreachable: the delete is
	// decided and waits.
	write(configMapTargetEvent("retained", "alice", ledgerTargetName))
	f.worker.markWorktreeDirty("a write failed part-way and could not be undone")
	_, restoreSyncs := failSyncs(t)
	deleted := configMapTargetEvent("keep-me", "alice", ledgerTargetName)
	deleted.Operation = "DELETE"
	write(deleted)
	require.Len(t, loop.pendingWrites, 2)
	require.False(t, loop.pendingWrites[1].materialized)

	// The operator stops pruning before the remote comes back.
	target := &configv1alpha3.GitTarget{}
	key := client.ObjectKey{Name: ledgerTargetName, Namespace: "default"}
	require.NoError(t, f.worker.Client.Get(f.worker.ctx, key, target))
	target.Spec.Prune = &configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneNever}
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, target))

	restoreSyncs()
	firePushTimer(loop)

	require.Empty(t, loop.pendingWrites)
	assert.Contains(t, remoteFileNames(t, f.repoDir), "keep-me",
		"the deferred delete is planned under the policy in force when it is committed")
}
