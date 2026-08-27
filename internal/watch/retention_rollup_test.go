// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

var (
	retentionCMScope     = types.CellKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "")
	retentionSecretScope = types.CellKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "")
)

// retentionPlan installs the cells a watch plan selects at the given stream revision, the step
// every declaration performs before those cells' resyncs can report.
func retentionPlan(m *Manager, gitDest types.ResourceReference, revision uint64, cells ...types.CellKey) {
	revisions := make(map[types.CellKey]uint64, len(cells))
	for _, cell := range cells {
		revisions[cell] = revision
	}
	m.retainTargetRetentionScopes(gitDest, revisions)
}

// TestRetentionRollup_SumsEveryScope: a resync fires per type and per namespace within a type, so
// the number an operator needs is the target-wide total, not whichever scope reported last.
func TestRetentionRollup_SumsEveryScope(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")

	retentionPlan(m, gitDest, 1, retentionCMScope, retentionSecretScope)
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
	retentionPlan(m, gitDest, 1, retentionCMScope)
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

// TestRetentionRollup_ANewPlanDropsScopesThatLeftIt is the eviction property, and the reason the
// plan installs the scope set instead of this roll-up maintaining its own lifecycle: when a type
// stops being watched, its count has to disappear, or the roll-up only ever grows and becomes a
// lie.
func TestRetentionRollup_ANewPlanDropsScopesThatLeftIt(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	retentionPlan(m, gitDest, 1, retentionCMScope, retentionSecretScope)
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	m.MarkTargetRetention(gitDest, retentionSecretScope, 1, v1alpha3.PruneOnEvent, 3)
	require.Equal(t, 5, m.RetentionForGitTarget(gitDest).RetainedDocuments)

	// Secrets left the watch plan. ConfigMaps was kept, so its stream — and its count — stay.
	retentionPlan(m, gitDest, 1, retentionCMScope)

	assert.Equal(t, 2, m.RetentionForGitTarget(gitDest).RetainedDocuments,
		"a scope that left the plan must take its count with it")
}

// A kept cell's stream is not restarted, so nothing re-reports for it. Its count has to survive
// the plan change, or every unrelated rule edit would zero a target's retention until the next
// resync of a cell that never moved.
func TestRetentionRollup_AKeptScopeKeepsItsCount(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	retentionPlan(m, gitDest, 1, retentionCMScope)
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)

	// A rule is added: secrets start at a fresh revision, configmaps keep theirs.
	m.retainTargetRetentionScopes(gitDest, map[types.CellKey]uint64{
		retentionCMScope: 1, retentionSecretScope: 2,
	})

	assert.Equal(t, 2, m.RetentionForGitTarget(gitDest).RetainedDocuments)
	m.MarkTargetRetention(gitDest, retentionSecretScope, 2, v1alpha3.PruneOnEvent, 3)
	assert.Equal(t, 5, m.RetentionForGitTarget(gitDest).RetainedDocuments)
}

// TestRetentionRollup_StaleRevisionIsIgnored is the property inherited from RenderFidelityGate: a
// cancelled watch's in-flight reply arrives after the new plan and must not resurrect a count for
// a scope this target no longer has, or overwrite the replacement stream's.
func TestRetentionRollup_StaleRevisionIsIgnored(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	retentionPlan(m, gitDest, 2, retentionCMScope)
	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 1)

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 99)
	m.MarkTargetRetention(gitDest, retentionSecretScope, 1, v1alpha3.PruneOnEvent, 99)

	assert.Equal(t, 1, m.RetentionForGitTarget(gitDest).RetainedDocuments,
		"neither a superseded revision nor a deselected scope may contribute")
}

// TestRetentionRollup_ReportsTheModeTheCountWasProducedUnder keeps the pair self-consistent. The
// mode travels with the count precisely so status cannot show a freshly declared `always` beside a
// number that a retaining policy produced.
func TestRetentionRollup_ReportsTheModeTheCountWasProducedUnder(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("acme", "tenant-acme")

	// A legacy GitTarget stores no mode at all; the roll-up must report the effective one.
	retentionPlan(m, gitDest, 1, retentionCMScope)
	m.MarkTargetRetention(gitDest, retentionCMScope, 1, "", 2)

	assert.Equal(t, v1alpha3.PruneOnEvent, m.RetentionForGitTarget(gitDest).Mode)
}

// TestRetentionRollup_IsPerGitTarget guards the sharing bug a single map invites.
func TestRetentionRollup_IsPerGitTarget(t *testing.T) {
	m := &Manager{}
	acme := types.NewResourceReference("acme", "tenant-acme")
	other := types.NewResourceReference("other", "tenant-other")

	retentionPlan(m, acme, 1, retentionCMScope)
	retentionPlan(m, other, 1, retentionCMScope)
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
	retentionPlan(m, gitDest, 1, retentionCMScope)
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
	retentionPlan(m, gitDest, 1, retentionCMScope)

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	require.Len(t, events, 1, "the first report is a change: nothing was known before")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 2)
	assert.Len(t, events, 1, "an unchanged roll-up must not enqueue")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneOnEvent, 0)
	assert.Len(t, events, 2, "returning to converged is a change an operator is waiting for")

	m.MarkTargetRetention(gitDest, retentionCMScope, 1, v1alpha3.PruneAlways, 0)
	assert.Len(t, events, 3, "the mode changing is a change even when the count does not")

	retentionPlan(m, gitDest, 2, retentionCMScope)
	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneAlways, 0)
	assert.Len(t, events, 3,
		"a restarted stream that re-reports the same roll-up is not a change an operator can see")
}

// A dropped retention report leaves the published count describing a mirror that has moved on,
// and nothing re-measures it until the cell is replanned — which for a settled target is the
// steady requeue away. Dropping is correct; dropping SILENTLY is what made a reproducible CI
// failure (retainedDocuments stuck at its pre-sweep value after prune.mode was widened,
// on a target whose files had been swept) take log archaeology to narrow and still not name.
func TestRetentionRollup_ADroppedReportSaysSoAndWhy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cell          types.CellKey
		revision      uint64
		wantReason    string
		wantInstalled string
	}{
		{
			name:          "a stream the plan has replaced",
			cell:          retentionCMScope,
			revision:      1,
			wantReason:    "the reporting stream has been replaced",
			wantInstalled: `"installedRevision"=2`,
		},
		{
			name:          "a cell the plan no longer holds",
			cell:          retentionSecretScope,
			revision:      2,
			wantReason:    "the cell is not in the current watch plan",
			wantInstalled: `"installedRevision"=0`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, lines := recordingLogger()
			m := &Manager{Log: log}
			gitDest := types.NewResourceReference("acme", "tenant-acme")
			retentionPlan(m, gitDest, 2, retentionCMScope)
			m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 1)

			m.MarkTargetRetention(gitDest, tc.cell, tc.revision, v1alpha3.PruneOnEvent, 0)

			assert.Equal(t, 1, m.RetentionForGitTarget(gitDest).RetainedDocuments,
				"the drop itself still stands: a stale report must not move the count")
			require.Equal(t, 1, countContaining(*lines, "retention report dropped"),
				"exactly one drop is reported, and it is not silent")
			joined := strings.Join(*lines, "\n")
			assert.Contains(t, joined, tc.wantReason, "the line names WHY it was dropped")
			assert.Contains(t, joined, tc.wantInstalled, "and carries both revisions to compare")
		})
	}
}

// The steady-state path must stay quiet: an accepted report is not a drop, and logging one per
// resync of every scope would bury the line that matters.
func TestRetentionRollup_AnAcceptedReportLogsNothing(t *testing.T) {
	log, lines := recordingLogger()
	m := &Manager{Log: log}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	retentionPlan(m, gitDest, 2, retentionCMScope)

	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 1)
	m.MarkTargetRetention(gitDest, retentionCMScope, 2, v1alpha3.PruneOnEvent, 0)

	assert.Equal(t, 0, countContaining(*lines, "retention report dropped"))
}

// TestMarkTargetRetention_SaysWhenAnAcceptedReportPublishesNothing closes the roll-up's remaining
// silence. mutateWatchPlane discards the WHOLE mutation when nothing an operator would see moved,
// so an accepted-and-unchanged report and a report that never arrived are the same silence from
// outside — which is exactly how far Failure B could be narrowed and no further
// (docs/design/watch-plane-status-convergence-failures.md, §3.4).
func TestMarkTargetRetention_SaysWhenAnAcceptedReportPublishesNothing(t *testing.T) {
	log, lines := recordingLogger()
	m := &Manager{Log: log}
	gitDest := types.NewResourceReference("acme", "tenant-acme")
	cell := types.CellKeyFor(configmapsGVR, "apps")
	m.retainTargetRetentionScopes(gitDest, map[types.CellKey]uint64{cell: 7})

	// First report publishes: it moves Reported from false to true.
	m.MarkTargetRetention(gitDest, cell, 7, v1alpha3.PruneOnEvent, 2)
	require.Equal(t, 2, m.RetentionForGitTarget(gitDest).RetainedDocuments)

	// Identical re-report: accepted, but nothing an operator sees moves, so it is discarded.
	m.MarkTargetRetention(gitDest, cell, 7, v1alpha3.PruneOnEvent, 2)

	assert.Contains(t, strings.Join(*lines, "\n"),
		"retention report accepted but published nothing")
}
