// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// S2 (docs/design/applier-echoes.md): a CatchUp only adopts objects the API created and converges,
// for Flux and Argo CD alike, decided in the branch worker; no apply goes out before every initial
// snapshot is published.

// appliedAt is a trigger whose last recorded operation succeeded, applying revision.
func appliedAt(revision string) func(*configv1alpha3.FluxTrigger) {
	return recording(&configv1alpha3.ApplyOperation{
		Phase: configv1alpha3.ApplyOperationSucceeded, Reason: configv1alpha3.ApplyReasonCatchUp,
		Commit: "older", AppliedRevision: revision,
	}, false)
}

// createdEvent is a live ADDED of a Deployment in target's folder, written last by managers.
func createdEvent(target string, managers ...string) Event {
	event := windowEvent(target, "1", managers...)
	event.Operation = string(itypes.OperationCreate)
	return event
}

// TestCatchUp_TheStartupConvergeAsksNothingOfAnApplierAtTheTip: the first observation leaves a
// converge pending for every applier. One whose trigger recorded the tip as applied is asked
// nothing; the other is asked a CatchUp of the tip.
func TestCatchUp_TheStartupConvergeAsksNothingOfAnApplierAtTheTip(t *testing.T) {
	loop, requester := manualLoop(t,
		applierTestTarget("fresh", true),
		applierTestTarget("current", true), fluxTrigger("current", appliedAt("main@sha1:AAA")),
	)
	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	loop.serviceAppliers()

	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, applierRef("fresh"), requests[0].Applier)
	assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, requests[0].Reason)
	assert.Equal(t, "aaa", requests[0].Commit)
	assert.False(t, loop.appliers[applierRef("current")].converge, "already applied: answered")

	report(requests[0], ApplySucceeded)
	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "the worker's own push asks nothing")
}

// TestCatchUp_AnAPICreateIsAdoptedEvenAtTheTip: an object the API created carries no owner labels and
// no tracking annotation, so its applier is asked to apply again, even the commit it applied last:
// that is what stamps them. A create of the applier's own, or of an object another applier owns,
// adopts nothing. The same holds for an Argo CD Application.
func TestCatchUp_AnAPICreateIsAdoptedEvenAtTheTip(t *testing.T) {
	for name, tc := range map[string]struct {
		target  *configv1alpha3.GitTarget
		manager string
	}{
		"flux":    {applierTestTarget("alpha", true), FluxFieldManager},
		"argo cd": {applierTestTarget("alpha", true, argoApplier), ArgoCDFieldManager},
	} {
		t.Run(name, func(t *testing.T) {
			loop, requester := manualLoop(t, tc.target)
			loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
			loop.serviceAppliers()
			converge := requester.taken()
			require.Len(t, converge, 1)
			converge[0].Report(ApplyResult{ID: converge[0].ID, Outcome: ApplySucceeded, AppliedRevision: "aaa"})
			loop.serviceAppliers()
			require.Empty(t, requester.taken())

			ownApply := createdEvent("alpha", tc.manager)
			loop.handleLiveEvents(&WriteRequest{Events: []Event{ownApply}, CommitMode: CommitModePerEvent})
			owned := createdEvent("alpha", "kubectl-create")
			owned.AppliedBy = AppliedBy{Kind: configv1alpha3.ApplierKindKustomization, Namespace: "x", Name: "other"}
			loop.handleLiveEvents(&WriteRequest{Events: []Event{owned}, CommitMode: CommitModePerEvent})
			loop.openWindow = nil
			loop.serviceAppliers()
			require.Empty(t, requester.taken(), "neither is an API create of the folder's own objects")

			loop.handleLiveEvents(&WriteRequest{
				Events: []Event{createdEvent("alpha", "kubectl-create")}, CommitMode: CommitModePerEvent,
			})
			loop.openWindow = nil
			loop.serviceAppliers()
			adopt := requester.taken()
			require.Len(t, adopt, 1, "adopted, although the applier is at the tip")
			assert.Equal(t, configv1alpha3.ApplyReasonCatchUp, adopt[0].Reason)
			assert.Equal(t, "aaa", adopt[0].Commit)
		})
	}
}

// TestCatchUp_NothingIsAskedBeforeTheInitialSnapshotsArePublished: every reason waits, a
// ForeignCommit included, until every collection of every GitTarget the applier covers has delivered
// its snapshot and the worker published what it made of it; the barrier is looked at again on a
// short timer.
func TestCatchUp_NothingIsAskedBeforeTheInitialSnapshotsArePublished(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	delivered := false
	loop.w.initialSnapshots = func(itypes.ResourceReference) bool { return delivered }
	observeForeignCommit(loop.w)

	loop.serviceAppliers()
	assert.Empty(t, requester.taken())
	assert.NotNil(t, loop.applierTimer, "the barrier is looked at again")
	assert.True(t, loop.appliers[applierRef("alpha")].foreign, "the reason stays pending")

	delivered = true
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1)
	assert.Equal(t, configv1alpha3.ApplyReasonForeignCommit, requests[0].Reason)
}

// TestCatchUp_AnEmptyTipAsksNothing: the remote does not carry the branch, so there is no commit to
// apply; the source reports the missing branch itself. The converge stays pending for when it does.
func TestCatchUp_AnEmptyTipAsksNothing(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	loop.w.inbox.observe(RemoteObservation{By: ObservedByFetch})
	loop.serviceAppliers()
	assert.Empty(t, requester.taken())
	assert.True(t, loop.appliers[applierRef("alpha")].converge)

	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByPush})
	loop.serviceAppliers()
	requests := requester.taken()
	require.Len(t, requests, 1, "the push that creates the branch gives it a commit")
	assert.Equal(t, "aaa", requests[0].Commit)
}

// TestCatchUp_AFirstNamingConvergesOnce: a GitTarget that names an applier already listed leaves one
// converge pending; listing it again leaves none.
func TestCatchUp_AFirstNamingConvergesOnce(t *testing.T) {
	loop, requester := manualLoop(t, applierTestTarget("alpha", true))
	loop.w.inbox.observe(RemoteObservation{Commit: "aaa", By: ObservedByFetch})
	loop.serviceAppliers()
	first := requester.taken()
	require.Len(t, first, 1)
	first[0].Report(ApplyResult{ID: first[0].ID, Outcome: ApplySucceeded, AppliedRevision: "main@sha1:aaa"})
	loop.serviceAppliers()

	loop.w.inbox.observe(RemoteObservation{Commit: "bbb", By: ObservedByPush})
	shared := applierTestTarget("bravo", true,
		func(g *configv1alpha3.GitTarget) { g.Spec.ApplyTriggerRef = testApplier("alpha") })
	require.NoError(t, loop.w.Client.Create(t.Context(), shared))
	loop.serviceAppliers()
	second := requester.taken()
	require.Len(t, second, 1, "bravo names alpha's trigger for the first time")
	assert.Equal(t, "bbb", second[0].Commit)
	report(second[0], ApplySucceeded)

	loop.serviceAppliers()
	assert.Empty(t, requester.taken(), "listing it again converges nothing")
}

func TestRevisionNamesCommit_ReadsEveryForm(t *testing.T) {
	for _, revision := range []string{"main@sha1:ABC", "sha1:abc", "main/abc", "feature/x/abc", "abc"} {
		assert.True(t, revisionNamesCommit(revision, "abc"), revision)
	}
	assert.False(t, revisionNamesCommit("main@sha1:abd", "abc"))
	assert.False(t, revisionNamesCommit("", "abc"))
	assert.False(t, revisionNamesCommit("abc", ""))
}
