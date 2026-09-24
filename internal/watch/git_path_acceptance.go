// SPDX-License-Identifier: Apache-2.0

package watch

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReportGitPathRefusal records a write plan the branch worker refused on a live-event path,
// where no result channel carries the error back to the router. It is installed on the
// WorkerManager (git.GitPathRefusalReporter) at startup, and applies the same reason mapping
// the resync path uses, so a refusal reaches the user as GitPathAccepted=False / Stalled=True
// whether it was a live write or a background resync that hit it.
func (m *Manager) ReportGitPathRefusal(
	gitDest types.ResourceReference,
	cell types.CellKey,
	refused *manifestanalyzer.AcceptanceRefusedError,
) {
	if refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderDoesNotMatchLive) {
		m.MarkTargetRenderFidelityDiverged(gitDest, renderFidelityDivergence(refused))
		return
	}
	m.MarkTargetGitPathScopeRefused(gitDest, cell, gitPathRefusalReason(refused), refused.BlockMessage())
}

// MarkTargetGitPathRefused records that the GitTarget path failed the structure-only
// acceptance gate. The refusal is target-wide, not stream-specific.
//
// It is a report from a branch-worker drain goroutine. Only a real TRANSITION republishes the
// snapshot and enqueues a reconcile, so the happy-path resync stream does not enqueue one per
// event.
func (m *Manager) MarkTargetGitPathRefused(gitDest types.ResourceReference, reason, message string) {
	m.reportGitPathAcceptance(gitDest, GitPathAcceptanceStatus{
		Accepted: false,
		Reason:   reason,
		Message:  message,
	})
}

// MarkTargetGitPathScopeRefused records that a scoped writer pass failed the structure-only
// acceptance gate. The status remains target-level, but the remembered scope prevents an
// unrelated successful resync from clearing it.
func (m *Manager) MarkTargetGitPathScopeRefused(
	gitDest types.ResourceReference,
	cell types.CellKey,
	reason string,
	message string,
) {
	status := GitPathAcceptanceStatus{
		Accepted: false,
		Reason:   reason,
		Message:  message,
	}
	if cell != (types.CellKey{}) {
		status.RefusedCell = cell
		status.RefusedCellSet = true
	}
	m.reportGitPathAcceptance(gitDest, status)
}

// ReportGitPathScan publishes what a read-only scan of the folder found: a structural refusal
// nobody tried to write, or its recovery when refused is nil.
//
// It is the refresher's channel, and it is separate from ReportGitPathRefusal for one reason: a
// scan has no cell. It reads the whole folder, so its verdict is target-wide, and the scoped
// clearing rule that write-raised refusals depend on cannot express it.
//
// Structure-only acceptance cannot produce a render-fidelity issue — that one needs the live
// objects, which a scan does not have — so unlike ReportGitPathRefusal there is no fidelity case
// to route away here.
func (m *Manager) ReportGitPathScan(
	gitDest types.ResourceReference,
	refused *manifestanalyzer.AcceptanceRefusedError,
) {
	if refused == nil {
		m.MarkTargetGitPathScanAccepted(gitDest)
		return
	}
	m.MarkTargetGitPathScanRefused(gitDest, gitPathRefusalReason(refused), refused.BlockMessage())
}

// MarkTargetGitPathScanRefused records that a read of the folder failed the structure-only gate.
//
// It overwrites a write-raised refusal, and that is the right way round: both describe the same
// folder, and the newer reading is the current one. The reverse also holds — a write that refuses
// afterwards replaces this with its own, more specific, scoped refusal.
func (m *Manager) MarkTargetGitPathScanRefused(gitDest types.ResourceReference, reason, message string) {
	m.reportGitPathAcceptance(gitDest, GitPathAcceptanceStatus{
		Accepted:     false,
		Reason:       reason,
		Message:      message,
		RaisedByScan: true,
	})
}

// MarkTargetGitPathScanAccepted clears a refusal ONLY if a scan is what raised it.
//
// A scan proves the folder's structure and nothing else. A write-boundary refusal — the file this
// write may not touch, the document that has no single root to go into — is invisible to it, so
// clearing one on a clean scan would report a target as writable that is not. Those stay where
// they are until a resync writes the cell that proves them gone.
func (m *Manager) MarkTargetGitPathScanAccepted(gitDest types.ResourceReference) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.acceptance[gitDest.Key()]
		if had && prior.Accepted {
			return false
		}
		if had && !prior.RaisedByScan {
			return false
		}
		status := acceptedGitPathStatus()
		status.At = metav1.Now()
		s.acceptance[gitDest.Key()] = status
		return true
	})
	if changed {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// MarkTargetGitPathAccepted clears any prior unscoped refusal for the GitTarget path. Scoped
// resyncs should call MarkTargetGitPathScopeAccepted so they only clear the refusal they proved.
func (m *Manager) MarkTargetGitPathAccepted(gitDest types.ResourceReference) {
	if prior, had := m.watchPlane().acceptance[gitDest.Key()]; had && prior.Accepted {
		return
	}
	m.reportGitPathAcceptance(gitDest, acceptedGitPathStatus())
}

// MarkTargetGitPathScopeAccepted clears a scoped refusal only when the successful resync covered
// the same cell that produced the refusal. A successful replay of secrets must not hide a still
// impossible configmaps path, because the GitTarget status is the operator's only durable clue.
func (m *Manager) MarkTargetGitPathScopeAccepted(gitDest types.ResourceReference, cell types.CellKey) {
	status := acceptedGitPathStatus()
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.acceptance[gitDest.Key()]
		if had && !prior.Accepted && cell != (types.CellKey{}) {
			if !prior.RefusedCellSet || prior.RefusedCell != cell {
				return false
			}
		}
		if had && prior.Accepted {
			return false
		}
		status.At = metav1.Now()
		s.acceptance[gitDest.Key()] = status
		return true
	})
	if changed {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// acceptedGitPathStatus is the canonical healthy GitPathAccepted condition payload.
func acceptedGitPathStatus() GitPathAcceptanceStatus {
	return GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	}
}

func (m *Manager) reportGitPathAcceptance(gitDest types.ResourceReference, status GitPathAcceptanceStatus) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.acceptance[gitDest.Key()]
		// Compared before the write, so "changed" describes what an operator would see: newly
		// refused, a different refusal reason/message, or a recovery.
		if had &&
			prior.Accepted == status.Accepted &&
			prior.Reason == status.Reason &&
			prior.Message == status.Message &&
			prior.RefusedCell == status.RefusedCell &&
			prior.RefusedCellSet == status.RefusedCellSet {
			return false
		}
		status.At = metav1.Now()
		s.acceptance[gitDest.Key()] = status
		return true
	})
	if changed {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// GitPathAcceptanceForGitTarget returns the latest acceptance status for the GitTarget.
// Missing state means no refusal has been observed, so the path is accepted.
func (m *Manager) GitPathAcceptanceForGitTarget(gitDest types.ResourceReference) GitPathAcceptanceStatus {
	if st, ok := m.watchPlane().acceptance[gitDest.Key()]; ok {
		return st
	}
	return GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	}
}
