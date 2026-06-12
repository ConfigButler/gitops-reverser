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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The audit-arrival wake is the FRESHNESS half of the R3 split (see
// docs/design/stream/api-source-of-truth-reconcile.md): for every Synced type a per-type tail loops
// on its :audit:stream and applies each change as a sweep-free per-event UPSERT/DELETE — the old
// per-event apply, re-sourced from the canonical stream onto the per-type stream. Because it never
// sweeps, it cannot delete an object whose create is still in flight, so the partial-burst race is
// gone by construction (not by a settle-window crutch). CORRECTNESS — catching orphans and missed
// deletes — is the checkpoint sweep's job (the TypeSynced / declare / boot-restore full splice in
// type_lifecycle.go + materialization.go), which runs only off an authoritative full LIST.
//
// Each tail holds one (pooled) Redis connection parked in a blocking read; one multiplexed XREAD
// across all type streams is a later optimisation (R10/HA) for the per-type fan-out at scale.

const (
	// auditTailBlock bounds one blocking read. A finite block keeps a single Redis connection from
	// parking forever and lets the loop re-check its context promptly; the cursor still never
	// misses an entry, because after the first read the tail resumes from a concrete stream ID.
	auditTailBlock = 5 * time.Second
	// auditTailBackoff is the pause after a transient read error before retrying, so a flapping
	// Redis does not hot-loop the tail.
	auditTailBackoff = 2 * time.Second
)

// AuditTailReader reads new entries on a type's audit stream as per-event git.Events. It is
// satisfied by queue.RedisByTypeStreamQueue and is optional on the Manager (nil disables the
// audit-arrival wake; the periodic checkpoint still reconciles). See
// docs/design/stream/api-source-of-truth-reconcile.md §8.
type AuditTailReader interface {
	// ReadTypeAuditChanges blocks up to `block` for new entries after lastID and returns them as
	// per-event git.Events (Object for an upsert, Identifier-only for a DELETE; no Path/GitTarget
	// set), plus the newest stream ID to resume from. "" or "$" anchors at the present.
	ReadTypeAuditChanges(
		ctx context.Context, group, resource, lastID string, block time.Duration,
	) ([]git.Event, string, error)
}

// startTypeAuditTail launches the per-type audit tail once for gvr, anchored at the type's
// checkpoint revision so it replays every audit event strictly after the checkpoint — closing the
// gap where an event that lands between the checkpoint LIST and the tail starting would otherwise be
// missed (e.g. a CRD/Secret created right after its type is first claimed). It runs under a child of
// the driver's context so a Released type (or shutdown) cancels it, and is idempotent: a repeat call
// for a tail already running is a no-op, so a periodic re-anchor's TypeSynced never spawns a
// duplicate. A nil reader (the wake not wired) is a no-op.
func (m *Manager) startTypeAuditTail(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, checkpointRV string,
) {
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
	go m.runTypeAuditTail(tailCtx, log, gvr, checkpointRV)
	log.V(1).Info("audit tail started", "gvr", gvr.String(), "anchorRV", checkpointRV)
}

// auditTailAnchor turns a checkpoint resourceVersion R into the stream-ID cursor the tail resumes
// from: "<R>-<maxuint64>", which is strictly greater than every entry at rv R (whose IDs are
// "<R>-<subseq>"), so the tail reads only entries with rv > R — exactly the events not already in
// the checkpoint @ R. A blank rv falls back to "$" (only entries arriving after the tail starts).
func auditTailAnchor(checkpointRV string) string {
	if strings.TrimSpace(checkpointRV) == "" {
		return "$"
	}
	return checkpointRV + "-18446744073709551615"
}

// isAuditTailRunning reports whether a per-type audit tail is already running for gvr. It is
// the "this is not the first time the type became serviceable" signal: the first TypeSynced (tail
// not yet running) fans an initial backfill, while a later one (a periodic re-anchor / late-event
// nudge, tail already live) re-fans the reconcile as a HEAL — deferred by the worker until the
// commit window is idle — to correct drift the in-order tail cannot express.
func (m *Manager) isAuditTailRunning(gvr schema.GroupVersionResource) bool {
	m.auditTailsMu.Lock()
	defer m.auditTailsMu.Unlock()
	_, running := m.auditTails[gvr]
	return running
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

// runTypeAuditTail loops on the type's audit stream and applies each batch of changes as sweep-free
// per-event upserts/deletes to every watching GitTarget. It exits when its context is cancelled
// (Release or shutdown).
func (m *Manager) runTypeAuditTail(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource, checkpointRV string,
) {
	block := m.auditTailBlockDuration()
	apply := m.auditTailApplyFn()
	// Anchor at the checkpoint revision so the tail replays every audit entry strictly after it — an
	// entry that arrived between the checkpoint LIST and this tail starting is then NOT missed. After
	// the first read the cursor is a concrete stream ID. A blank rv falls back to "$" (present-only).
	lastID := auditTailAnchor(checkpointRV)
	for ctx.Err() == nil {
		changes, newID, err := m.AuditTailReader.ReadTypeAuditChanges(ctx, gvr.Group, gvr.Resource, lastID, block)
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
		if len(changes) > 0 {
			apply(ctx, log, gvr, changes)
		}
	}
}

// applyAuditChangesForType fans a batch of per-event changes to every GitTarget that watches the
// type, as sweep-free upserts/deletes routed through the GitTarget's event stream (the worker's
// commit window coalesces them into commits). Each change is scoped to the GitTarget's watched
// namespaces and stamped with the GitTarget's write path, exactly as the live event path did. It
// never sweeps, so an object whose create has not yet landed in the log is never deleted.
func (m *Manager) applyAuditChangesForType(
	ctx context.Context,
	log logr.Logger,
	gvr schema.GroupVersionResource,
	changes []git.Event,
) {
	if m.EventRouter == nil {
		return
	}
	for _, table := range m.allWatchedTypeTables() {
		watched := watchedTypeForGVR(table, gvr)
		if watched == nil {
			continue
		}
		if m.EventRouter.GetGitTargetEventStream(table.GitDest) == nil {
			continue // the GitTarget's stream is not registered yet; its first checkpoint splice covers it
		}
		path, ok := m.gitTargetPath(ctx, table.GitDest)
		if !ok {
			continue
		}
		inScope := namespaceScopePredicate(watched.SnapshotNamespaces())
		for _, change := range changes {
			if !inScope(change.Identifier.Namespace) {
				continue
			}
			ev := change
			ev.Path = path
			if err := m.EventRouter.RouteToGitTargetEventStream(ev, table.GitDest); err != nil {
				log.V(1).Info("audit-tail route failed",
					"gitDest", table.GitDest.String(), "resource", ev.Identifier.String(), "err", err.Error())
			}
		}
	}
}

// watchedTypeForGVR returns the table's WatchedType for gvr, or nil if the GitTarget does not watch
// it. It is the membership-plus-scope lookup the audit-tail apply needs (tableWatchesGVR is the
// bool-only twin used where the scope is not needed).
func watchedTypeForGVR(table WatchedTypeTable, gvr schema.GroupVersionResource) *WatchedType {
	for i := range table.Types {
		if table.Types[i].GVR == gvr {
			return &table.Types[i]
		}
	}
	return nil
}

// gitTargetPath reads a GitTarget's write path (spec.path) — the folder its mirrored documents live
// under, the same path the live event route stamped onto each git.Event. A GitTarget that cannot be
// read yields ok=false, so the tail skips it (its next checkpoint splice still covers it).
func (m *Manager) gitTargetPath(ctx context.Context, gitDest types.ResourceReference) (string, bool) {
	if m.Client == nil {
		return "", false
	}
	var gt configv1alpha1.GitTarget
	if err := m.Client.Get(
		ctx, k8stypes.NamespacedName{Name: gitDest.Name, Namespace: gitDest.Namespace}, &gt,
	); err != nil {
		return "", false
	}
	return gt.Spec.Path, true
}

// auditTailBlockDuration returns the configured blocking-read window, honouring the test override so
// unit tests run fast.
func (m *Manager) auditTailBlockDuration() time.Duration {
	if m.auditTailBlockOverride > 0 {
		return m.auditTailBlockOverride
	}
	return auditTailBlock
}

// auditTailApplyFn returns the per-batch action, defaulting to the sweep-free per-event apply and
// overridable in tests so the tail's read/apply loop can be observed without a full worker stack.
func (m *Manager) auditTailApplyFn() func(context.Context, logr.Logger, schema.GroupVersionResource, []git.Event) {
	if m.auditTailApplyOverride != nil {
		return m.auditTailApplyOverride
	}
	return m.applyAuditChangesForType
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
