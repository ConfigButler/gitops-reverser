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
	if gate == nil || revision == 0 {
		return
	}
	status, applied := gate.RecordScopeClean(
		target,
		revision,
		cell,
	)
	if applied {
		m.recordRenderFidelityStatus(target, status)
	}
}

// MarkTargetRenderFidelityScopeDiverged records a replay refusal caused by a rendered token.
func (m *Manager) MarkTargetRenderFidelityScopeDiverged(
	target types.ResourceReference,
	revision uint64,
	cell types.CellKey,
	divergence manifestanalyzer.RenderDivergence,
) {
	gate := m.fidelityGate()
	if gate == nil || revision == 0 {
		return
	}
	status, applied := gate.RecordScopeDivergence(
		target, revision, cell, divergence)
	if applied {
		m.recordRenderFidelityStatus(target, status)
	}
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
		m.enqueueGitPathChange(target)
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
