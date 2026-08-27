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
	refused *manifestanalyzer.AcceptanceRefusedError,
) {
	if refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderDoesNotMatchLive) {
		m.MarkTargetRenderFidelityDiverged(gitDest, renderFidelityDivergence(refused))
		return
	}
	m.MarkTargetGitPathRefused(gitDest, gitPathRefusalReason(refused), refused.BlockMessage())
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

// MarkTargetGitPathAccepted clears any prior refusal for the GitTarget path. The steady-state
// resync calls it on every successful apply, so the fast path below has to be free: an already
// accepted path takes a pointer load and returns.
func (m *Manager) MarkTargetGitPathAccepted(gitDest types.ResourceReference) {
	if prior, had := m.watchPlane().acceptance[gitDest.Key()]; had && prior.Accepted {
		return
	}
	m.reportGitPathAcceptance(gitDest, GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	})
}

func (m *Manager) reportGitPathAcceptance(gitDest types.ResourceReference, status GitPathAcceptanceStatus) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.acceptance[gitDest.Key()]
		// Compared before the write, so "changed" describes what an operator would see: newly
		// refused, a different refusal reason, or a recovery.
		if had && prior.Accepted == status.Accepted && prior.Reason == status.Reason {
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
