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

// TestReportGitPathScan_RaisesAndRecovers is the pair the refresher needs: a read publishes a
// refusal nobody tried to write, and a later read that passes takes it back. Both wake the
// controller, because the condition it publishes is the operator's only sign either way.
func TestReportGitPathScan_RaisesAndRecovers(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, scanRefusal())

	status, had := m.watchPlane().acceptance[gitDest.Key()]
	require.True(t, had)
	assert.False(t, status.Accepted)
	assert.True(t, status.RaisedByScan, "a read raised it, and only a read may take it back")
	assert.False(t, status.RefusedCellSet, "a whole-folder scan has no cell to scope it to")
	require.Len(t, events, 1)

	m.ReportGitPathScan(gitDest, nil)

	status = m.watchPlane().acceptance[gitDest.Key()]
	assert.True(t, status.Accepted)
	assert.Len(t, events, 2, "the recovery is as much news as the refusal")
}

// TestReportGitPathScan_NeverClearsAWriteBoundaryRefusal is the rule that keeps this safe. A
// structural read sees the FILES; it cannot see that a write may not touch one of them, or that a
// new document has no single root to go into. Clearing a write-raised refusal on a clean scan
// would report a target as writable that is not.
func TestReportGitPathScan_NeverClearsAWriteBoundaryRefusal(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	cell := types.CellKey{Resource: "configmaps"}
	m.MarkTargetGitPathScopeRefused(gitDest, cell, "WriteBoundary", "this write may not touch that file")

	m.ReportGitPathScan(gitDest, nil)

	status := m.watchPlane().acceptance[gitDest.Key()]
	assert.False(t, status.Accepted, "the write refusal stands until a write proves it gone")
	assert.Equal(t, cell, status.RefusedCell)
}

// TestReportGitPathScan_AWriteRefusalReplacesAScanRefusal. Both describe the same folder and the
// newer reading is the current one — and the write's is the more specific, because it names the
// cell that recovery has to prove.
func TestReportGitPathScan_AWriteRefusalReplacesAScanRefusal(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	cell := types.CellKey{Resource: "configmaps"}

	m.ReportGitPathScan(gitDest, scanRefusal())
	m.MarkTargetGitPathScopeRefused(gitDest, cell, "WriteBoundary", "this write may not touch that file")

	status := m.watchPlane().acceptance[gitDest.Key()]
	assert.False(t, status.RaisedByScan)
	assert.True(t, status.RefusedCellSet)

	// And the scoped recovery now clears it, which is the path that exists today.
	m.MarkTargetGitPathScopeAccepted(gitDest, cell)
	assert.True(t, m.watchPlane().acceptance[gitDest.Key()].Accepted)
}

// TestMarkTargetGitPathScanAccepted_IsSilentWhenNothingWasRefused keeps the steady state quiet: a
// refresher publishes a verdict on every tick, and an already-accepted target must not be
// republished or re-enqueued for it.
func TestMarkTargetGitPathScanAccepted_IsSilentWhenNothingWasRefused(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")

	m.ReportGitPathScan(gitDest, nil)
	require.Len(t, events, 1, "the first verdict is news: nothing was known before")
	published := m.watchPlane()

	m.ReportGitPathScan(gitDest, nil)

	assert.Len(t, events, 1, "a folder that was fine and still is says nothing new")
	assert.Same(t, published, m.watchPlane())
}
