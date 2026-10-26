// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the branch worker's side of the render-fidelity gate: what it records when a replay
// resync applies, and what it says when the gate drops a write.
//
// The gate is per GitTarget and its scopes are per collection: one scope that has not reported
// under its current revision closes normal writes for every collection of the target. A live
// event or atomic write that meets a closed gate is DROPPED, and nothing replays it: the watch
// cursor has already moved past it. So the drop is counted (git_writes_dropped_total) and the first
// one under each gate status is logged at Info, naming the scopes the gate waits for.

// The kind label of git_writes_dropped_total.
const (
	writeDroppedEvent  = "event"
	writeDroppedAtomic = "atomic"
)

// The reason label of git_writes_dropped_total.
const (
	writeDropRenderFidelityPending  = "render_fidelity_pending"
	writeDropRenderFidelityDiverged = "render_fidelity_diverged"
)

// renderFidelityDropMark is the gate status a GitTarget's last Info-logged drop was made under.
type renderFidelityDropMark struct {
	revision uint64
	state    RenderFidelityState
}

// recordReplayClean records the scope a replay resync measured as clean, now: on the loop, once
// its plan was accepted, and before its caller is answered.
//
// The caller records the same result once it has the answer (the watch layer's resync drain), and
// that is where the condition is published. But the caller runs on another goroutine, and the loop
// takes its next item the moment it has answered. A live event queued behind the resync would then
// meet the gate before the caller recorded anything, find it closed, and be dropped for good. The
// e2e spec for a rules[].sourceNamespace override lost its ConfigMap this way, on a sibling scope
// whose resync sat ahead of the event in the queue.
//
// Recording it twice is harmless: the gate accepts a second clean result under the same revision
// unchanged, and ignores a revision the plan has moved past. A divergence is left to the caller,
// because a scope that diverged keeps the gate closed exactly as a pending one does.
func (l *branchWorkerEventLoop) recordReplayClean(req *ResyncRequest) {
	if l.w.renderFidelityGate == nil || req.RenderFidelityRevision == 0 || req.Scope == nil {
		return
	}
	l.w.renderFidelityGate.RecordScopeClean(
		itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace),
		req.RenderFidelityRevision, req.Scope.Collection)
}

// dropAtRenderFidelity counts one write the closed gate dropped, and logs it: at Info for the first
// drop of target under this gate status, at V(1) for the rest, so a target whose gate stays closed
// through a burst of edits says so once without logging every edit. resource names the dropped
// object, empty for an atomic write.
func (l *branchWorkerEventLoop) dropAtRenderFidelity(
	target itypes.ResourceReference,
	status RenderFidelityStatus,
	kind, resource string,
) {
	reason := writeDropRenderFidelityPending
	if status.State == RenderFidelityFalse {
		reason = writeDropRenderFidelityDiverged
	}
	log := l.w.Log.WithValues("gitTarget", target.String(), "kind", kind, "reason", reason,
		"renderFidelity", status.Message)
	if resource != "" {
		log = log.WithValues("resource", resource)
	}
	mark := renderFidelityDropMark{revision: status.Revision, state: status.State}
	if l.fidelityDropsLogged[target.Key()] != mark {
		if l.fidelityDropsLogged == nil {
			l.fidelityDropsLogged = map[string]renderFidelityDropMark{}
		}
		l.fidelityDropsLogged[target.Key()] = mark
		log.Info("Dropped a write because the GitTarget's render fidelity is not established; it is " +
			"missing from Git until the object changes again or its collection replays. Further " +
			"drops under this gate status are counted in git_writes_dropped_total and logged at V(1)")
	} else {
		log.V(1).Info("Dropped a write because the GitTarget's render fidelity is not established")
	}
	l.recordWriteDropped(kind, reason)
}

// recordWriteDropped counts one write dropped at the render-fidelity gate.
func (l *branchWorkerEventLoop) recordWriteDropped(kind, reason string) {
	if telemetry.GitWritesDroppedTotal == nil {
		return
	}
	ctx := l.w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	telemetry.GitWritesDroppedTotal.Add(ctx, 1, metric.WithAttributes(
		l.w.gitBranchAttrs(attribute.String("kind", kind), attribute.String("reason", reason))...))
}
