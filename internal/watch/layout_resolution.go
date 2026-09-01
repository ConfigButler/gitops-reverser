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

// sameLayout compares two reports by what a reader would see in status.
//
// The resolution time is deliberately not compared: it advances on every scan of an unchanged
// folder, so comparing it would republish the whole immutable snapshot and enqueue a reconcile
// once per resync per target, forever, to advance a clock nobody reads a decision from. It is the
// same trap targetPassStatus avoided by dropping its timestamps.
//
// The REVISION is not compared either, for the same reason — every commit to the branch
// moves it, whichever target caused the commit — with one exception, below: a report that has one
// where the last had none is always a change. Without that exception the field would be written
// once, at the first scan of a branch that usually has no commit yet, and then never advance,
// which would leave it permanently empty on exactly the targets it is meant to inform. What it
// therefore means is the revision this resolution was FIRST observed at, not the latest scanned.
func sameLayout(a, b git.LayoutReport) bool {
	if a.Revision == "" && b.Revision != "" {
		return false
	}
	if a.Reason != b.Reason || a.Mode != b.Mode || a.RenderRoot != b.RenderRoot {
		return false
	}
	return sameStrings(a.RenderRoots, b.RenderRoots) && sameStrings(a.ReadOnlyBases, b.ReadOnlyBases)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
