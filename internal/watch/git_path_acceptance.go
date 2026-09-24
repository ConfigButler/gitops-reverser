// SPDX-License-Identifier: Apache-2.0

package watch

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// gitPathRefusal is one producer's standing verdict on a GitTarget's folder. The zero value is
// "nothing refused", which is what a target with no record and a producer that has found nothing
// both mean.
type gitPathRefusal struct {
	Refused bool
	Reason  string
	Message string
	// Cell is the watched cell a WRITE refusal was found for, and CellSet says whether it was
	// scoped at all. Recovery compares it, so an unrelated successful resync cannot clear a
	// refusal it proved nothing about.
	Cell    types.CellKey
	CellSet bool
	At      metav1.Time
}

// gitPathAcceptance holds the two verdicts about one folder SEPARATELY, and that separation is the
// design rather than an implementation detail.
//
// Two producers answer the same question and they are not interchangeable. A WRITE refusal is
// evidence that something with work in hand could not proceed; it names a cell, and only a resync
// that wrote that cell proves it gone. A SCAN refusal is evidence that a read of the folder found
// structure nobody can write; it has no cell, and the read that raised it is what clears it.
//
// They shared one slot in the first cut, and every one of the three defects a review found came
// from that: each producer overwrote the other's verdict, so a scan could erase an unresolved
// write refusal and then clear itself; a scan refusal that looked identical to a standing write
// refusal was discarded as "no change", leaving a record no scan could ever clear; and the two
// producers flipped the record back and forth on their own cadences, each flip counting as news
// and enqueueing another reconcile, which drove the next scan and resync.
type gitPathAcceptance struct {
	write gitPathRefusal
	scan  gitPathRefusal
}

// published is what an operator reads: the write verdict when one stands, otherwise the scan's.
//
// The precedence is fixed rather than most-recent on purpose. Recency would let two producers
// running at their own cadences move the published view back and forth, which is the feedback
// loop this structure exists to remove. A write refusal wins because it is the more actionable of
// the two: it names the cell whose successful resync is the way out.
func (a gitPathAcceptance) published() GitPathAcceptanceStatus {
	switch {
	case a.write.Refused:
		return GitPathAcceptanceStatus{
			Accepted:       false,
			Reason:         a.write.Reason,
			Message:        a.write.Message,
			At:             a.write.At,
			RefusedCell:    a.write.Cell,
			RefusedCellSet: a.write.CellSet,
		}
	case a.scan.Refused:
		return GitPathAcceptanceStatus{
			Accepted: false,
			Reason:   a.scan.Reason,
			Message:  a.scan.Message,
			At:       a.scan.At,
		}
	default:
		return acceptedGitPathStatus()
	}
}

// sameAs compares two published views by what a reader would see. The timestamp is deliberately
// out: it moves on every report, and comparing it would enqueue a reconcile per report forever.
func sameAs(a, b GitPathAcceptanceStatus) bool {
	return a.Accepted == b.Accepted &&
		a.Reason == b.Reason &&
		a.Message == b.Message &&
		a.RefusedCell == b.RefusedCell &&
		a.RefusedCellSet == b.RefusedCellSet
}

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

// ReportGitPathScan publishes what a read-only scan of the folder found: a structural refusal
// nobody tried to write, or its recovery when refused is nil.
//
// It is the refresher's channel, and it is separate from ReportGitPathRefusal because a scan has
// no cell: it reads the whole folder, so its verdict is target-wide and cannot take part in the
// scoped recovery that write refusals depend on.
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

// MarkTargetGitPathRefused records that the GitTarget path failed the structure-only
// acceptance gate. The refusal is target-wide, not stream-specific.
//
// It is a report from a branch-worker drain goroutine. Only a real TRANSITION republishes the
// snapshot and enqueues a reconcile, so the happy-path resync stream does not enqueue one per
// event.
func (m *Manager) MarkTargetGitPathRefused(gitDest types.ResourceReference, reason, message string) {
	m.MarkTargetGitPathScopeRefused(gitDest, types.CellKey{}, reason, message)
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
	m.mutateGitPathAcceptance(gitDest, func(a *gitPathAcceptance) {
		a.write = gitPathRefusal{
			Refused: true,
			Reason:  reason,
			Message: message,
			Cell:    cell,
			CellSet: cell != (types.CellKey{}),
			At:      metav1.Now(),
		}
	})
}

// MarkTargetGitPathScanRefused records that a READ of the folder failed the structure-only gate.
//
// It touches only the scan verdict. A write refusal standing beside it is untouched evidence: it
// was raised by something that had work in hand, and nothing a read can do proves it resolved.
func (m *Manager) MarkTargetGitPathScanRefused(gitDest types.ResourceReference, reason, message string) {
	m.mutateGitPathAcceptance(gitDest, func(a *gitPathAcceptance) {
		a.scan = gitPathRefusal{Refused: true, Reason: reason, Message: message, At: metav1.Now()}
	})
}

// MarkTargetGitPathAccepted clears any prior refusal for the GitTarget path, write and scan alike.
// Scoped resyncs should call MarkTargetGitPathScopeAccepted so they only clear the refusal they
// proved.
func (m *Manager) MarkTargetGitPathAccepted(gitDest types.ResourceReference) {
	m.mutateGitPathAcceptance(gitDest, func(a *gitPathAcceptance) {
		a.write = gitPathRefusal{}
		a.scan = gitPathRefusal{}
	})
}

// MarkTargetGitPathScopeAccepted clears a write refusal only when the successful resync covered
// the same cell that produced it. A successful replay of secrets must not hide a still impossible
// configmaps path, because the GitTarget status is the operator's only durable clue.
//
// It clears the SCAN verdict unconditionally, and that asymmetry is deliberate: the resync ran the
// same structure-only gate the scan runs, over the same folder, and passed. That is strictly
// stronger evidence than a read, so a scan refusal left standing beside it would be stale.
func (m *Manager) MarkTargetGitPathScopeAccepted(gitDest types.ResourceReference, cell types.CellKey) {
	m.mutateGitPathAcceptance(gitDest, func(a *gitPathAcceptance) {
		a.scan = gitPathRefusal{}
		if !a.write.Refused {
			return
		}
		if cell != (types.CellKey{}) && (!a.write.CellSet || a.write.Cell != cell) {
			return
		}
		a.write = gitPathRefusal{}
	})
}

// MarkTargetGitPathScanAccepted records that a read of the folder found nothing wrong.
//
// It clears ONLY the scan verdict. A structural read sees the files; it cannot see that a write
// may not touch one of them, or that a new document has no single root to go into, so clearing a
// write refusal here would report a target as writable that is not.
func (m *Manager) MarkTargetGitPathScanAccepted(gitDest types.ResourceReference) {
	m.mutateGitPathAcceptance(gitDest, func(a *gitPathAcceptance) {
		a.scan = gitPathRefusal{}
	})
}

// acceptedGitPathStatus is the canonical healthy GitPathAccepted condition payload.
func acceptedGitPathStatus() GitPathAcceptanceStatus {
	return GitPathAcceptanceStatus{
		Accepted: true,
		Reason:   "GitPathAccepted",
		Message:  "GitTarget path accepted",
	}
}

// mutateGitPathAcceptance applies one producer's verdict and wakes the controller only if the
// PUBLISHED view moved.
//
// The two are separate questions, and conflating them was a defect rather than a shortcut. The
// record must always be written, because it is what a later recovery is compared against — a
// verdict dropped for looking like the one already stored left a record whose provenance was
// wrong, and nothing could clear it. The reconcile must only be enqueued when a reader would see
// something different, or two producers reporting the same standing problem on their own cadences
// drive each other round a loop that outruns the intended ten-second re-check.
func (m *Manager) mutateGitPathAcceptance(gitDest types.ResourceReference, apply func(*gitPathAcceptance)) {
	changed := m.mutateWatchPlane(func(s *watchPlaneState) bool {
		current := s.acceptance[gitDest.Key()]
		before := current.published()
		apply(&current)
		s.acceptance[gitDest.Key()] = current
		return !sameAs(before, current.published())
	})
	if changed {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// GitPathAcceptanceForGitTarget returns the latest acceptance status for the GitTarget.
// Missing state means no refusal has been observed, so the path is accepted.
func (m *Manager) GitPathAcceptanceForGitTarget(gitDest types.ResourceReference) GitPathAcceptanceStatus {
	if st, ok := m.watchPlane().acceptance[gitDest.Key()]; ok {
		return st.published()
	}
	return acceptedGitPathStatus()
}
