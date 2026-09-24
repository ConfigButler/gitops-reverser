// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestDeclareForce_ARequestArrivingMidPassSurvivesIt is the hazard a boolean could not express.
//
// A pass plans against a COPY of the declare record, so a recovery request raised while it runs
// cannot be the one it is about to satisfy: the streams it installs were chosen before that
// request existed. Acknowledging "a force was pending" rather than "this request was consumed"
// therefore reported a recovery that never happened — and because the dirty sequence schedules
// the next pass without one, the target kept its existing streams, nothing replayed, and a branch
// whose worker had just been replaced could stay unpopulated.
func TestDeclareForce_ARequestArrivingMidPassSurvivesIt(t *testing.T) {
	m := &Manager{}
	ref := types.NewResourceReference("target", "tenant")
	m.declareIntentFor(ref, "default", "", v1alpha3.PruneOnEvent, false)

	m.RequestRecheckForGitTarget(ref)
	planned := *m.triggers().declares[ref.Key()]
	require.True(t, planned.forcePending(), "precondition: the pass plans a forced recheck")

	// The branch worker is replaced again while that pass is in flight.
	m.RequestRecheckForGitTarget(ref)
	m.clearDeclareForce(ref, planned.forceRequests)

	assert.True(t, m.triggers().declares[ref.Key()].forcePending(),
		"the second request was not satisfied by a pass planned before it arrived")

	// The next pass consumes it, and only then is the target settled.
	second := *m.triggers().declares[ref.Key()]
	m.clearDeclareForce(ref, second.forceRequests)
	assert.False(t, m.triggers().declares[ref.Key()].forcePending(),
		"a pass that planned against the request does satisfy it")
	assert.Empty(t, m.ForcedRecheckTargetsForTest())
}

// TestDeclareForce_AFailedPassLeavesTheRequestStanding is the other half of the same rule: only a
// pass that reached the data plane acknowledges anything, so a failure cannot consume a recovery.
func TestDeclareForce_AFailedPassLeavesTheRequestStanding(t *testing.T) {
	m := &Manager{}
	ref := types.NewResourceReference("target", "tenant")
	m.declareIntentFor(ref, "default", "", v1alpha3.PruneOnEvent, true)

	require.Equal(t, []string{ref.Key()}, m.ForcedRecheckTargetsForTest())

	// A pass fails: applyTargetPlan returns before it acknowledges anything.
	assert.True(t, m.triggers().declares[ref.Key()].forcePending())

	// The level-triggered declare repeats, and the request is still there to be consumed.
	m.declareIntentFor(ref, "default", "", v1alpha3.PruneOnEvent, false)
	assert.True(t, m.triggers().declares[ref.Key()].forcePending(),
		"a re-declare that asks for nothing new must not drop what is outstanding")
}

// TestDeclareForce_AnUndeclaredTargetNeedsNoRecovery keeps RequestRecheckForGitTarget from
// inventing an intent: a target that has not declared starts its streams on its first pass, and a
// stream that starts replays.
func TestDeclareForce_AnUndeclaredTargetNeedsNoRecovery(t *testing.T) {
	m := &Manager{}
	m.RequestRecheckForGitTarget(types.NewResourceReference("never-declared", "tenant"))

	assert.Empty(t, m.ForcedRecheckTargetsForTest())
	assert.Empty(t, m.triggers().declares)
}
