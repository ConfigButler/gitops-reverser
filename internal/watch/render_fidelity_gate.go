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

func renderFidelityScopes(keys []targetWatchKey) []git.RenderFidelityScope {
	scopes := make([]git.RenderFidelityScope, 0, len(keys))
	for _, key := range keys {
		scopes = append(scopes, git.RenderFidelityScope{GVR: key.GVR, Namespace: key.Namespace})
	}
	return scopes
}

// beginTargetRenderFidelityEpochLocked replaces the target's scope set. targetWatchesMu must be
// held. The returned bool tells the caller to enqueue a status refresh after releasing the lock.
func (m *Manager) beginTargetRenderFidelityEpochLocked(
	target types.ResourceReference,
	keys []targetWatchKey,
) bool {
	gate := m.fidelityGate()
	if gate == nil {
		return false
	}
	status := gate.Begin(target, renderFidelityScopes(keys))
	if m.targetRenderFidelity == nil {
		m.targetRenderFidelity = map[string]git.RenderFidelityStatus{}
	}
	prior, had := m.targetRenderFidelity[target.Key()]
	m.targetRenderFidelity[target.Key()] = status
	return !had || renderFidelityStatusChanged(prior, status)
}

// RenderFidelityEpochForGitTarget returns the epoch a replay result must carry. A zero epoch
// means no shared gate is wired, so callers preserve the legacy data path.
func (m *Manager) RenderFidelityEpochForGitTarget(target types.ResourceReference) uint64 {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	return m.targetRenderFidelity[target.Key()].Epoch
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

// MarkTargetRenderFidelityScopeClean records one complete clean replay result from the current
// epoch. A stale cancellation tail is ignored by the gate and cannot reopen a failed target.
func (m *Manager) MarkTargetRenderFidelityScopeClean(
	target types.ResourceReference,
	epoch uint64,
	key targetWatchKey,
) {
	gate := m.fidelityGate()
	if gate == nil || epoch == 0 {
		return
	}
	status, applied := gate.RecordScopeClean(
		target,
		epoch,
		git.RenderFidelityScope{GVR: key.GVR, Namespace: key.Namespace},
	)
	if applied {
		m.recordRenderFidelityStatus(target, status)
	}
}

// MarkTargetRenderFidelityScopeDiverged records a replay refusal caused by a rendered token.
func (m *Manager) MarkTargetRenderFidelityScopeDiverged(
	target types.ResourceReference,
	epoch uint64,
	key targetWatchKey,
	divergence manifestanalyzer.RenderDivergence,
) {
	gate := m.fidelityGate()
	if gate == nil || epoch == 0 {
		return
	}
	status, applied := gate.RecordScopeDivergence(
		target, epoch, git.RenderFidelityScope{GVR: key.GVR, Namespace: key.Namespace}, divergence)
	if applied {
		m.recordRenderFidelityStatus(target, status)
	}
}

// MarkTargetRenderFidelityDiverged closes normal writes immediately when a live window hits the
// same boundary outside a scoped replay. A fresh watch epoch is the only recovery route.
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
	return before.Epoch != after.Epoch || before.State != after.State || before.Reason != after.Reason ||
		before.Message != after.Message
}

func (m *Manager) dropTargetRenderFidelityLocked(target types.ResourceReference) {
	delete(m.targetRenderFidelity, target.Key())
	if gate := m.fidelityGate(); gate != nil {
		gate.Forget(target)
	}
}
