// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// The tests here pin docs/design/applier-echoes.md below end to end. The Git ones run on a real Git
// server (startRealGitServer): file:// never compares the tip a push expects, so a test of what a
// rejected push replays would pass without one ever being rejected.

const echoTarget = "team-a"

// echoFixture is a real Git server whose main already holds team-a's folder, a warm worker for main,
// and its event loop. pushFromOutside moves main behind the worker's back, as a person would.
type echoFixture struct {
	*ledgerFixture

	loop *branchWorkerEventLoop
	// clone is the outside clone pushFromOutside commits from.
	clone string
}

func newEchoFixture(t *testing.T, slug string, prune configv1alpha3.PruneMode, docs map[string]string) *echoFixture {
	t.Helper()
	return newEchoFixtureWith(t, slug, prune, docs, nil)
}

// newEchoFixtureWith is newEchoFixture with files, by path, committed from outside after the documents
// and before the worker clones, so the worker starts on them as on anything it has applied.
func newEchoFixtureWith(
	t *testing.T, slug string, prune configv1alpha3.PruneMode, docs, files map[string]string,
) *echoFixture {
	t.Helper()
	f := newLedgerFixture(t, slug, true)
	f.worker.mapper = configMapMapper()
	createGitTargetWithPruneMode(t, f.worker, echoTarget, echoTarget, prune)
	clone := filepath.Join(t.TempDir(), "outside")
	gitIn(t, filepath.Dir(clone), "clone", "--quiet", f.repoDir, clone)
	gitIn(t, clone, "config", "user.name", "Outside")
	gitIn(t, clone, "config", "user.email", "outside@example.com")
	e := &echoFixture{ledgerFixture: f, clone: clone}
	for name, value := range docs {
		e.pushFromOutside(name, value)
	}
	if len(files) > 0 {
		for path, content := range files {
			full := filepath.Join(clone, path)
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
			require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
			gitIn(t, clone, "add", path)
		}
		gitIn(t, clone, "commit", "--quiet", "-m", "outside: files")
		gitIn(t, clone, "push", "--quiet", "origin", "HEAD:main")
	}
	require.NoError(t, f.worker.ensureRepositoryInitialized(f.worker.ctx))
	e.loop = newBranchWorkerEventLoop(f.worker, 0)
	t.Cleanup(e.loop.stopTimers)
	return e
}

// echoFile is where the worker keeps the ConfigMap name of team-a.
func echoFile(name string) string { return echoTarget + "/default/configmaps/" + name + ".yaml" }

// echoDocument is the document the worker writes for echoEvent(name, value).
func echoDocument(name, value string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name +
		"\n  namespace: default\ndata:\n  key: " + value + "\n"
}

// echoEvent is a live update of the ConfigMap name to value, by alice, for team-a.
func echoEvent(name, value string) Event {
	event := configMapTargetEvent(name, "alice", echoTarget)
	event.Operation = string(itypes.OperationUpdate)
	event.Object.Object["data"] = map[string]interface{}{"key": value}
	return event
}

// echoDesired is a snapshot entry of the ConfigMap name holding value.
func echoDesired(name, value string) manifestanalyzer.DesiredResource {
	return manifestanalyzer.DesiredResource{
		Resource: echoEvent(name, value).Identifier,
		Object:   echoEvent(name, value).Object,
	}
}

// pushFromOutside commits one document to main from another clone, and returns the commit.
func (f *echoFixture) pushFromOutside(name, value string) string {
	f.t.Helper()
	gitIn(f.t, f.clone, "pull", "--quiet", "--ff-only", "origin", "main")
	full := filepath.Join(f.clone, echoFile(name))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(f.t, os.WriteFile(full, []byte(echoDocument(name, value)), 0o600))
	gitIn(f.t, f.clone, "add", echoFile(name))
	gitIn(f.t, f.clone, "commit", "--quiet", "-m", "outside: "+name+"="+value)
	gitIn(f.t, f.clone, "push", "--quiet", "origin", "HEAD:main")
	return gitIn(f.t, f.clone, "rev-parse", "HEAD")
}

// onMain is the document main holds at file, or "" when it holds none.
func (f *echoFixture) onMain(file string) string {
	f.t.Helper()
	listed := gitIn(f.t, f.repoDir, "ls-tree", "--name-only", "-r", "main")
	for _, path := range splitLines(listed) {
		if path == file {
			return gitIn(f.t, f.repoDir, "show", "main:"+file) + "\n"
		}
	}
	return ""
}

// changedSince is the documents main changed after commit: the folder's bootstrap files, which a
// first write stages, are left out.
func (f *echoFixture) changedSince(commit string) []string {
	f.t.Helper()
	var documents []string
	for _, file := range splitLines(gitIn(f.t, f.repoDir, "diff", "--name-only", commit, "main")) {
		if strings.HasPrefix(file, echoTarget+"/default/") {
			documents = append(documents, file)
		}
	}
	return documents
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// writeWindow sends one window of live writes, closed at once (the loop's window is zero).
func (f *echoFixture) writeWindow(events ...Event) {
	f.loop.handleQueueItem(WorkItem{Request: &WriteRequest{Events: events}})
}

// TestApplierEchoes_AnUnchangedObjectIsNoAction is F2 for a window, without timing luck: the write
// is decided inside the push cooldown, the remote moves, and the push is rejected and replayed. An
// object whose plan changed nothing is no action, so the replay never plans it onto the newer tip,
// where it would revert what that tip changed.
func TestApplierEchoes_AnUnchangedObjectIsNoAction(t *testing.T) {
	t.Run("alone, it is never retained", func(t *testing.T) {
		f := newEchoFixture(t, "echo-unchanged-alone", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
		holdPushes(f.loop)

		f.writeWindow(echoEvent("x", "x1")) // the applier's echo of what Git holds
		require.Empty(t, f.loop.pendingWrites, "a write that changes nothing settles at once")

		outside := f.pushFromOutside("x", "x2")
		f.loop.pushPending()

		assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")), "the Git-side push survives")
		assert.Empty(t, f.changedSince(outside), "and nothing was committed over it")
	})

	t.Run("beside a changed object, only the changed one is committed", func(t *testing.T) {
		f := newEchoFixture(t, "echo-unchanged-batch", configv1alpha3.PruneOnEvent,
			map[string]string{"x": "x1", "y": "y1"})
		holdPushes(f.loop)

		f.writeWindow(echoEvent("x", "x1"), echoEvent("y", "y2"))
		require.Len(t, f.loop.pendingWrites, 1)
		require.Len(t, f.loop.pendingWrites[0].Events, 1, "the write keeps the object that changed")
		assert.Equal(t, "y", f.loop.pendingWrites[0].Events[0].Identifier.Name)

		outside := f.pushFromOutside("x", "x2")
		f.loop.pushPending()
		require.Empty(t, f.loop.pendingWrites, "published after the replay")

		assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")), "the Git-side push survives the replay")
		assert.Equal(t, echoDocument("y", "y2"), f.onMain(echoFile("y")))
		assert.Equal(t, []string{echoFile("y")}, f.changedSince(outside))
	})

	t.Run("a save keeps every object it captured", func(t *testing.T) {
		f := newEchoFixture(t, "echo-unchanged-save", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
		holdPushes(f.loop)
		save := attachReq("alice", time.Hour)
		save.GitTargetName = echoTarget
		save.MaxDuration = 0
		serviceAttach(f.loop, save)

		f.writeWindow(echoEvent("x", "x1"))
		require.Len(t, f.loop.pendingWrites, 1, "a save is never trimmed")
		require.Len(t, f.loop.pendingWrites[0].Events, 1)

		f.pushFromOutside("x", "x2")
		f.loop.pushPending()

		res, resolved := outcome(t, f.worker)
		require.True(t, resolved)
		assert.Equal(t, FinalizeCommitted, res.Outcome, "the replay re-asserted the save over the push")
		assert.Equal(t, echoDocument("x", "x1"), f.onMain(echoFile("x")), "API wins for a save (C2)")
	})
}

// TestApplierEchoes_AResyncReplayAppliesItsDecidedActions is F2 and P3 for a resync, under
// prune.mode: Always: the snapshot decides once, on its tree, which objects to upsert and which
// documents to sweep, and a replay applies exactly those. A document a newer tip added survives,
// and an object the snapshot found unchanged is left as the newer tip has it.
func TestApplierEchoes_AResyncReplayAppliesItsDecidedActions(t *testing.T) {
	f := newEchoFixture(t, "echo-resync-replay", configv1alpha3.PruneAlways,
		map[string]string{"x": "x1", "y": "y1", "gone": "g1"})
	holdPushes(f.loop)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{echoDesired("x", "x1"), echoDesired("y", "y2")},
		ResourceVersion:    "7",
		GitTargetName:      echoTarget,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	f.loop.handleQueueItem(resyncItem(req))
	result := <-req.Result
	require.NoError(t, result.Err)
	assert.Equal(t, 1, result.Stats.Updated)
	assert.Equal(t, 1, result.Stats.Deleted, "the snapshot's sweep removed the document the cluster lacks")
	require.Len(t, f.loop.pendingWrites, 1)
	actions := f.loop.pendingWrites[0].resyncActions
	require.NotNil(t, actions)
	require.Len(t, actions.upserts, 1, "the unchanged object is no action")
	assert.Equal(t, "y", actions.upserts[0].Resource.Name)
	require.Len(t, actions.deletes, 1)
	assert.Equal(t, "gone", actions.deletes[0].resource.Name)

	f.pushFromOutside("z", "z1")
	outside := f.pushFromOutside("x", "x2")
	f.loop.pushPending()
	require.Empty(t, f.loop.pendingWrites, "published after the replay")

	assert.Equal(t, echoDocument("z", "z1"), f.onMain(echoFile("z")), "a Git-side addition survives the replay")
	assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")), "an object the snapshot left alone stays")
	assert.Equal(t, echoDocument("y", "y2"), f.onMain(echoFile("y")))
	assert.Empty(t, f.onMain(echoFile("gone")), "the decided delete still lands")
	assert.ElementsMatch(t, []string{echoFile("y"), echoFile("gone")}, f.changedSince(outside))
}

// TestApplierEchoes_AReplayedResyncObeysATightenedPruneMode pins that a replayed sweep delete is
// gated by the policy in force at the replay: an operator who stopped pruning since the resync was
// decided is obeyed.
func TestApplierEchoes_AReplayedResyncObeysATightenedPruneMode(t *testing.T) {
	f := newEchoFixture(t, "echo-resync-tightened", configv1alpha3.PruneAlways,
		map[string]string{"y": "y1", "gone": "g1"})
	holdPushes(f.loop)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{echoDesired("y", "y2")},
		GitTargetName:      echoTarget,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	f.loop.handleQueueItem(resyncItem(req))
	require.NoError(t, (<-req.Result).Err)
	require.Len(t, f.loop.pendingWrites, 1)

	var target configv1alpha3.GitTarget
	require.NoError(t, f.worker.Client.Get(f.worker.ctx, keyOf(echoTarget), &target))
	target.Spec.Prune = &configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneNever}
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, &target))

	f.pushFromOutside("z", "z1")
	f.loop.pushPending()
	require.Empty(t, f.loop.pendingWrites)

	assert.Equal(t, echoDocument("gone", "g1"), f.onMain(echoFile("gone")),
		"pruning stopped, so the delete is not replayed")
	assert.Equal(t, echoDocument("y", "y2"), f.onMain(echoFile("y")))
}

// keyOf is the key of a GitTarget in the default namespace.
func keyOf(name string) client.ObjectKey { return client.ObjectKey{Namespace: "default", Name: name} }

// withClaim names a FluxClaim on team-a and gives the worker a requester that reports nothing,
// so the test reports for it. The loop's commit window is an hour, so a window stays open until
// something closes it.
func (f *echoFixture) withClaim() *recordingRequester {
	f.t.Helper()
	var target configv1alpha3.GitTarget
	require.NoError(f.t, f.worker.Client.Get(f.worker.ctx, keyOf(echoTarget), &target))
	target.Spec.ApplierClaimRef = testApplier(echoTarget)
	require.NoError(f.t, f.worker.Client.Update(f.worker.ctx, &target))
	createTestClaim(f.t, f.worker.Client, &target)
	requester := &recordingRequester{manual: true}
	f.worker.applyRequester = requester
	f.loop.stopTimers()
	f.loop = newBranchWorkerEventLoop(f.worker, time.Hour)
	f.t.Cleanup(f.loop.stopTimers)
	f.loop.followRecordedOperations(time.Now())
	// The converges the start and the naming left pending, answered already: these tests drive
	// other reasons.
	f.worker.inbox.take()
	for _, state := range f.loop.appliers {
		state.converge = false
	}
	return requester
}

// echoApplier is the applier team-a's applier claim holds.
func (f *echoFixture) echoApplier() *applierState {
	f.t.Helper()
	require.Len(f.t, f.loop.appliers, 1)
	for _, state := range f.loop.appliers {
		return state
	}
	return nil
}

// TestApplierEchoes_FlushThenApply is S1, driven in the worker: when an apply comes due, the open
// window is closed and the writes up to the flush mark are published before the send, cooldown or
// not, so the apply holds them. An edit decided after the mark does not hold the send: it is recorded
// in the apply window, and the apply landing over it is withheld, with a Repair pending.
func TestApplierEchoes_FlushThenApply(t *testing.T) {
	f := newEchoFixture(t, "echo-flush", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1", "y": "y1"})
	requester := f.withClaim()
	holdPushes(f.loop) // the push cooldown runs: a flush does not wait for it

	f.writeWindow(echoEvent("x", "x2")) // E0, in the open window when the apply comes due
	require.NotNil(t, f.loop.windows.only())
	restore := failPushes(t, f.worker)
	f.echoApplier().repair = true
	f.loop.endWake(0)

	assert.Nil(t, f.loop.windows.only(), "the apply closed the open window")
	assert.Empty(t, requester.taken(), "the writes up to the mark are not published yet")
	require.NotNil(t, f.loop.flushEdits, "edits are recorded from the mark")

	f.writeWindow(echoEvent("y", "y2")) // E1, decided after the mark
	require.NotNil(t, f.loop.windows.only(), "it opens a window of its own")

	restore()
	f.loop.retry.due = time.Now().Add(-time.Second)
	f.loop.runRetry()
	f.loop.endWake(0)

	sent := requester.taken()
	require.Len(t, sent, 1, "the writes up to the mark are published, and E1's open window does not hold the send")
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, sent[0].Reason)
	assert.Equal(t, echoDocument("x", "x2"), f.onMain(echoFile("x")), "the apply holds E0")
	assert.Equal(t, gitIn(t, f.repoDir, "rev-parse", "main"), sent[0].Commit, "for the tip the flush produced")
	assert.Equal(t, echoDocument("y", "y1"), f.onMain(echoFile("y")), "E1 is not published yet")

	// The apply writes the commit over E1, the applier's manager alone and newer than the edit.
	late := echoEvent("y", "y1")
	late.LastFieldManagers = []string{FluxFieldManager}
	late.LastFieldManagedAt = time.Now().Add(time.Hour)
	assert.True(t, f.loop.withholdLateApply(late), "E1 was recorded in the window: the apply over it is withheld")
	assert.True(t, f.echoApplier().repair, "and a Repair puts E1 back once it is published")
}

// TestApplierEchoes_AnApplierThatJoinsAFlushHoldsTheEditsSinceItsMark is S1 for an applier that comes
// due while a flush waits for its publication. It is sent in that flush, for the tip it produces, so
// an edit decided after the mark and before it came due is not in the commit it applies either. Its
// window recorded edits only from when it joined: the apply over such an edit was taken for the apply
// landing, not mirrored, and no Repair put the edit back on the cluster.
func TestApplierEchoes_AnApplierThatJoinsAFlushHoldsTheEditsSinceItsMark(t *testing.T) {
	const late = "team-b"
	f := newEchoFixture(t, "echo-flush-join", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
	// Its folder is on the branch already, so the barrier holds nothing back for it.
	createGitTargetWithPruneMode(t, f.worker, late, echoTarget, configv1alpha3.PruneOnEvent)
	var lateTarget configv1alpha3.GitTarget
	require.NoError(t, f.worker.Client.Get(f.worker.ctx, keyOf(late), &lateTarget))
	lateTarget.Spec.ApplierClaimRef = testApplier(late)
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, &lateTarget))
	createTestClaim(t, f.worker.Client, &lateTarget)
	requester := f.withClaim()
	require.Len(t, f.loop.appliers, 2)
	applierFor := func(target string) *applierState {
		for _, state := range f.loop.appliers {
			if state.covers(Event{GitTargetName: target, GitTargetNamespace: "default"}) {
				return state
			}
		}
		t.Fatalf("no applier covers %s", target)
		return nil
	}
	holdPushes(f.loop)

	f.writeWindow(echoEvent("x", "x2"))
	restore := failPushes(t, f.worker)
	applierFor(echoTarget).repair = true
	f.loop.endWake(0)
	require.Empty(t, requester.taken(), "the flush waits for its publication")

	edit := configMapTargetEvent("y", "alice", late)
	edit.Operation = string(itypes.OperationUpdate)
	f.writeWindow(edit) // decided after the mark, before team-b's applier comes due
	applierFor(late).repair = true
	f.loop.endWake(0)
	require.Empty(t, requester.taken())

	restore()
	f.loop.retry.due = time.Now().Add(-time.Second)
	f.loop.runRetry()
	f.loop.endWake(0)
	require.Len(t, requester.taken(), 2, "both appliers are sent in the one flush")

	applierFor(late).repair = false
	over := configMapTargetEvent("y", "alice", late)
	over.Operation = string(itypes.OperationUpdate)
	over.LastFieldManagers = []string{FluxFieldManager}
	over.LastFieldManagedAt = time.Now().Add(time.Hour)
	assert.True(t, f.loop.withholdLateApply(over), "the apply over the edit is withheld")
	assert.True(t, applierFor(late).repair, "and a Repair puts the edit back once it is published")
}

// TestApplierEchoes_AFolderNotOnTheBranchIsCreatedBeforeItsApplierIsAsked is F-d: a GitTarget that
// names its claim after its snapshots settled empty has no folder, since only a resync of a GitTarget
// naming a claim creates one with no object. Its applier is not asked to apply a missing path: the
// barrier holds it, says so (F-a), and asks for one fresh snapshot, which is used even with a foreign
// commit pending, because the apply held for the folder would wait on it.
func TestApplierEchoes_AFolderNotOnTheBranchIsCreatedBeforeItsApplierIsAsked(t *testing.T) {
	f := newEchoFixture(t, "echo-folder-missing", configv1alpha3.PruneOnEvent, nil)
	var asked []itypes.ResourceReference
	f.worker.resyncRecheck = func(target itypes.ResourceReference) { asked = append(asked, target) }
	requester := f.withClaim()
	state := f.echoApplier()
	state.foreign = true

	f.loop.endWake(0)
	f.loop.endWake(0)
	assert.Empty(t, requester.taken(), "the folder is not on the branch")
	assert.Equal(t, []itypes.ResourceReference{itypes.NewResourceReference(echoTarget, "default")}, asked,
		"one fresh snapshot is asked for, once")
	assert.Contains(t, state.held, "is not on the branch", "the hold says why")
	assert.EqualValues(t, 1, f.worker.heldAppliers.Load())

	req := &ResyncRequest{GitTargetName: echoTarget, GitTargetNamespace: "default",
		Result: make(chan ResyncResult, 1)}
	f.loop.handleResyncRequest(req)
	f.loop.endWake(0)
	require.NoError(t, (<-req.Result).Err, "used, not discarded for the pending foreign commit")
	assert.NotEmpty(t, f.onMain(echoTarget+"/README.md"), "its resync created the folder")

	f.loop.endWake(0)
	sent := requester.taken()
	require.Len(t, sent, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, sent[0].Reason)
	assert.Empty(t, state.held, "the hold lifted")
	assert.Zero(t, f.worker.heldAppliers.Load())
}

// TestApplierEchoes_AResyncThatAddsAnObjectAdoptsIt is S2: an object a resync adds to Git runs on
// the cluster already, and its applier never applied it, so a later Git-side delete would leave it
// there. It is adopted; a resync that only updates adopts nothing.
func TestApplierEchoes_AResyncThatAddsAnObjectAdoptsIt(t *testing.T) {
	f := newEchoFixture(t, "echo-resync-adopt", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
	f.withClaim()

	resync := func(desired ...manifestanalyzer.DesiredResource) {
		req := &ResyncRequest{Desired: desired, GitTargetName: echoTarget, GitTargetNamespace: "default",
			Result: make(chan ResyncResult, 1)}
		f.loop.applyResync(req)
		f.loop.advance()
		require.NoError(t, (<-req.Result).Err)
	}
	resync(echoDesired("x", "x2"))
	assert.False(t, f.echoApplier().adopt, "an update adopts nothing")

	resync(echoDesired("x", "x2"), echoDesired("new", "n1"))
	assert.True(t, f.echoApplier().adopt, "the object the resync added is adopted")
}

// TestApplierEchoes_ASuspendedResyncIsNotRetained: a suspended target's resync scans and writes
// nothing, so it decides nothing: it settles at once, and is neither retained nor pushed.
func TestApplierEchoes_ASuspendedResyncIsNotRetained(t *testing.T) {
	f := newEchoFixture(t, "echo-resync-suspended", configv1alpha3.PruneOnEvent, map[string]string{"x": "x1"})
	var target configv1alpha3.GitTarget
	require.NoError(t, f.worker.Client.Get(f.worker.ctx, keyOf(echoTarget), &target))
	target.Spec.Suspend = true
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, &target))

	req := &ResyncRequest{Desired: []manifestanalyzer.DesiredResource{echoDesired("x", "x2")},
		GitTargetName: echoTarget, GitTargetNamespace: "default", Result: make(chan ResyncResult, 1)}
	f.loop.applyResync(req)
	f.loop.advance()
	require.NoError(t, (<-req.Result).Err)
	assert.Empty(t, f.loop.pendingWrites, "nothing to publish")
	assert.True(t, f.loop.lastPushAt.IsZero(), "so nothing was pushed")
}

// TestApplierEchoes_AnAppliedFolderIsCreatedByItsResync: a folder an applier applies must exist
// before the applier is asked to apply it, or Argo CD retries a sync of a missing path for minutes
// and Flux fails it. A resync of a GitTarget naming an applier claim creates the folder with its bootstrap
// files, with no object to write; once it exists, an empty resync commits nothing. A GitTarget no
// applier applies keeps the old rule: an empty resync commits nothing.
func TestApplierEchoes_AnAppliedFolderIsCreatedByItsResync(t *testing.T) {
	// Initial: the seed below is a foreign commit the resync's own fetch finds, which F3 discards a
	// later snapshot for; the first one of a collection is the baseline, and is used.
	emptyResync := func(f *echoFixture) {
		t.Helper()
		req := &ResyncRequest{GitTargetName: echoTarget, GitTargetNamespace: "default", Initial: true,
			Result: make(chan ResyncResult, 1)}
		f.loop.applyResync(req)
		f.loop.advance()
		require.NoError(t, (<-req.Result).Err)
	}
	seed := func(f *echoFixture) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(f.clone, "NOTES.md"), []byte("elsewhere\n"), 0o600))
		gitIn(t, f.clone, "add", "NOTES.md")
		gitIn(t, f.clone, "commit", "--quiet", "-m", "outside: a file outside the folder")
		gitIn(t, f.clone, "push", "--quiet", "origin", "HEAD:main")
	}
	main := func(f *echoFixture) string { return gitIn(t, f.repoDir, "rev-parse", "main") }

	applied := newEchoFixture(t, "echo-bootstrap-applied", configv1alpha3.PruneOnEvent, nil)
	seed(applied)
	applied.withClaim()
	emptyResync(applied)
	assert.NotEmpty(t, applied.onMain(echoTarget+"/README.md"), "the folder exists, with its bootstrap files")
	created := main(applied)
	emptyResync(applied)
	assert.Equal(t, created, main(applied), "a folder that exists needs no commit")

	plain := newEchoFixture(t, "echo-bootstrap-plain", configv1alpha3.PruneOnEvent, nil)
	seed(plain)
	before := main(plain)
	emptyResync(plain)
	assert.Equal(t, before, main(plain), "no applier applies it: an empty resync commits nothing")
	assert.Empty(t, plain.onMain(echoTarget+"/README.md"))
}
