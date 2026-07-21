// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

var (
	retentionCMScope     = targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}}
	retentionSecretScope = targetWatchKey{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}}
)

// TestRetentionRollup_SumsEveryScope: a resync fires per type and per namespace within a type, so
// the number an operator needs is the target-wide total, not whichever scope reported last.
func TestRetentionRollup_SumsEveryScope(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	m.MarkTargetRetention(gitDest, retentionSecretScope, 1, v1alpha3.PruneOnEvent, 3)

	summary := m.RetentionForGitTarget(gitDest)
	assert.True(t, summary.Reported)
	assert.Equal(t, 5, summary.RetainedDocuments)
	assert.Equal(t, v1alpha3.PruneOnEvent, summary.Mode)
	assert.False(t, summary.ObservedTime.IsZero())
}

// TestRetentionRollup_ZeroIsRecordedAsActivelyAsAnyOtherCount is the likeliest regression in this
// whole projection. "Converged" and "retaining" are the two states the field exists to separate,
// and only publishing non-zero counts would make the first indistinguishable from a stale reading.
func TestRetentionRollup_ZeroIsRecordedAsActivelyAsAnyOtherCount(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 4)
	require.Equal(t, 4, m.RetentionForGitTarget(gitDest).RetainedDocuments)

	// The operator removed the stale documents by hand; the next resync finds nothing to retain.
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 0)

	summary := m.RetentionForGitTarget(gitDest)
	assert.True(t, summary.Reported, "a reported zero is still a report")
	assert.Zero(t, summary.RetainedDocuments)
}

// TestRetentionRollup_UnreportedIsNotZero keeps "nobody has told us yet" distinguishable from "a
// resync ran and found nothing". The controller projects the first as an absent status block.
func TestRetentionRollup_UnreportedIsNotZero(t *testing.T) {
	m := &Manager{}

	summary := m.RetentionForGitTarget(types.NewResourceReference("acme", "tenant-acme"))

	assert.False(t, summary.Reported)
	assert.Zero(t, summary.RetainedDocuments)
}

// TestRetentionRollup_ANewEpochDropsScopesThatLeftThePlan is the eviction property, and the reason
// this reuses the watch epoch instead of maintaining its own scope lifecycle: when a type stops
// being watched, its count has to disappear, or the roll-up only ever grows and becomes a lie.
func TestRetentionRollup_ANewEpochDropsScopesThatLeftThePlan(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	m.MarkTargetRetention(gitDest, retentionSecretScope, 1, v1alpha3.PruneOnEvent, 3)
	require.Equal(t, 5, m.RetentionForGitTarget(gitDest).RetainedDocuments)

	// Secrets left the watch plan; the new declaration replays only ConfigMaps.
	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 2)

	assert.Equal(t, 2, m.RetentionForGitTarget(gitDest).RetainedDocuments,
		"a scope that left the plan must take its count with it")
}

// TestRetentionRollup_StaleEpochIsIgnored is the property inherited from RenderFidelityGate: a
// cancelled watch's in-flight reply arrives after the new declaration and must not resurrect a
// count for a scope this target no longer has.
func TestRetentionRollup_StaleEpochIsIgnored(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 1)

	m.MarkTargetRetention(gitDest, retentionSecretScope, 1, v1alpha3.PruneOnEvent, 99)

	assert.Equal(t, 1, m.RetentionForGitTarget(gitDest).RetainedDocuments,
		"a record from a superseded epoch must not contribute")
}

// TestRetentionRollup_ReportsTheModeTheCountWasProducedUnder keeps the pair self-consistent. The
// mode travels with the count precisely so status cannot show a freshly declared `always` beside a
// number that a retaining policy produced.
func TestRetentionRollup_ReportsTheModeTheCountWasProducedUnder(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")

	// A legacy GitTarget stores no mode at all; the roll-up must report the effective one.
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, "", 2)

	assert.Equal(t, v1alpha3.PruneOnEvent, m.RetentionForGitTarget(gitDest).Mode)
}

// TestRetentionRollup_IsPerGitTarget guards the sharing bug a single map invites.
func TestRetentionRollup_IsPerGitTarget(t *testing.T) {
	m := &Manager{}
	acme := types.NewResourceReference("acme", "tenant-acme")
	other := types.NewResourceReference("other", "tenant-other")

	m.MarkTargetRetention(acme, retentionCMScope, 1, v1alpha3.PruneOnEvent, 7)

	assert.Equal(t, 7, m.RetentionForGitTarget(acme).RetainedDocuments)
	assert.False(t, m.RetentionForGitTarget(other).Reported)
}

// TestRetentionRollup_ForgottenTargetReportsNothing: a deleted GitTarget's roll-up must go with it,
// so a recreated target under the same name starts from "not reported" rather than inheriting a
// predecessor's count.
func TestRetentionRollup_ForgottenTargetReportsNothing(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 3)

	m.forgetTargetRetention(gitDest)

	assert.False(t, m.RetentionForGitTarget(gitDest).Reported)
}

// TestRetentionRollup_EnqueuesOnChangeOnly: the first appearance of a retention must not wait out
// the steady requeue — an operator consults this before flipping a target to `always`. A steady
// state must not enqueue at all, or a target that is deliberately retaining would re-reconcile on
// every resync of every scope, forever.
func TestRetentionRollup_EnqueuesOnChangeOnly(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("acme", "tenant-acme")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	require.Len(t, events, 1, "the first report is a change: nothing was known before")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	assert.Len(t, events, 1, "an unchanged roll-up must not enqueue")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 0)
	assert.Len(t, events, 2, "returning to converged is a change an operator is waiting for")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneAlways, 0)
	assert.Len(t, events, 3, "the mode changing is a change even when the count does not")

	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneAlways, 0)
	assert.Len(t, events, 3,
		"a new epoch that re-reports the same roll-up is not a change an operator can see")
}
