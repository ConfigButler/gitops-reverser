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

// reconcileTargetRenderFidelityLocked installs the target's current scope set and returns the
// revision each scope's stream must report under. targetWatchesMu must be held.
//
// Only the cells in restarted go back to pending; a cell whose stream was left running keeps
// its revision and its result, so an unrelated plan change neither closes writes on it nor
// clears a divergence nothing re-measured. The second return says whether the caller should
// enqueue a status refresh after releasing the lock. A nil map means no shared gate is wired,
// which the mark path treats as the legacy data path.
func (m *Manager) reconcileTargetRenderFidelityLocked(
	target types.ResourceReference,
	cells []types.CellKey,
	restarted []types.CellKey,
) (map[types.CellKey]uint64, bool) {
	gate := m.fidelityGate()
	if gate == nil {
		return nil, false
	}
	status, revisions := gate.Reconcile(target, cells, restarted)
	if m.targetRenderFidelity == nil {
		m.targetRenderFidelity = map[string]git.RenderFidelityStatus{}
	}
	prior, had := m.targetRenderFidelity[target.Key()]
	m.targetRenderFidelity[target.Key()] = status
	return revisions, !had || renderFidelityStatusChanged(prior, status)
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

func (m *Manager) recordRenderFidelityStatus(target types.ResourceReference, status git.RenderFidelityStatus) {
	m.targetWatchesMu.Lock()
	if m.targetRenderFidelity == nil {
		m.targetRenderFidelity = map[string]git.RenderFidelityStatus{}
	}
	prior, had := m.targetRenderFidelity[target.Key()]
	m.targetRenderFidelity[target.Key()] = status
	changed := !had || renderFidelityStatusChanged(prior, status)
	m.targetWatchesMu.Unlock()
	if changed {
		m.enqueueGitPathChange(target)
	}
}

func renderFidelityStatusChanged(before, after git.RenderFidelityStatus) bool {
	return before.Revision != after.Revision || before.State != after.State || before.Reason != after.Reason ||
		before.Message != after.Message
}

func (m *Manager) dropTargetRenderFidelityLocked(target types.ResourceReference) {
	delete(m.targetRenderFidelity, target.Key())
	if gate := m.fidelityGate(); gate != nil {
		gate.Forget(target)
	}
}
