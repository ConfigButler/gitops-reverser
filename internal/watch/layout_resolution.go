// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReportLayoutResolved records what a scan resolved about a GitTarget's folder. It is installed
// on the WorkerManager (git.LayoutReporter) at startup, and it is the only writer of the
// layouts projection.
//
// Every scan calls it, including the ones that resolve exactly what the last one did — the
// branch worker has no memory across scans and should not grow one. The transition test lives
// here, so a steady-state target republishes nothing and enqueues no reconcile, and a folder
// whose shape actually changed does both.
func (m *Manager) ReportLayoutResolved(gitDest types.ResourceReference, report git.LayoutReport) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		if prior, had := s.layouts[gitDest.Key()]; had && sameLayout(prior, report) {
			return false
		}
		s.layouts[gitDest.Key()] = report
		return true
	})
	if changed {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// LayoutForGitTarget returns the most recently resolved layout for a GitTarget, and whether one
// has been resolved at all. Absent means no scan has reported yet — which is different from a
// folder that resolved to nothing, and the controller has to distinguish the two: the first
// leaves LayoutResolved Unknown, the second sets it with reason None.
func (m *Manager) LayoutForGitTarget(gitDest types.ResourceReference) (git.LayoutReport, bool) {
	report, ok := m.watchPlane().layouts[gitDest.Key()]
	return report, ok
}

// sameLayout compares two reports by what a reader would see in status, deliberately ignoring
// the observed revision and time.
//
// Those two advance on every scan of an unchanged folder, so comparing them would republish the
// whole immutable snapshot and enqueue a reconcile once per resync per target, forever, to
// advance a clock nobody reads a decision from. It is the same trap targetPassStatus avoided by
// dropping its timestamps. The cost is that observedTime lags a folder that never changes,
// which is the correct trade: the field exists to date the RESOLUTION, and the resolution is
// exactly what has not changed.
func sameLayout(a, b git.LayoutReport) bool {
	if a.Reason != b.Reason || a.RenderRoot != b.RenderRoot || a.ByTypeEntries != b.ByTypeEntries {
		return false
	}
	if !sameOptionalBool(a.SerializeNamespace, b.SerializeNamespace) {
		return false
	}
	if len(a.RenderRoots) != len(b.RenderRoots) {
		return false
	}
	for i := range a.RenderRoots {
		if a.RenderRoots[i] != b.RenderRoots[i] {
			return false
		}
	}
	if len(a.Examples) != len(b.Examples) {
		return false
	}
	for i := range a.Examples {
		if a.Examples[i] != b.Examples[i] {
			return false
		}
	}
	return true
}

func sameOptionalBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
