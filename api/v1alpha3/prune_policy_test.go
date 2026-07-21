// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEffectivePruneMode_LegacyGitTargetIsOnEvent is the compatibility contract PR 5 rests on.
//
// The CRD default only writes onEvent into a NEWLY created object; Kubernetes does not retro-fill
// stored objects, so a GitTarget written before this field existed reaches the controller with a
// nil spec.prune forever. If that read as anything other than OnEvent, every existing target
// would change behaviour on upgrade without anyone editing it — which is exactly the class of
// surprise this release exists to prevent.
func TestEffectivePruneMode_LegacyGitTargetIsOnEvent(t *testing.T) {
	legacy := &GitTarget{}
	assert.Equal(t, PruneOnEvent, legacy.EffectivePruneMode(),
		"a GitTarget that never declared spec.prune must be OnEvent, not unset")

	declaredEmpty := &GitTarget{Spec: GitTargetSpec{Prune: &PrunePolicy{}}}
	assert.Equal(t, PruneOnEvent, declaredEmpty.EffectivePruneMode(),
		"a prune object written without a mode must also be OnEvent")
}

// TestEffectivePruneMode_HonoursDeclaredMode covers the three declared values end to end through
// the GitTarget accessor, so the helper and the field cannot drift.
func TestEffectivePruneMode_HonoursDeclaredMode(t *testing.T) {
	for _, mode := range []PruneMode{PruneNever, PruneOnEvent, PruneAlways} {
		target := &GitTarget{Spec: GitTargetSpec{Prune: &PrunePolicy{Mode: mode}}}
		assert.Equal(t, mode, target.EffectivePruneMode(), "declared mode %q must survive the accessor", mode)
	}
}

// TestPruneMode_PathsMatchTheDocumentedTable pins the two-path table from
// docs/design/watchrule-source-namespace/pr5-gittarget-deletion-safety.md. The two predicates are
// deliberately independent: `onEvent` differs from `always` on ONE of them, and a change that
// collapsed them into a single boolean would silently turn the safe default into full
// convergence.
func TestPruneMode_PathsMatchTheDocumentedTable(t *testing.T) {
	for _, tc := range []struct {
		mode        PruneMode
		eventDelete bool
		sweep       bool
	}{
		{PruneNever, false, false},
		{PruneOnEvent, true, false},
		{PruneAlways, true, true},
	} {
		assert.Equal(t, tc.eventDelete, tc.mode.AppliesEventDeletes(),
			"%q: explicit source DELETE", tc.mode)
		assert.Equal(t, tc.sweep, tc.mode.SweepsOrphans(),
			"%q: resync mark-and-sweep", tc.mode)
	}
}

// TestPruneMode_UnsetIsNotNever guards the trap that makes the zero value dangerous: read
// literally, "" answers false to BOTH predicates, which is `never` — a mode that silently stops
// mirroring deletes. Every internal carrier of the value normalizes through OrDefault first.
func TestPruneMode_UnsetIsNotNever(t *testing.T) {
	var unset PruneMode

	assert.False(t, unset.AppliesEventDeletes(),
		"read literally the empty mode looks like never — this is why nothing may read it literally")
	assert.Equal(t, PruneOnEvent, unset.OrDefault(),
		"OrDefault is the one place that turns unset into the documented default")
	assert.True(t, unset.OrDefault().AppliesEventDeletes(),
		"a normalized unset mode mirrors an explicit source DELETE")
	assert.False(t, unset.OrDefault().SweepsOrphans(),
		"a normalized unset mode never infers a deletion")
}

// TestPruneMode_UnrecognizedValueRetains covers the downgrade case: an object stored by a newer
// build under a mode this build's enum does not have. It must fail closed toward RETENTION on
// both paths — never toward deleting content whose policy it cannot understand.
func TestPruneMode_UnrecognizedValueRetains(t *testing.T) {
	future := PruneMode("onEventWithApproval")

	assert.Equal(t, future, future.OrDefault(),
		"an unrecognized value is not unset; OrDefault must leave it alone rather than reinterpret it")
	assert.False(t, future.AppliesEventDeletes())
	assert.False(t, future.SweepsOrphans())
}
