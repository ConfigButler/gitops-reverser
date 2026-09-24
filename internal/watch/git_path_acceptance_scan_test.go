// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func scanRefusal() *manifestanalyzer.AcceptanceRefusedError {
	return &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{
			Kind:    manifestanalyzer.IssueDuplicate,
			Path:    "team-a/duplicate.yaml",
			Message: "team-a/duplicate.yaml: one resource declared twice",
		}},
	}
}

func configMapCell() types.CellKey { return types.CellKey{Resource: "configmaps"} }

// TestReportGitPathScan_RaisesAndRecovers is the pair the refresher needs: a read publishes a
// refusal nobody tried to write, and a later read that passes takes it back. Both wake the
// controller, because the condition it publishes is the operator's only sign either way.
func TestReportGitPathScan_RaisesAndRecovers(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, scanRefusal())

	status := m.GitPathAcceptanceForGitTarget(gitDest)
	assert.False(t, status.Accepted)
	assert.False(t, status.RefusedCellSet, "a whole-folder scan has no cell to scope it to")
	require.Len(t, events, 1)

	m.ReportGitPathScan(gitDest, nil)

	assert.True(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)
	assert.Len(t, events, 2, "the recovery is as much news as the refusal")
}

// TestGitPathAcceptance_AStandingRefusalIsReportedOnceByEachProducer is the feedback loop, as a
// test. A refused folder has BOTH producers reporting it: the refresher reads it on every tick,
// and the resync the refusal itself triggers writes it and refuses again. While the two shared one
// record, each report looked like a change, so each enqueued a reconcile, which drove the next
// scan and resync — a loop that outruns the ten-second re-check it was supposed to ride.
func TestGitPathAcceptance_AStandingRefusalIsReportedOnceByEachProducer(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, scanRefusal())
	m.MarkTargetGitPathScopeRefused(gitDest, configMapCell(), "Unsupported", "the same folder, from a write")
	settled := len(events)

	// Three more rounds of exactly what is already known.
	for range 3 {
		m.ReportGitPathScan(gitDest, scanRefusal())
		m.MarkTargetGitPathScopeRefused(gitDest, configMapCell(), "Unsupported", "the same folder, from a write")
	}

	assert.Len(t, events, settled,
		"nothing an operator reads has changed, so nothing may wake the controller")
	assert.LessOrEqual(t, settled, 2, "and reaching the settled state costs one transition per producer")
}

// TestGitPathAcceptance_AScanNeverErasesAnUnresolvedWriteRefusal is the hazard of two producers in
// one slot. The sequence is write refusal, then a structural scan refusal, then a clean scan: if
// the scan's verdict displaced the write's, the clean scan would clear a write-boundary problem no
// write ever proved resolved, and the target would report healthy while it is not writable.
func TestGitPathAcceptance_AScanNeverErasesAnUnresolvedWriteRefusal(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")

	m.MarkTargetGitPathScopeRefused(gitDest, configMapCell(), "WriteBoundary", "this write may not touch that file")
	m.ReportGitPathScan(gitDest, scanRefusal())
	m.ReportGitPathScan(gitDest, nil)

	status := m.GitPathAcceptanceForGitTarget(gitDest)
	require.False(t, status.Accepted, "no write has proved the write-boundary refusal resolved")
	assert.Equal(t, "WriteBoundary", status.Reason)
	assert.Equal(t, configMapCell(), status.RefusedCell, "and its cell survived, so recovery still has a key")

	// The write's own recovery still works, and clears it.
	m.MarkTargetGitPathScopeAccepted(gitDest, configMapCell())
	assert.True(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)
}

// TestGitPathAcceptance_AScanRefusalIsRecordedEvenWhenItReadsLikeTheWriteRefusal. A write refusal
// can be unscoped — a commit window spanning several cells reports one — and a scan refusal about
// the same folder can carry the same reason and message. Dropping the scan's verdict for looking
// identical left a record whose provenance said "write", so the later clean scan could not clear
// it and the target stayed refused with nothing able to recover it.
func TestGitPathAcceptance_AScanRefusalIsRecordedEvenWhenItReadsLikeTheWriteRefusal(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	refused := scanRefusal()

	m.MarkTargetGitPathRefused(gitDest, gitPathRefusalReason(refused), refused.BlockMessage())
	m.ReportGitPathScan(gitDest, refused)
	// The write recovers on its own terms; the scan's verdict must still be standing.
	m.MarkTargetGitPathAccepted(gitDest)
	m.ReportGitPathScan(gitDest, refused)
	require.False(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)

	m.ReportGitPathScan(gitDest, nil)

	assert.True(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted,
		"the scan raised the standing refusal, so the scan can clear it")
}

// TestMarkTargetGitPathScopeAccepted_ClearsTheScanVerdictToo. A resync that succeeded ran the same
// structure-only gate over the same folder and passed, which is strictly stronger evidence than a
// read. A scan refusal left standing beside it would be stale.
func TestMarkTargetGitPathScopeAccepted_ClearsTheScanVerdictToo(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, scanRefusal())
	m.MarkTargetGitPathScopeAccepted(gitDest, configMapCell())

	assert.True(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)
}

// TestMarkTargetGitPathScopeAccepted_LeavesAnotherCellsRefusal keeps the rule the scoping exists
// for: a successful replay of one type must not hide a still impossible path in another.
func TestMarkTargetGitPathScopeAccepted_LeavesAnotherCellsRefusal(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")

	m.MarkTargetGitPathScopeRefused(gitDest, configMapCell(), "WriteBoundary", "configmaps are stuck")
	m.MarkTargetGitPathScopeAccepted(gitDest, types.CellKey{Resource: "secrets"})

	assert.False(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)
}

// TestMarkTargetGitPathScanAccepted_IsSilentWhenNothingWasRefused keeps the steady state quiet: a
// refresher publishes a verdict on every tick, and an already-accepted target must not be
// republished or re-enqueued for it.
func TestMarkTargetGitPathScanAccepted_IsSilentWhenNothingWasRefused(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, nil)
	m.ReportGitPathScan(gitDest, nil)

	assert.Empty(t, events, "a folder that was fine and still is says nothing at all")
	assert.True(t, m.GitPathAcceptanceForGitTarget(gitDest).Accepted)
}
