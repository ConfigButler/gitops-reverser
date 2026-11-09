// SPDX-License-Identifier: Apache-2.0

package git

// Live events held while a GitTarget's render fidelity is pending (render_fidelity_hold.go): Window
// B of docs/design/live-edit-dropped-while-render-fidelity-pending.md, which Fix 1 cannot reach.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// heldEvent is a live event of the fixture's ConfigMaps collection, by author.
func (f *replayFixture) streamEvent(name, author string, operation itypes.OperationType) Event {
	event := configMapTargetEvent(name, author, ledgerTargetName)
	event.Operation = string(operation)
	event.SourceCollection = f.scope.Collection
	if operation == itypes.OperationDelete {
		event.Object = nil
	}
	return event
}

func (f *replayFixture) send(events ...Event) {
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: events}})
}

// windowNames is the objects the open window holds, in the order they were added.
func (f *replayFixture) windowNames() []string {
	if f.loop.windows.only() == nil {
		return nil
	}
	var names []string
	for _, event := range f.loop.windows.only().orderedEvents() {
		names = append(names, event.Identifier.Name)
	}
	return names
}

// TestRenderFidelityHold_AnEditMadeWhileAnotherCollectionReplaysIsCommittedByItsAuthor is Window B:
// an edit that arrives while some collection of the GitTarget is still replaying met a pending gate
// and was dropped for good. It is held, and committed with its author once the gate opens, though
// nothing wakes the loop but its recheck: the watch layer's drain records the scope on its own
// goroutine.
func TestRenderFidelityHold_AnEditMadeWhileAnotherCollectionReplaysIsCommittedByItsAuthor(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newReplayFixture(t, "render-fidelity-hold-window-b")

	f.send(f.streamEvent("edited", "alice", itypes.OperationUpdate))
	require.Nil(t, f.loop.windows.only())
	require.NotNil(t, f.loop.holdTimer, "the held GitTarget's gate is looked at again")
	assert.Positive(t, f.loop.heldBytes(), "held writes count against the intake budget")

	_, applied := f.gate.RecordScopeClean(f.target, f.revision, f.scope.Collection)
	require.True(t, applied)
	f.loop.endWake(0) // the recheck

	assert.Equal(t, []string{"edited"}, f.windowNames(), "released once the gate opened")
	assert.Equal(t, "alice", f.loop.windows.only().Author, "with its author")
	assert.Empty(t, f.loop.held)
	assert.Zero(t, f.loop.heldEventBytes)
	assert.Zero(t, writesDropped(t, reader, writeDropRenderFidelityPending))
}

// TestRenderFidelityHold_HeldEditsGoBeforeLaterOnes: a gate can open between two wakes, and the next
// event of the GitTarget must not overtake the ones held for it.
func TestRenderFidelityHold_HeldEditsGoBeforeLaterOnes(t *testing.T) {
	f := newReplayFixture(t, "render-fidelity-hold-order")
	f.send(f.streamEvent("first", "alice", itypes.OperationUpdate))
	_, applied := f.gate.RecordScopeClean(f.target, f.revision, f.scope.Collection)
	require.True(t, applied)

	f.send(f.streamEvent("second", "bob", itypes.OperationUpdate))

	// The two edits change different files, so their windows stay open side by side, in the order
	// the edits were admitted.
	open := f.loop.windows.all()
	require.Len(t, open, 2)
	assert.Equal(t, "alice", open[0].Author, "alice's held edit went first")
	assert.Equal(t, "first", open[0].orderedEvents()[0].Identifier.Name)
	assert.Equal(t, "bob", open[1].Author)
	assert.Equal(t, "second", open[1].orderedEvents()[0].Identifier.Name)
}

// TestRenderFidelityHold_ASnapshotSupersedesTheEditsOfItsCollectionHeldBeforeIt: the replay resync
// of a collection is newer than the events of it held before it was taken off the queue, and holds
// their effect, so applying them after it would write older state over newer. They are dropped. A
// DELETE of an object the snapshot does not hold is kept: a snapshot removes a document only under
// prune mode Always. Events of another collection, and events held after the snapshot was taken,
// stay held.
func TestRenderFidelityHold_ASnapshotSupersedesTheEditsOfItsCollectionHeldBeforeIt(t *testing.T) {
	f := newReplayFixture(t, "render-fidelity-hold-supersede")
	secrets := itypes.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "default")
	_, revisions := f.gate.Reconcile(f.target,
		[]itypes.CollectionKey{f.scope.Collection, secrets}, []itypes.CollectionKey{secrets})
	require.Equal(t, f.revision, revisions[f.scope.Collection], "precondition: configmaps kept its stream")

	other := configMapTargetEvent("other-collection", "alice", ledgerTargetName)
	other.SourceCollection = secrets
	f.send(
		f.streamEvent("keep", "alice", itypes.OperationUpdate),      // in the snapshot: superseded
		f.streamEvent("gone", "alice", itypes.OperationDelete),      // not in it: kept
		f.streamEvent("recreated", "alice", itypes.OperationDelete), // in it again: superseded
		other,
	)
	require.Len(t, f.loop.held[f.target].events, 4)

	req := f.replay()
	req.Desired = append(req.Desired, desiredCMIn("recreated", "default", "green"))
	f.loop.handleQueueItem(WorkItem{Resync: &resyncMarker{request: req}})
	require.NoError(t, (<-req.Result).Err)
	f.send(f.streamEvent("after", "alice", itypes.OperationUpdate)) // held after the snapshot: kept

	held := f.loop.held[f.target]
	require.NotNil(t, held, "secrets is still pending")
	var names []string
	for _, h := range held.events {
		names = append(names, h.event.Identifier.Name)
	}
	assert.Equal(t, []string{"gone", "other-collection", "after"}, names)
	assert.Equal(t, 1, held.deletes)
}

// TestRenderFidelityHold_ACreateHeldIsAdoptedOnceItIsInAWrite is F-c: an adopt was lost while render
// fidelity was closed, because the gate returned before the create was noted. Noted on arrival, it
// would be sent before the create is in any commit, which stamps nothing and spends it. A held create
// is noted when it is released, or when a snapshot that holds it supersedes it, so the CatchUp goes
// after a flush that publishes it.
func TestRenderFidelityHold_ACreateHeldIsAdoptedOnceItIsInAWrite(t *testing.T) {
	setup := func(t *testing.T, slug string) (*echoFixture, *recordingRequester, *RenderFidelityGate, uint64) {
		t.Helper()
		f := newEchoFixture(t, slug, configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
		requester := f.withClaim()
		gate := NewRenderFidelityGate()
		f.worker.renderFidelityGate = gate
		_, revisions := gate.Reconcile(itypes.NewResourceReference(echoTarget, "default"),
			[]itypes.CollectionKey{echoScope().Collection}, []itypes.CollectionKey{echoScope().Collection})
		created := configMapTargetEvent("created", "alice", echoTarget)
		created.SourceCollection = echoScope().Collection
		f.writeWindow(created)
		require.Nil(t, f.loop.windows.only(), "held")
		require.Empty(t, requester.taken(), "nothing to adopt yet: the create is in no commit")
		return f, requester, gate, revisions[echoScope().Collection]
	}

	t.Run("released", func(t *testing.T) {
		f, requester, gate, revision := setup(t, "render-fidelity-hold-adopt-released")
		gate.RecordScopeClean(itypes.NewResourceReference(echoTarget, "default"), revision, echoScope().Collection)
		f.loop.endWake(0)

		sent := requester.taken()
		require.Len(t, sent, 1)
		assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, sent[0].Reason)
		assert.NotEmpty(t, f.onMain(echoFile("created")), "the flush published the create first")
	})

	t.Run("superseded", func(t *testing.T) {
		f, requester, _, revision := setup(t, "render-fidelity-hold-adopt-superseded")
		scope := echoScope()
		// The snapshot holds the object as Git already does: it adds nothing, so only the
		// superseded create says it was the API's.
		gitIn(t, f.clone, "pull", "--quiet", "--ff-only", "origin", "main")
		f.pushFromOutside("created", "created")
		req := &ResyncRequest{GitTargetName: echoTarget, GitTargetNamespace: "default", Scope: &scope,
			RenderFidelityRevision: revision, Initial: true, Result: make(chan ResyncResult, 1),
			Desired: []manifestanalyzer.DesiredResource{echoDesired("x", "x1"), echoDesired("created", "created")}}
		f.loop.handleQueueItem(WorkItem{Resync: &resyncMarker{request: req}})
		require.NoError(t, (<-req.Result).Err)

		require.Len(t, requester.taken(), 1, "the push from outside is a foreign commit, which is asked")
		require.NotNil(t, f.echoApplier().inflight)
		assert.True(t, f.echoApplier().inflight.adopt, "and the attempt carries the adopt")
		assert.Empty(t, f.loop.held)
	})
}

// TestRenderFidelityHold_AnApplyOverAHeldEditRepairsItOnceItIsInGit: an apply sent while an edit is
// held applies a commit without it. Its window recorded only the flush's edits, so the applier's
// write over the held edit was taken for the apply landing: the edit then reached Git with the
// cluster still running the overwritten value, and no Repair. The window now holds the held events
// from the send, and the Repair waits for the edit to reach Git: sent before, it applies a commit
// without the edit and is spent, and pending, it reads as Git ahead, so F3 discards the snapshot the
// gate waits for.
func TestRenderFidelityHold_AnApplyOverAHeldEditRepairsItOnceItIsInGit(t *testing.T) {
	target := itypes.NewResourceReference(echoTarget, "default")
	configMaps := echoScope().Collection
	secrets := itypes.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "default")
	setup := func(t *testing.T, slug string) (*echoFixture, *recordingRequester, map[itypes.CollectionKey]uint64) {
		t.Helper()
		f := newEchoFixture(t, slug, configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
		requester := f.withClaim()
		f.worker.initialSnapshots = func(itypes.ResourceReference) bool { return true }
		gate := NewRenderFidelityGate()
		f.worker.renderFidelityGate = gate
		// ConfigMaps replayed clean; Secrets is replaying again, so the GitTarget's events are held.
		_, revisions := gate.Reconcile(target, []itypes.CollectionKey{configMaps, secrets},
			[]itypes.CollectionKey{configMaps, secrets})
		gate.RecordScopeClean(target, revisions[configMaps], configMaps)
		edit := echoEvent("x", "x2")
		edit.SourceCollection = configMaps
		edit.LastFieldManagers = []string{"kubectl-edit"}
		edit.LastFieldManagedAt = time.Now()
		f.writeWindow(edit)
		require.Len(t, f.loop.held[target].events, 1)

		// A push from outside is applied while the edit is held.
		f.echoApplier().foreign = true
		f.loop.endWake(0)
		sent := requester.taken()
		require.Len(t, sent, 1)
		require.Equal(t, echoDocument("x", "x1"), f.onMain(echoFile("x")), "the commit sent does not hold the edit")

		over := echoEvent("x", "x1")
		over.SourceCollection = configMaps
		over.LastFieldManagers = []string{FluxFieldManager}
		over.LastFieldManagedAt = time.Now().Add(time.Second)
		assert.True(t, f.loop.withholdLateApply(over), "the apply over the held edit is withheld")
		report(sent[0], ApplySucceeded)
		f.loop.endWake(0)
		assert.Empty(t, requester.taken(), "no Repair goes before the edit is in Git")
		assert.False(t, f.loop.appliersAhead(), "nor is one pending, which would hold back the Secrets snapshot")
		return f, requester, revisions
	}
	repaired := func(t *testing.T, f *echoFixture, requester *recordingRequester) {
		t.Helper()
		f.loop.finalizeOpenWindowWithReason(windowFinalizeReasonIdleTimeout)
		f.loop.pushPending()
		f.loop.endWake(0)
		assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")))
		sent := requester.taken()
		require.Len(t, sent, 1, "the cluster runs x1, so the commit that holds the edit is applied")
		assert.Equal(t, configv1alpha3.ApplyReasonRepair, sent[0].Reason)
		assert.Equal(t, gitIn(t, f.repoDir, "rev-parse", "main"), sent[0].Commit)
	}

	// The Secrets replay is not the collection's first, so F3 would discard its snapshot with a Repair
	// pending, and ask again only once that Repair went.
	replaySecrets := func(t *testing.T, f *echoFixture, revisions map[itypes.CollectionKey]uint64) {
		t.Helper()
		scope := ResyncScope{Collection: secrets}
		req := &ResyncRequest{GitTargetName: echoTarget, GitTargetNamespace: "default", Scope: &scope,
			RenderFidelityRevision: revisions[secrets], Result: make(chan ResyncResult, 1)}
		f.loop.handleQueueItem(WorkItem{Resync: &resyncMarker{request: req}})
		require.NoError(t, (<-req.Result).Err, "the snapshot is used, not discarded")
	}

	t.Run("released", func(t *testing.T) {
		f, requester, revisions := setup(t, "render-fidelity-hold-overwritten-released")
		replaySecrets(t, f, revisions)
		f.loop.endWake(0)
		repaired(t, f, requester)
	})

	t.Run("across a second apply", func(t *testing.T) {
		f, requester, revisions := setup(t, "render-fidelity-hold-overwritten-twice")
		f.echoApplier().foreign = true
		f.loop.endWake(0)
		sent := requester.taken()
		require.Len(t, sent, 1, "another apply goes while the edit is held")
		over := echoEvent("x", "x1")
		over.SourceCollection = configMaps
		over.LastFieldManagers = []string{FluxFieldManager}
		over.LastFieldManagedAt = time.Now().Add(2 * time.Second)
		assert.True(t, f.loop.withholdLateApply(over), "its window holds the edit from the send")
		report(sent[0], ApplySucceeded)
		f.loop.endWake(0)
		require.Empty(t, requester.taken())
		require.Len(t, f.loop.held[target].events, 1, "the edit is still held")

		replaySecrets(t, f, revisions)
		f.loop.endWake(0)
		repaired(t, f, requester)
	})

	t.Run("superseded", func(t *testing.T) {
		f, requester, revisions := setup(t, "render-fidelity-hold-overwritten-superseded")
		// A snapshot of ConfigMaps listed before the apply wrote over the edit holds it.
		scope := echoScope()
		req := &ResyncRequest{GitTargetName: echoTarget, GitTargetNamespace: "default", Scope: &scope,
			RenderFidelityRevision: revisions[configMaps], Initial: true, Result: make(chan ResyncResult, 1),
			Desired: []manifestanalyzer.DesiredResource{echoDesired("x", "x2")}}
		f.loop.handleQueueItem(WorkItem{Resync: &resyncMarker{request: req}})
		require.NoError(t, (<-req.Result).Err)
		require.Empty(t, f.loop.held, "the snapshot superseded the edit")
		repaired(t, f, requester)
	})
}

// echoScope is the echo fixture's ConfigMaps collection.
func echoScope() ResyncScope {
	return ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "default")
}

// TestRenderFidelityHold_AHoldPastItsLimitAsksForAFreshSnapshot: a scope can stay pending for a long
// time, so the hold is bounded. Past the limit the GitTarget's held writes are dropped and counted,
// and a fresh snapshot of it is asked for, which commits the current state as the operator.
func TestRenderFidelityHold_AHoldPastItsLimitAsksForAFreshSnapshot(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newReplayFixture(t, "render-fidelity-hold-limit")
	now := time.Now()
	f.worker.clock = func() time.Time { return now }
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	logged := captureInfo(f.worker)

	f.send(f.streamEvent("edited", "alice", itypes.OperationUpdate),
		f.streamEvent("removed", "alice", itypes.OperationDelete))
	now = now.Add(renderFidelityHoldLimit - time.Second)
	f.loop.endWake(0)
	require.NotNil(t, f.loop.held[f.target], "not yet")

	now = now.Add(time.Second)
	f.loop.endWake(0)
	assert.Empty(t, f.loop.held, "given up")
	assert.Zero(t, f.loop.heldEventBytes)
	assert.Equal(t, []itypes.ResourceReference{f.target}, asked, "a fresh snapshot is asked for")
	assert.Equal(t, int64(2), writesDropped(t, reader, writeDropRenderFidelityPending))
	gaveUp := logged.matching("Dropped the live writes held")
	require.Len(t, gaveUp, 1)
	assert.Contains(t, gaveUp[0], `"deletions"=1`, "it says how many deletions a snapshot may not bring back")
}

// TestRenderFidelityHold_TheBranchBudgetBoundsTheHold: held writes are charged like a window, and
// past the branch's retained-byte budget they are given up at once rather than grow.
func TestRenderFidelityHold_TheBranchBudgetBoundsTheHold(t *testing.T) {
	f := newReplayFixture(t, "render-fidelity-hold-budget")
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	event := f.streamEvent("edited", "alice", itypes.OperationUpdate)
	f.worker.branchBufferMaxBytes = eventRetainedCharge(&event) + 1

	f.send(event)
	require.NotNil(t, f.loop.held[f.target])
	f.send(f.streamEvent("again", "alice", itypes.OperationUpdate))

	assert.Empty(t, f.loop.held)
	assert.Equal(t, []itypes.ResourceReference{f.target}, asked)
}

// TestRenderFidelityHold_ADivergenceAfterAReplayDropsWhatWasHeld: a divergence is the gate doing its
// job, so what was held for the GitTarget is dropped, as a write arriving then would be.
func TestRenderFidelityHold_ADivergenceAfterAReplayDropsWhatWasHeld(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	f := newReplayFixture(t, "render-fidelity-hold-diverged")
	f.send(f.streamEvent("edited", "alice", itypes.OperationUpdate))

	_, applied := f.gate.RecordScopeDivergence(f.target, f.revision, f.scope.Collection,
		manifestanalyzer.RenderDivergence{Field: "data.region", Token: "${REGION}"})
	require.True(t, applied)
	f.loop.endWake(0)

	assert.Empty(t, f.loop.held)
	assert.Nil(t, f.loop.windows.only())
	assert.Equal(t, int64(1), writesDropped(t, reader, writeDropRenderFidelityDiverged))
}
