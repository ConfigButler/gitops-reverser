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
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file is the CommitRequest watermark barrier — the C-B1 drain-coordinator of
// docs/design/stream/canonical-stream-retirement.md §6. At CommitRequest creation time the
// reconciler takes a per-type snapshot of each claimed type's stream top (TakeTypeSnapshot),
// building an independent watermark per type. The barrier (DrainTailsToSnapshot) then waits
// until, for each type T in the snapshot, the per-type audit tail has APPLIED (enqueued onto
// the GitTarget's FIFO BranchWorker) every entry at or below that type's watermark. Because
// each tail feeds a single FIFO writer, a finalize enqueued after the drain is guaranteed to
// commit every pre-watermark mutation.
//
// Using per-type watermarks (not a single cross-type rv_C) means no cross-type RV ordering
// is assumed: each comparison stays within the same type's RV space, so aggregated-API types
// with their own RV counters are handled correctly.

const (
	// FinalizeBarrierTimeout bounds one watermark wait. Decided (Option A,
	// docs/design/stream/commitrequest-barrier-timeout-decision.md): ~3 tail read windows;
	// on expiry the caller finalizes anyway and REPORTS the degrade — never a hang, never
	// a failed CommitRequest. A constant, not a knob.
	FinalizeBarrierTimeout = 15 * time.Second

	// barrierPollInterval is how often the barrier re-reads the tail cursors. The cursors
	// move at the tails' pace (one blocking read window at worst), so polling beats
	// plumbing a notification channel through every tail for an operation this rare.
	barrierPollInterval = 200 * time.Millisecond
)

// auditHighWaterReader is the capability TakeTypeSnapshot uses to read each type's current
// stream top. queue.RedisByTypeStreamQueue implements it; a reader that does not causes
// TakeTypeSnapshot to leave the type's watermark blank (the barrier then skips the type —
// safe because the checkpoint backstops it).
type auditHighWaterReader interface {
	TypeAuditHighWater(ctx context.Context, group, resource string) string
}

// TakeTypeSnapshot returns the current stream-top RV for each type the GitTarget currently
// claims. It is called at CommitRequest creation time — before the first status stamp — to
// build the per-type watermark map that DrainTailsToSnapshot will wait on.
//
// Each type's watermark is read from its own stream (via the optional auditHighWaterReader
// capability), so no cross-type RV comparison is ever made. A type whose stream is empty or
// whose reader does not implement the capability is recorded as "" — the barrier treats ""
// as "nothing to wait for" and skips the type.
func (m *Manager) TakeTypeSnapshot(
	ctx context.Context, gitDest types.ResourceReference,
) map[schema.GroupVersionResource]string {
	gvrs := m.watchedGVRsForGitDest(gitDest)
	snapshot := make(map[schema.GroupVersionResource]string, len(gvrs))
	reader, hasHW := m.AuditTailReader.(auditHighWaterReader)
	for _, gvr := range gvrs {
		if hasHW {
			snapshot[gvr] = reader.TypeAuditHighWater(ctx, gvr.Group, gvr.Resource)
		}
		// Without the capability the entry is left as "": barrier skips the type.
	}
	return snapshot
}

// DrainTailsToSnapshot blocks until every type in the snapshot has had its per-type audit
// tail apply all entries up to the type's own watermark, or until timeout/ctx-cancel.
// It returns true when all watermarks were reached and false on the bounded degrade — the
// caller finalizes either way and reports the degrade (Option A).
//
// Per type T in the snapshot:
//   - "" or non-numeric watermark → skip (empty stream or opaque RV such as aggregated API)
//   - no running tail → skip (correctness rides the checkpoint)
//   - streamIDRV(cursor_T) >= numericValue(snapshot[T]) → passed
//
// Types absent from the snapshot were not claimed at CommitRequest creation time and are
// not waited on. The snapshot IS the wait set — DrainTailsToSnapshot reads only the
// in-memory cursor map on each poll, making no Redis calls.
func (m *Manager) DrainTailsToSnapshot(
	ctx context.Context,
	snapshot map[schema.GroupVersionResource]string,
	timeout time.Duration,
) bool {
	if len(snapshot) == 0 {
		return true
	}

	deadline := time.Now().Add(timeout)
	for {
		if m.snapshotReached(snapshot) {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		wait := barrierPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if !sleepOrDone(ctx, wait) {
			return false
		}
	}
}

// snapshotReached evaluates the per-type barrier condition once. It reads only the
// in-memory cursor map — no Redis calls — so it is safe to call on every poll tick.
func (m *Manager) snapshotReached(snapshot map[schema.GroupVersionResource]string) bool {
	for gvr, hwRaw := range snapshot {
		if hwRaw == "" {
			continue // stream was empty at snapshot time — nothing to wait for
		}
		hw, err := strconv.ParseUint(strings.TrimSpace(hwRaw), 10, 64)
		if err != nil {
			continue // opaque RV (e.g. aggregated API) — can't compare, skip
		}
		cursorID, running := m.auditTailCursor(gvr)
		if !running {
			continue // no tail running — correctness rides the checkpoint
		}
		cursor, ok := streamIDRV(cursorID)
		if ok && cursor >= hw {
			continue // tail has consumed up to or past this type's snapshot watermark
		}
		return false
	}
	return true
}

// streamIDRV parses the leading resourceVersion component of a stream ID (or of the
// "<rv>-<maxuint64>" anchor form). ok is false for "$" or anything non-numeric.
func streamIDRV(id string) (uint64, bool) {
	lead, _, _ := strings.Cut(id, "-")
	rv, err := strconv.ParseUint(lead, 10, 64)
	if err != nil {
		return 0, false
	}
	return rv, true
}

// watchedGVRsForGitDest returns the distinct types the GitTarget currently watches.
// Used by TakeTypeSnapshot to determine which types to include in the per-type snapshot.
func (m *Manager) watchedGVRsForGitDest(gitDest types.ResourceReference) []schema.GroupVersionResource {
	for _, table := range m.allWatchedTypeTables() {
		if table.GitDest != gitDest {
			continue
		}
		seen := map[schema.GroupVersionResource]struct{}{}
		out := make([]schema.GroupVersionResource, 0, len(table.Types))
		for i := range table.Types {
			if _, dup := seen[table.Types[i].GVR]; dup {
				continue
			}
			seen[table.Types[i].GVR] = struct{}{}
			out = append(out, table.Types[i].GVR)
		}
		return out
	}
	return nil
}
