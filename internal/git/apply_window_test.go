// SPDX-License-Identifier: Apache-2.0

package git

// The apply window: while an apply GitOps Reverser asked for runs, the applier's own write over an
// object the API side edited since the request was sent is withheld, so the API value wins. Only a
// proven applier write is withheld; one that could be the edit itself is mirrored and counted. See
// apply_window.go.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// windowClock is when the managedFields entries of the last windowEvent were written.
var windowClock time.Time

// windowEvent is a live UPDATE to the Deployment "api" in target's folder, written last by managers,
// a second after the previous one and never before now, as the API server stamps them: an applier's
// entry older than the attempt's send is an earlier apply's (apply_window.go).
func windowEvent(target, replicas string, managers ...string) Event {
	windowClock = latest(windowClock, time.Now().Truncate(time.Second)).Add(time.Second)
	return windowEventAt(target, replicas, windowClock, managers...)
}

// windowEventAt is windowEvent, its managers' latest entries written at.
func windowEventAt(target, replicas string, at time.Time, managers ...string) Event {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "shop"},
		"spec":     map[string]any{"replicas": replicas},
	}}
	return Event{
		Object:             obj,
		Identifier:         itypes.NewResourceIdentifier("apps", "v1", "deployments", "shop", "api"),
		Operation:          string(itypes.OperationUpdate),
		LastFieldManagers:  managers,
		LastFieldManagedAt: at,
		GitTargetName:      target,
		GitTargetNamespace: "team-a",
		Path:               "apps/" + target,
	}
}

// sentAttempt is a loop with alpha's applier sent an attempt that is still out.
func sentAttempt(t *testing.T) (*branchWorkerEventLoop, ApplyRequest) {
	t.Helper()
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	out := requester.taken()
	require.Len(t, out, 1)
	return loop, out[0]
}

func echoes(t *testing.T, reader *sdkmetric.ManualReader, outcome string) int64 {
	t.Helper()
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_apply_echoes_total",
		map[string]string{"outcome": outcome})
	return got
}

func collisions(t *testing.T, reader *sdkmetric.ManualReader, outcome string) int64 {
	t.Helper()
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_apply_window_collisions_total",
		map[string]string{"outcome": outcome})
	return got
}

// TestApplyWindow_TheAppliersLateWriteOverANewerEditIsWithheld is the race the window exists for:
// Flux applies the commit it was asked for after an API edit landed, and that write must not reach
// Git as Flux's revert of the edit.
func TestApplyWindow_TheAppliersLateWriteOverANewerEditIsWithheld(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "an API edit is mirrored")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the applier's write over it is withheld")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
}

// TestApplyWindow_AnUndoOfAnEditIsMirrored is review 7's W2. Inside a window, `kubectl label cm/x
// tmp=1` is mirrored; `kubectl label cm/x tmp-` then removes every field kubectl-label owns, which
// deletes its managedFields entry, so the newest entry left is kustomize-controller's, from an
// earlier apply. That undo looked like the applier's write alone over a newer edit: it was withheld
// and a Repair sent, which put tmp=1 back. A real late apply moves the applier's entry forward; an
// undo does not. So only an applier entry at or after the edit is withheld: in the same second the
// edit, which arrived first, was written first, and the applier's write alone is its apply over it.
func TestApplyWindow_AnUndoOfAnEditIsMirrored(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)
	earlierApply, edit := time.Unix(1_800_000_000, 0), time.Unix(1_800_000_100, 0)

	assert.False(t, loop.withholdLateApply(windowEventAt("alpha", "3", edit, "kubectl-label")), "the edit")
	assert.False(t, loop.withholdLateApply(windowEventAt("alpha", "2", earlierApply, FluxFieldManager)),
		"its undo leaves only an older entry of the applier's, and is mirrored")
	assert.False(t, loop.appliers[applierRef("alpha")].repair, "so no Repair puts the edit back")
	assert.True(t, loop.withholdLateApply(windowEventAt("alpha", "2", edit, FluxFieldManager)),
		"the applier's write alone in the same second as the edit, after it, is its apply over it")
	assert.Equal(t, int64(0), collisions(t, reader, collisionAmbiguous))
	assert.True(t, loop.withholdLateApply(windowEventAt("alpha", "2", edit.Add(time.Second), FluxFieldManager)),
		"an apply arriving after the edit moves the applier's entry forward, and is withheld")
	assert.Equal(t, int64(2), collisions(t, reader, collisionWithheld))
}

// TestApplyWindow_OnlyAWithheldWriteIsKeptFromTheCommitWindow follows both events through the live
// path: the commit window ends up holding the API edit, not Flux's revert of it.
func TestApplyWindow_OnlyAWithheldWriteIsKeptFromTheCommitWindow(t *testing.T) {
	loop, _ := sentAttempt(t)

	loop.handleLiveEvents(&WriteRequest{Events: []Event{
		windowEvent("alpha", "3", "kubectl-edit"),
		windowEvent("alpha", "2", FluxFieldManager),
	}})

	require.NotNil(t, loop.openWindow)
	require.Len(t, loop.openWindow.pathToEvent, 1)
	for _, event := range loop.openWindow.pathToEvent {
		replicas, _, _ := unstructured.NestedString(event.Object.Object, "spec", "replicas")
		assert.Equal(t, "3", replicas, "Git keeps the API's value")
	}
}

// TestApplyWindow_WhatIsNeverWithheld: the applier's write is mirrored when it could be the edit
// itself, when it is a delete, and for a folder another applier (or none) applies. Its write to an
// object nobody edited since the attempt is not mirrored, and leaves no Repair: it is the apply
// itself, writing what Git holds (F1).
func TestApplyWindow_WhatIsNeverWithheld(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"an object nobody edited since the attempt: the apply writes what Git holds, and is not mirrored")
	assert.False(t, loop.appliers[applierRef("alpha")].repair, "nothing to put back")
	assert.Equal(t, int64(1), echoes(t, reader, echoNotMirrored))

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager, "kubectl-edit")),
		"a tie may be the API edit itself, and withholding that would lose it")
	assert.Equal(t, int64(1), collisions(t, reader, collisionAmbiguous))

	deleted := windowEvent("alpha", "", FluxFieldManager)
	deleted.Operation = string(itypes.OperationDelete)
	assert.False(t, loop.withholdLateApply(deleted), "a delete does not say who deleted")

	assert.False(t, loop.withholdLateApply(windowEvent("bravo", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("bravo", "2", FluxFieldManager)),
		"bravo's folder is not the applier's")
	assert.Equal(t, int64(0), collisions(t, reader, collisionWithheld))
}

// TestApplyWindow_NoWindowBeforeAnAttempt: without an attempt out, the applier applies on its own
// schedule and its writes are mirrored as before.
func TestApplyWindow_NoWindowBeforeAnAttempt(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
}

// TestApplyWindow_ClosesAGraceAfterFluxFinished: a delivered attempt keeps its window open a short
// grace, because Flux's writes reach the worker through the watch after Flux reported done; then it
// closes.
func TestApplyWindow_ClosesAGraceAfterFluxFinished(t *testing.T) {
	loop, req := sentAttempt(t)
	report(req, ApplySucceeded)
	loop.serviceAppliers()

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "within the grace")

	for _, state := range loop.appliers {
		state.window.closesAt = time.Now().Add(-time.Millisecond)
	}
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "after it")
}

// TestApplyWindow_AnAttemptThatAppliedNothingHasNoWindow: a stale, failed or dropped attempt
// stamped nothing, so nothing the applier writes afterwards is the apply it asked for.
func TestApplyWindow_AnAttemptThatAppliedNothingHasNoWindow(t *testing.T) {
	for _, outcome := range []ApplyOutcome{ApplyStale, ApplyRequestFailed, ApplyRejected, ApplyDropped} {
		t.Run(string(outcome), func(t *testing.T) {
			loop, req := sentAttempt(t)
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")))
			report(req, outcome)
			loop.serviceAppliers()
			state := loop.appliers[applierRef("alpha")]
			if state.window != nil {
				// A stale attempt is decided again at once: the window is the next attempt's.
				require.NotEqual(t, req.ID, state.window.attempt, "the attempt that applied nothing has none")
				return
			}
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
		})
	}
}

// TestApplyWindow_ArgoCDsLateWriteIsWithheldByItsOwnManagers: an Argo CD Application's window
// knows Argo CD's managers, its apply (argocd-controller) and its Replace=true create and replace
// (argocd-application-controller), and not Flux's.
func TestApplyWindow_ArgoCDsLateWriteIsWithheldByItsOwnManagers(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, requester := manualLoop(t, applierTestTarget("alpha", true, argoApplier))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	require.Len(t, requester.taken(), 1)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "an API edit is mirrored")
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"Flux is not this applier: its write is an edit like any other")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager)),
		"Argo CD's apply over the newer edit is withheld")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDDefaultFieldManager)),
		"so is its replace")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager, ArgoCDDefaultFieldManager)),
		"both of its own managers in one second are still only Argo CD")
	assert.Equal(t, int64(3), collisions(t, reader, collisionWithheld))

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", ArgoCDFieldManager, "kubectl-edit")),
		"a tie with another manager may be the edit itself")
	assert.Equal(t, int64(1), collisions(t, reader, collisionAmbiguous))
}

// TestApplyWindow_AFirstTieIsRememberedAsAnEdit: a write by the applier and another manager in the
// same second may be the API edit, so it is mirrored. It must also be remembered as one: when the
// applier then writes the object alone a second later, that is its apply reverting the edit, and is
// withheld. A tie on an object nobody edited before is not a collision, so it is not counted.
func TestApplyWindow_AFirstTieIsRememberedAsAnEdit(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager, "kubectl-edit")),
		"a tie may be the API edit itself")
	assert.Equal(t, int64(0), collisions(t, reader, collisionAmbiguous),
		"nothing was edited before it, so it collides with nothing")
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the applier's write a second later is its apply over the tie's edit")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
}

// completedThenNext is a loop whose applier, alpha's, completed attempt A over an API edit made
// while it ran, and was sent attempt B within A's grace: a push somebody else made asks it again.
func completedThenNext(t *testing.T) (*branchWorkerEventLoop, *recordingRequester, ApplyRequest) {
	t.Helper()
	loop, requester := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	observeForeignCommit(loop.w)
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	require.False(t, loop.withholdLateApply(windowEvent("alpha", "3", "kubectl-edit")), "the edit is mirrored")

	report(first[0], ApplySucceeded)
	loop.w.inbox.observe(RemoteObservation{Commit: "ccc", By: ObservedByFetch})
	loop.serviceAppliers()
	next := requester.taken()
	require.Len(t, next, 1, "B goes out within A's grace")
	require.NotEqual(t, first[0].ID, next[0].ID)
	return loop, requester, next[0]
}

// sentAfterTheEarlierApply dates the applier's current window's send an hour on, so the writes the
// next windowEvents date come before it: one attempt is out at a time, so the earlier apply's late
// writes were made before the next attempt was sent. A carried edit guards only those.
func sentAfterTheEarlierApply(t *testing.T, loop *branchWorkerEventLoop) {
	t.Helper()
	onlyApplier(t, loop).window.sentAt = time.Now().Add(time.Hour)
}

// TestApplyWindow_ANewAttemptKeepsTheGraceOfTheOneBefore is the fifth review's third finding. A new
// attempt replaced its applier's window, and the edits the one before had seen went with it, while
// that earlier apply's writes could still arrive within its grace. Its late write over the edit was
// then mirrored, and the revert reached Git as the applier's commit.
func TestApplyWindow_ANewAttemptKeepsTheGraceOfTheOneBefore(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _, _ := completedThenNext(t)
	sentAfterTheEarlierApply(t, loop)

	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"A's late write over the edit is withheld while B is out")
	assert.Equal(t, int64(1), collisions(t, reader, collisionWithheld))
	assert.True(t, onlyApplier(t, loop).repair, "and has a Repair pending")
}

// TestApplyWindow_AnAttemptThatAppliedNothingKeepsTheGraceBeforeIt: B ending without an apply has no
// window of its own, but A's grace still runs, so A's late write is withheld until it ends, and
// mirrored after.
func TestApplyWindow_AnAttemptThatAppliedNothingKeepsTheGraceBeforeIt(t *testing.T) {
	for _, outcome := range []ApplyOutcome{ApplyStale, ApplyDropped} {
		t.Run(string(outcome), func(t *testing.T) {
			loop, _, next := completedThenNext(t)
			report(next, outcome)
			holdApplies(loop) // a stale B is not sent again yet
			loop.serviceAppliers()
			state := onlyApplier(t, loop)
			require.Nil(t, state.inflight)
			sentAfterTheEarlierApply(t, loop)

			assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
				"A's late write is withheld within A's grace")
			assert.True(t, state.repair)

			state.window.closesAt = time.Now().Add(-time.Millisecond)
			assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)), "after it")
		})
	}
}

// TestApplyWindow_TheCarriedGraceEndsWhenTheEarlierWindowWouldHave: the edits a new attempt carries
// from the window before it count only until that window would have closed. After that, the
// applier's write is B's apply of a commit that holds the edit, and is mirrored; an edit made since
// B was sent still counts.
func TestApplyWindow_TheCarriedGraceEndsWhenTheEarlierWindowWouldHave(t *testing.T) {
	loop, _, _ := completedThenNext(t)
	window := onlyApplier(t, loop).window
	require.False(t, window.carriedUntil.IsZero())
	window.carriedUntil = time.Now().Add(-time.Millisecond)

	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager)),
		"A's grace is over: the applier's write is B's apply landing, which is not mirrored")
	assert.False(t, onlyApplier(t, loop).repair, "and leaves nothing to put back")
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "4", "kubectl-edit")))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "3", FluxFieldManager)),
		"an edit made while B runs is B's own")
}

// TestApplyWindow_ConsecutiveOperationsOnOneApplier composes the steps: A completes, B is sent within
// A's grace, A's late write over the edit is withheld, B completes, and the Repair that write left
// pending goes out once, after a flush, at the tip. Each window closes a grace after its own attempt.
func TestApplyWindow_ConsecutiveOperationsOnOneApplier(t *testing.T) {
	loop, requester, second := completedThenNext(t)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, second.Reason)
	sentAfterTheEarlierApply(t, loop)

	require.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the Repair waits for B")

	report(second, ApplySucceeded)
	loop.serviceAppliers()
	state := onlyApplier(t, loop)
	repair := requester.taken()
	require.Len(t, repair, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonRepair, repair[0].Reason)
	assert.Equal(t, "ccc", repair[0].Commit)
	assert.WithinDuration(t, time.Now().Add(applyWindowGrace), state.window.carriedUntil, time.Second,
		"the Repair carries B's grace, which ends a grace after B completed")

	report(repair[0], ApplySucceeded)
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the Repair is sent once")
	assert.False(t, state.repairPending())
	assert.WithinDuration(t, time.Now().Add(applyWindowGrace), state.window.closesAt, time.Second)
}

// TestApplyWindow_AnApplierEntryOlderThanTheSendIsAnEdit is F1's lower bound: an entry of the
// applier's older than every send still open was written by an earlier apply, and is newest only
// because an update removed every field the editor owned. It is the undo of an edit, mirrored, and
// it counts as one, so the apply landing over it afterwards is withheld.
func TestApplyWindow_AnApplierEntryOlderThanTheSendIsAnEdit(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := sentAttempt(t)
	beforeSend := time.Now().Add(-time.Hour).Truncate(time.Second)

	assert.False(t, loop.withholdLateApply(windowEventAt("alpha", "3", beforeSend, FluxFieldManager)),
		"an earlier apply's entry is an edit, and is mirrored")
	assert.Equal(t, int64(0), echoes(t, reader, echoNotMirrored))
	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"the apply landing over it is withheld")
	assert.True(t, loop.appliers[applierRef("alpha")].repair)
}

// TestApplyWindow_ACarriedEditDoesNotWithholdTheNextApply: B is sent within A's grace, carrying the
// edit made while A ran. B applies a commit that holds every edit published before it was sent, so
// B's own write over that object, made after B's send, is B's apply landing: not withheld, and no
// Repair. Only A's late write, made before B was sent, is withheld because of the carried edit.
func TestApplyWindow_ACarriedEditDoesNotWithholdTheNextApply(t *testing.T) {
	loop, _, _ := completedThenNext(t)
	window := onlyApplier(t, loop).window
	window.sentAt = time.Now().Add(-time.Hour) // B's send, before every write windowEvent dates

	assert.True(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)),
		"B's write is its apply landing, not mirrored")
	assert.False(t, onlyApplier(t, loop).repair, "and B is not repaired over an edit its commit holds")
}

// TestApplyWindow_ADelayedEchoOfTheAttemptBeforeIsNotMirrored: B is sent within A's grace, and A's
// apply of an object nobody edited arrives after B was sent. Its entry is older than B's send, but
// not than A's, which is still carried: it is A's apply landing, and is not mirrored.
func TestApplyWindow_ADelayedEchoOfTheAttemptBeforeIsNotMirrored(t *testing.T) {
	loop, _, _ := completedThenNext(t)
	window := onlyApplier(t, loop).window
	require.False(t, window.carriedSentAt.IsZero())
	window.sentAt = time.Now().Add(time.Hour) // B was sent after A's write
	betweenSends := window.carriedSentAt.Add(time.Second)

	echo := windowEventAt("alpha", "2", betweenSends, FluxFieldManager)
	echo.Identifier.Name = "nobody-edited"
	assert.True(t, loop.withholdLateApply(echo), "A's echo, inside A's grace")
	assert.False(t, onlyApplier(t, loop).repair)

	window.carriedUntil = time.Now().Add(-time.Millisecond)
	assert.False(t, loop.withholdLateApply(echo), "once A's grace is over the entry is older than every send")
}

// TestApplyWindow_AnApplierWriteWithNoWindowIsLate: with no apply window open, the applier's
// managers alone are mirrored as an edit, and counted late (C3).
func TestApplyWindow_AnApplierWriteWithNoWindowIsLate(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	loop.followRecordedOperations(time.Now())
	require.Len(t, loop.appliers, 1)

	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", FluxFieldManager)))
	assert.False(t, loop.withholdLateApply(windowEvent("alpha", "2", "kubectl-edit")))
	assert.False(t, loop.withholdLateApply(windowEvent("bravo", "2", FluxFieldManager)), "no applier covers bravo")
	assert.Equal(t, int64(1), echoes(t, reader, echoLate))
}

// TestApplyWindow_ClockSkew is F-e: the send is the operator's clock and a managedFields time is the
// source API server's, to the second, so a skew between the two moves an entry across the send.
// Each case runs with the server's clock behind and ahead of the operator's by up to two seconds.
func TestApplyWindow_ClockSkew(t *testing.T) {
	sent := time.Unix(1_800_000_000, 400_000_000)
	type step struct {
		at       time.Duration // operator time of the write, from the send
		managers []string
		withheld bool
		// echo is the apply landing on an object nobody edited: a server clock that dates it before
		// the send makes it look like an earlier apply's entry left by an undo, and it is mirrored as
		// an edit. That is the cost the window takes rather than ever withhold an undo.
		echo bool
	}
	cases := []struct {
		name   string
		steps  []step
		repair bool
	}{
		{
			name: "the apply lands on an object nobody edited",
			steps: []step{
				{at: 300 * time.Millisecond, managers: []string{FluxFieldManager}, withheld: true, echo: true},
			},
		},
		{
			name: "a late apply over a newer edit",
			steps: []step{
				{at: 200 * time.Millisecond, managers: []string{"kubectl-edit"}},
				{at: 800 * time.Millisecond, managers: []string{FluxFieldManager}, withheld: true},
			},
			repair: true,
		},
		{
			name: "an undo of an edit made in the window leaves an earlier apply's entry",
			steps: []step{
				{at: 200 * time.Millisecond, managers: []string{"kubectl-label"}},
				{at: -30 * time.Second, managers: []string{FluxFieldManager}},
			},
		},
		{
			name:  "an undo of an edit published before the send leaves an earlier apply's entry",
			steps: []step{{at: -10 * time.Second, managers: []string{FluxFieldManager}}},
		},
	}
	for _, skew := range []time.Duration{-2 * time.Second, -time.Second, 0, time.Second, 2 * time.Second} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s, server clock %+v", tc.name, skew), func(t *testing.T) {
				loop, _ := sentAttempt(t)
				state := loop.appliers[applierRef("alpha")]
				state.window.sentAt = sent
				for i, s := range tc.steps {
					written := sent.Add(s.at + skew).Truncate(time.Second)
					datedBeforeSend := written.Before(sent.Truncate(time.Second))
					want := s.withheld && (!s.echo || !datedBeforeSend)
					assert.Equal(t, want,
						loop.withholdLateApply(windowEventAt("alpha", "2", written, s.managers...)),
						"step %d, %v", i, s.managers)
				}
				assert.Equal(t, tc.repair, state.repair, "repair")
			})
		}
	}
}
