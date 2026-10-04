// SPDX-License-Identifier: Apache-2.0

package git

// The branch's intake gate during an outage: what a worker accepts is bounded at enqueue, intake
// stays paused until a publication lands, and the producers it refused are woken when it reopens.
// See docs/design/gittarget-branch-worker-log.md, "Recovery contract".

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// outageLoop is a worker whose remote cannot be reached, with one write held in its log and a
// retry pending: the state in which the budget applies.
func outageLoop(t *testing.T, slug string) (*ledgerFixture, *branchWorkerEventLoop, func()) {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")
	f.worker.markWorktreeDirty("a write failed part-way and could not be undone")
	_, restoreSyncs := failSyncs(t)
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(loop.stopTimers)
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("held-during-the-outage", "alice", ledgerTargetName)},
		CommitMode: CommitModePerEvent,
	}})
	loop.finalizeOpenWindow()
	loop.syncUnpushedWorkFlag()
	require.True(t, loop.retry.pending())
	require.Len(t, loop.pendingWrites, 1)
	return f, loop, restoreSyncs
}

// The budget bounds what a worker accepts at enqueue. It used to be read only once per loop
// iteration, against what the loop already held, so producers could fill the whole FIFO between
// two iterations however far past the budget that went.
func TestIntake_TheBudgetBoundsAcceptedWorkAtEnqueue(t *testing.T) {
	f, loop, _ := outageLoop(t, "intake-bounded-at-enqueue")
	budget := loop.heldBytes() + 10*1024
	f.worker.branchBufferMaxBytes = budget
	loop.syncUnpushedWorkFlag()
	require.Nil(t, f.worker.IntakePaused(), "below the budget, intake is open")

	const offered = 50
	admitted := 0
	for i := range offered {
		if f.worker.Enqueue(configMapTargetEvent(fmt.Sprintf("offered-%d", i), "alice", ledgerTargetName)) {
			admitted++
		}
	}
	assert.Positive(t, admitted, "the room under the budget is used")
	assert.Less(t, admitted, offered, "what does not fit is refused at enqueue, before the loop runs")
	assert.NotNil(t, f.worker.IntakePaused(), "and the refusal pauses the branch")

	for len(f.worker.eventQueue) > 0 {
		loop.handleQueueItem(<-f.worker.eventQueue)
		loop.releaseHandledItem()
	}
	assert.LessOrEqual(t, loop.heldBytes(), budget, "everything accepted fits the budget once the loop holds it")
}

// Everything the loop keeps counts against the budget: the open window, the log, a deferred heal's
// snapshot, and a save registered while it waits for a window.
func TestIntake_EveryKindOfHeldWorkCounts(t *testing.T) {
	_, loop, _ := outageLoop(t, "intake-held-kinds")
	held := loop.heldBytes()

	req := attachReq("alice", time.Hour)
	req.GitTargetName = ledgerTargetName
	req.Message = "save once the window opens"
	loop.handleAttachCommitRequest(req)
	require.Contains(t, loop.pendingCRs, req.id())
	assert.GreaterOrEqual(t, loop.heldBytes()-held, int64(pendingWriteOverheadBytes),
		"a registered save is charged for what it keeps")
	held = loop.heldBytes()

	heal := &ResyncRequest{GitTargetName: ledgerTargetName, GitTargetNamespace: "default", Heal: true,
		Desired: []manifestanalyzer.DesiredResource{{Object: bigConfigMap("deferred", 4096)}},
		Result:  make(chan ResyncResult, 1)}
	loop.openWindow = &openWindow{} // a heal waits for the window to close
	loop.handleResyncRequest(heal)
	require.Len(t, loop.deferredHeals, 1)
	assert.GreaterOrEqual(t, loop.heldBytes()-held, int64(4096), "a deferred heal's snapshot is held")
	held = loop.heldBytes()

	loop.windowBytes = 2048
	assert.Equal(t, held+2048, loop.heldBytes(), "and so is the open window")
}

// A paused branch reopens only once a publication lands. A budget that has room again, or a remote
// that can be read but still refuses the push, proves nothing about whether the backlog can drain.
func TestIntake_StaysPausedUntilAPublicationLands(t *testing.T) {
	f, loop, restoreSyncs := outageLoop(t, "intake-paused-until-published")
	f.worker.branchBufferMaxBytes = 1
	loop.syncUnpushedWorkFlag()
	paused := f.worker.IntakePaused()
	require.NotNil(t, paused)

	f.worker.branchBufferMaxBytes = 1 << 40
	loop.syncUnpushedWorkFlag()
	assert.NotNil(t, f.worker.IntakePaused(), "room under the budget does not reopen a paused branch")

	restoreSyncs()
	restorePushes := failPushes(t, f.worker)
	fireRetry(loop)
	loop.syncUnpushedWorkFlag()
	require.NotEmpty(t, loop.pendingWrites)
	assert.NotNil(t, f.worker.IntakePaused(), "a remote that can be read but refuses the push stays paused")
	assert.True(t, loop.retry.pending(), "and the backoff continues")

	restorePushes()
	fireRetry(loop)
	loop.syncUnpushedWorkFlag()
	require.Empty(t, loop.pendingWrites)
	assert.Nil(t, f.worker.IntakePaused(), "a landed publication reopens intake")
	select {
	case <-paused:
	default:
		t.Fatal("the producers waiting on the pause are woken")
	}
}

// A snapshot larger than the whole budget is accepted on a healthy branch, whose log drains at the
// next push. During an outage it can never fit, so it is refused with that said, pauses the branch
// rather than being offered again on every reconnect, and is accepted once a publication lands.
func TestIntake_ASnapshotLargerThanTheBudget(t *testing.T) {
	f, loop, restoreSyncs := outageLoop(t, "intake-oversized-snapshot")
	f.worker.branchBufferMaxBytes = loop.heldBytes() + 4096
	loop.syncUnpushedWorkFlag()
	require.Nil(t, f.worker.IntakePaused())

	oversized := func() *ResyncRequest {
		return &ResyncRequest{GitTargetName: ledgerTargetName, GitTargetNamespace: "default",
			Desired: []manifestanalyzer.DesiredResource{{Object: bigConfigMap("huge", 64*1024)}},
			Result:  make(chan ResyncResult, 1)}
	}
	refused := oversized()
	require.False(t, f.worker.EnqueueResync(refused))
	err := (<-refused.Result).Err
	require.ErrorIs(t, err, ErrFinalizeQueueFull)
	assert.Contains(t, err.Error(), "exceeds", "the refusal says the snapshot cannot fit")
	assert.NotNil(t, f.worker.IntakePaused(), "the refusal pauses the branch")

	restoreSyncs()
	fireRetry(loop)
	loop.syncUnpushedWorkFlag()
	require.Nil(t, f.worker.IntakePaused())
	assert.True(t, f.worker.EnqueueResync(oversized()), "a healthy branch accepts it")
}

// Lifecycle work is not payload: while intake is paused, a withdrawal still enters the FIFO and
// cancels a save that is only waiting for a window.
func TestIntake_AWithdrawalProgressesWhilePaused(t *testing.T) {
	f, loop, _ := outageLoop(t, "intake-withdraw-while-paused")
	req := attachReq("alice", time.Hour)
	req.GitTargetName = ledgerTargetName
	loop.handleAttachCommitRequest(req)
	f.worker.branchBufferMaxBytes = 1
	loop.syncUnpushedWorkFlag()
	require.NotNil(t, f.worker.IntakePaused())

	f.worker.EnqueueWithdraw(req)
	require.Len(t, f.worker.eventQueue, 1, "a withdrawal is not refused by the pause")
	loop.handleQueueItem(<-f.worker.eventQueue)
	loop.releaseHandledItem()
	outcome, ok := f.worker.LookupCommitRequestOutcome(req.Namespace, req.Name, req.UID)
	require.True(t, ok)
	assert.ErrorIs(t, outcome.Err, ErrCommitRequestWithdrawn)
}

// bigConfigMap is a ConfigMap whose data is about size bytes.
func bigConfigMap(name string, size int) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetName(name)
	obj.SetNamespace("default")
	obj.Object["data"] = map[string]interface{}{"blob": strings.Repeat("x", size)}
	return obj
}
