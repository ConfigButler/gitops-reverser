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
func (m *Manager) MarkTargetGitPathRefused(gitDest types.ResourceReference, reason, message string) {
	m.targetWatchesMu.Lock()
	if m.targetGitPathAcceptance == nil {
		m.targetGitPathAcceptance = map[string]GitPathAcceptanceStatus{}
	}
	prev, had := m.targetGitPathAcceptance[gitDest.Key()]
	m.targetGitPathAcceptance[gitDest.Key()] = GitPathAcceptanceStatus{
		Accepted: false,
		Reason:   reason,
		Message:  message,
		At:       metav1.Now(),
	}
	// Emit only on a real transition (newly refused, or the refusal reason changed) so the
	// happy-path resync stream does not enqueue a reconcile per event.
	changed := !had || prev.Accepted || prev.Reason != reason
	m.targetWatchesMu.Unlock()
	if changed {
		m.enqueueGitPathChange(gitDest)
	}
}

// MarkTargetGitPathAccepted clears any prior refusal for the GitTarget path.
func (m *Manager) MarkTargetGitPathAccepted(gitDest types.ResourceReference) {
	m.targetWatchesMu.Lock()
	if m.targetGitPathAcceptance == nil {
		m.targetGitPathAcceptance = map[string]GitPathAcceptanceStatus{}
	}
	prev, had := m.targetGitPathAcceptance[gitDest.Key()]
	m.targetGitPathAcceptance[gitDest.Key()] = GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
		At:       metav1.Now(),
	}
	// Emit only when clearing a prior refusal (recovery). The steady-state resync calls this
	// on every successful apply; without the transition guard that would be a reconcile storm.
	changed := had && !prev.Accepted
	m.targetWatchesMu.Unlock()
	if changed {
		m.enqueueGitPathChange(gitDest)
	}
}

// GitPathAcceptanceForGitTarget returns the latest acceptance status for the GitTarget.
// Missing state means no refusal has been observed, so the path is accepted.
func (m *Manager) GitPathAcceptanceForGitTarget(gitDest types.ResourceReference) GitPathAcceptanceStatus {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if m.targetGitPathAcceptance != nil {
		if st, ok := m.targetGitPathAcceptance[gitDest.Key()]; ok {
			return st
		}
	}
	return GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	}
}

func (m *Manager) dropTargetGitPathAcceptanceLocked(gitDest types.ResourceReference) {
	if m.targetGitPathAcceptance != nil {
		delete(m.targetGitPathAcceptance, gitDest.Key())
	}
}
