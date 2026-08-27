// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// RenderFidelityStatus is the GitTarget render-vs-live condition state shared with the writer.
type RenderFidelityStatus = git.RenderFidelityStatus

func (m *Manager) fidelityGate() *git.RenderFidelityGate {
	if m.EventRouter == nil || m.EventRouter.WorkerManager == nil {
		return nil
	}
	return m.EventRouter.WorkerManager.RenderFidelityGate()
}

// reconcileTargetRenderFidelity installs the target's current scope set and returns the revision
// each scope's stream must report under, plus whether the caller should enqueue a status refresh.
//
// Only the cells in restarted go back to pending; a cell whose stream was left running keeps its
// revision and its result, so an unrelated plan change neither closes writes on it nor clears a
// divergence nothing re-measured. A nil map means no shared gate is wired, which the mark path
// treats as the legacy data path.
//
// The gate call happens before anything is published, so the owner never holds its own state lock
// across a call into the writer's. That coupling — two subsystems' locks with an order to get
// wrong — is one of the four hazards the ownership design names.
func (m *Manager) reconcileTargetRenderFidelity(
	target types.ResourceReference,
	cells []types.CellKey,
	restarted []types.CellKey,
) (map[types.CellKey]uint64, bool) {
	gate := m.fidelityGate()
	if gate == nil {
		return nil, false
	}
	status, revisions := gate.Reconcile(target, cells, restarted)
	return revisions, m.publishRenderFidelityStatus(target, status)
}

// RenderFidelityForGitTarget returns the latest condition projection. Missing state means the
// target has not installed watches yet and remains writable for compatibility.
func (m *Manager) RenderFidelityForGitTarget(target types.ResourceReference) RenderFidelityStatus {
	gate := m.fidelityGate()
	if gate == nil {
		return git.RenderFidelityStatus{State: git.RenderFidelityTrue, Reason: "RenderMatchesLive",
			Message: "Every rendered token matches live"}
	}
	return gate.Status(target)
}

// MarkTargetRenderFidelityScopeClean records one complete clean replay result from the cell's
// current stream. A stale cancellation tail carries the retired stream's revision, so the gate
// ignores it and it cannot reopen a failed target.
func (m *Manager) MarkTargetRenderFidelityScopeClean(
	target types.ResourceReference,
	revision uint64,
	cell types.CellKey,
) {
	gate := m.fidelityGate()
	if gate == nil {
		return
	}
	if revision == 0 {
		// A wired gate always issues a non-zero revision, so a stream reporting under zero was
		// started without one. Its result is unusable and its scope keeps owing a report.
		m.logUnappliedFidelityReport(target, cell, revision, "clean (stream carries no revision)")
		return
	}
	status, applied := gate.RecordScopeClean(target, revision, cell)
	if !applied {
		m.logUnappliedFidelityReport(target, cell, revision, "clean")
		return
	}
	m.recordRenderFidelityStatus(target, status)
}

// logUnappliedFidelityReport names a scope result the gate would not take.
//
// This is the last silent branch in the roll-up. RecordScope* answers applied=false for three
// different reasons — the target is unknown, the scope is not in the current plan, or the result
// carries a superseded revision — and every caller used to drop that answer on the floor. A scope
// then owes a report for ever with nothing to say why, which is Failure A's signature
// (docs/design/watch-plane-status-convergence-failures.md, §2.5).
//
// The retention roll-up already logs its refusals, and that asymmetry is exactly why B had
// evidence and A had none. Info, because it is rare by construction: a healthy plan produces one
// accepted report per scope per revision, and a report nobody can accept means the target will not
// converge on its own.
func (m *Manager) logUnappliedFidelityReport(
	target types.ResourceReference,
	cell types.CellKey,
	revision uint64,
	kind string,
) {
	m.Log.WithName("render-fidelity").Info(
		"a render scope result was not applied; this scope still owes a report and cannot converge alone",
		"gitDest", target.String(), "cell", cell.String(), "reportedRevision", revision, "result", kind,
		"status", m.RenderFidelityForGitTarget(target).Message)
}

// MarkTargetRenderFidelityScopeDiverged records a replay refusal caused by a rendered token.
func (m *Manager) MarkTargetRenderFidelityScopeDiverged(
	target types.ResourceReference,
	revision uint64,
	cell types.CellKey,
	divergence manifestanalyzer.RenderDivergence,
) {
	gate := m.fidelityGate()
	if gate == nil {
		return
	}
	if revision == 0 {
		// A wired gate always issues a non-zero revision, so a stream reporting under zero was
		// started without one. Its result is unusable and its scope keeps owing a report.
		m.logUnappliedFidelityReport(target, cell, revision, "diverged (stream carries no revision)")
		return
	}
	status, applied := gate.RecordScopeDivergence(target, revision, cell, divergence)
	if !applied {
		m.logUnappliedFidelityReport(target, cell, revision, "diverged")
		return
	}
	m.recordRenderFidelityStatus(target, status)
}

// MarkTargetRenderFidelityDiverged closes normal writes immediately when a live window hits the
// same boundary outside a scoped replay. Restarting every scope is the only recovery route.
func (m *Manager) MarkTargetRenderFidelityDiverged(
	target types.ResourceReference,
	divergence manifestanalyzer.RenderDivergence,
) {
	gate := m.fidelityGate()
	if gate == nil {
		return
	}
	m.recordRenderFidelityStatus(target, gate.Fail(target, divergence))
}

// recordRenderFidelityStatus is a drain goroutine's report of a gate result.
func (m *Manager) recordRenderFidelityStatus(target types.ResourceReference, status git.RenderFidelityStatus) {
	if m.publishRenderFidelityStatus(target, status) {
		m.enqueueGitTargetReconcile(target)
	}
}

// publishRenderFidelityStatus records the projected gate state and reports whether it moved.
func (m *Manager) publishRenderFidelityStatus(
	target types.ResourceReference,
	status git.RenderFidelityStatus,
) bool {
	return m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.fidelity[target.Key()]
		if had && !renderFidelityStatusChanged(prior, status) {
			return false
		}
		s.fidelity[target.Key()] = status
		return true
	})
}

func renderFidelityStatusChanged(before, after git.RenderFidelityStatus) bool {
	return before.Revision != after.Revision || before.State != after.State || before.Reason != after.Reason ||
		before.Message != after.Message
}

// forgetTargetRenderFidelity drops a deleted GitTarget's projection and its gate scopes.
func (m *Manager) forgetTargetRenderFidelity(target types.ResourceReference) {
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		if _, had := s.fidelity[target.Key()]; !had {
			return false
		}
		delete(s.fidelity, target.Key())
		return true
	})
	if gate := m.fidelityGate(); gate != nil {
		gate.Forget(target)
	}
}
