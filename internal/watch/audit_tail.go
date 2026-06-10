/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The audit-arrival wake (R2): for every Synced type, a per-type tail blocks on its audit stream
// and fires a splice reconcile when a mutating event lands, so a GitTarget reflects a change well
// inside the checkpoint interval instead of waiting for the next re-anchor. It is the freshness
// half of the splice (the TypeSynced wake in materialization.go is the correctness half). Each tail
// is started when its type first becomes Synced and stopped when the type is Released; it is
// idempotent, so a periodic re-anchor's repeated TypeSynced never spawns a second tail.
//
// Each tail holds one (pooled) Redis connection parked in a blocking read. That is fine for the
// per-type fan-out at the scale this runs (one pod, tens of claimed types); a single multiplexed
// reader is a later optimisation (R10/HA).

const (
	// auditTailBlock bounds one blocking read. A finite block keeps a single Redis connection from
	// parking forever and lets the loop re-check its context promptly; the cursor still never
	// misses an entry, because after the first read the tail resumes from a concrete stream ID.
	auditTailBlock = 5 * time.Second
	// auditTailSettle is the quiet window the tail waits out after the last event before
	// reconciling, so a burst of co-arriving events is folded into ONE reconcile. This is not just
	// efficiency: the splice mark-and-sweep needs the WHOLE burst in the log first. Reconciling on
	// a partial burst could sweep an object whose create event is still microseconds away in the
	// stream — exactly the bi-directional sharing race (Flux writes two objects to the shared path;
	// a reconcile that has seen only the first would delete the second). One settle window closes
	// that gap; the periodic re-anchor backstops anything slower. Each arrival within the window
	// extends it, so a whole burst (even one spread over several windows) coalesces into one
	// reconcile. Sized with margin over the sub-100ms gap co-applied objects actually show, since
	// freshness rides the still-live audit path in R2 and is not on this latency.
	auditTailSettle = 2 * time.Second
	// auditTailBackoff is the pause after a transient read error before retrying, so a flapping
	// Redis does not hot-loop the tail.
	auditTailBackoff = 2 * time.Second
)

// AuditTailReader blocks for the next entry on a type's audit stream. It is satisfied by
// queue.RedisByTypeStreamQueue and is optional on the Manager (nil disables the audit-arrival
// wake; the periodic re-anchor still reconciles). See api-source-of-truth-reconcile.md §8 (R2).
type AuditTailReader interface {
	// AwaitTypeAuditEntry blocks up to `block` for a new entry after lastID, returning the newest
	// stream ID to resume from and whether anything new arrived. "" or "$" anchors at the present.
	AwaitTypeAuditEntry(ctx context.Context, group, resource, lastID string, block time.Duration) (string, bool, error)
}

// startTypeAuditTail launches the per-type audit tail once for gvr, under a child of the driver's
// context so a Released type (or shutdown) cancels it. It is idempotent: a repeat call for a tail
// already running is a no-op, so a periodic re-anchor's TypeSynced never spawns a duplicate. A nil
// reader (the wake not wired) is a no-op.
func (m *Manager) startTypeAuditTail(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.AuditTailReader == nil {
		return
	}
	m.auditTailsMu.Lock()
	defer m.auditTailsMu.Unlock()
	if m.auditTails == nil {
		m.auditTails = map[schema.GroupVersionResource]context.CancelFunc{}
	}
	if _, running := m.auditTails[gvr]; running {
		return
	}
	tailCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel is stored below and invoked by stopTypeAuditTail
	m.auditTails[gvr] = cancel
	go m.runTypeAuditTail(tailCtx, log, gvr)
	log.V(1).Info("audit tail started", "gvr", gvr.String())
}

// stopTypeAuditTail cancels and forgets a type's audit tail (the type was Released — checkpoint
// dropped, so there is nothing fresh to serve). It is a no-op when no tail is running.
func (m *Manager) stopTypeAuditTail(gvr schema.GroupVersionResource) {
	m.auditTailsMu.Lock()
	defer m.auditTailsMu.Unlock()
	if cancel, ok := m.auditTails[gvr]; ok {
		cancel()
		delete(m.auditTails, gvr)
	}
}

// runTypeAuditTail loops on the type's audit stream and fires a per-type splice reconcile whenever a
// burst of mutating events settles. The reconcile is self-gating (it holds unless the checkpoint is
// Synced) and idempotent (the splice folds the whole log), so a coalesced burst yields one correct
// reconcile. It exits when its context is cancelled (Release or shutdown).
func (m *Manager) runTypeAuditTail(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	block, settle := m.auditTailBlockDuration(), m.auditTailSettleDuration()
	reconcile := m.auditTailReconcileFn()
	// "$" anchors at the present so the tail only reacts to entries after it starts; the TypeSynced
	// wake already reconciled the log up to here. After the first read the cursor is a concrete ID.
	lastID := "$"
	for ctx.Err() == nil {
		newID, hasNew, err := m.AuditTailReader.AwaitTypeAuditEntry(ctx, gvr.Group, gvr.Resource, lastID, block)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.V(1).Info("audit tail read error; backing off", "gvr", gvr.String(), "err", err.Error())
			if !sleepOrDone(ctx, auditTailBackoff) {
				return
			}
			continue
		}
		lastID = newID
		if !hasNew {
			continue
		}
		// Drain the rest of the burst before reconciling, so the splice sees the whole co-arriving
		// set and never sweeps an object whose create event is still in flight.
		lastID = m.drainAuditBurst(ctx, gvr, lastID, settle)
		reconcile(ctx, log, gvr)
	}
}

// drainAuditBurst keeps reading until the stream is quiet for one settle window, returning the
// cursor to resume from. Each further arrival extends the window; a settle-length gap with nothing
// new ends the burst. A read error (including context cancellation) ends the drain with the cursor
// reached so far — the caller still reconciles on what it has, and the next loop iteration handles
// the cancellation.
func (m *Manager) drainAuditBurst(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	lastID string,
	settle time.Duration,
) string {
	for ctx.Err() == nil {
		id, hasNew, err := m.AuditTailReader.AwaitTypeAuditEntry(ctx, gvr.Group, gvr.Resource, lastID, settle)
		if err != nil {
			return lastID
		}
		lastID = id
		if !hasNew {
			return lastID // quiet for one settle window → the burst has settled
		}
	}
	return lastID
}

// auditTailBlockDuration / auditTailSettleDuration return the configured blocking-read and
// burst-settle windows, honouring the test overrides so unit tests run fast.
func (m *Manager) auditTailBlockDuration() time.Duration {
	if m.auditTailBlockOverride > 0 {
		return m.auditTailBlockOverride
	}
	return auditTailBlock
}

func (m *Manager) auditTailSettleDuration() time.Duration {
	if m.auditTailSettleOverride > 0 {
		return m.auditTailSettleOverride
	}
	return auditTailSettle
}

// auditTailReconcileFn returns the per-burst action, defaulting to the per-type splice reconcile and
// overridable in tests so the tail's coalescing can be observed without a full reconcile stack.
func (m *Manager) auditTailReconcileFn() func(context.Context, logr.Logger, schema.GroupVersionResource) {
	if m.auditTailReconcileOverride != nil {
		return m.auditTailReconcileOverride
	}
	return m.reconcileTypeForSyncedTargets
}

// sleepOrDone waits for d or for ctx to cancel, reporting false if the context cancelled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
