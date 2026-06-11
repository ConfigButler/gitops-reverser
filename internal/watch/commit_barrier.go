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
// docs/design/stream/canonical-stream-retirement.md §6. A CommitRequest's own
// resourceVersion (rv_C) is a global etcd-revision watermark: every mutation its author
// made before creating it has a smaller RV. The barrier blocks until, for each type the
// GitTarget claims, the per-type audit tail has APPLIED (enqueued onto the GitTarget's
// FIFO BranchWorker) every stream entry strictly below rv_C — after which a finalize
// enqueued behind those upserts is guaranteed to commit them.

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

// auditHighWaterReader is the optional observation capability the barrier's quiet-type
// shortcut uses: a type whose stream high-water is already below the watermark has no
// pre-watermark entry left to deliver once its tail has caught up to that high-water.
// queue.RedisByTypeStreamQueue implements it; a reader that does not simply denies the
// shortcut (the barrier then needs cursor ≥ watermark, or times out).
type auditHighWaterReader interface {
	TypeAuditHighWater(ctx context.Context, group, resource string) string
}

// DrainTailsToWatermark blocks until every per-type audit tail of the GitTarget's watched
// types has applied all stream entries strictly below rv, or until timeout/ctx-cancel.
// It returns true when the watermark was reached and false on the bounded degrade — the
// caller finalizes either way and reports the degrade (Option A).
//
// Per type T, the §6 condition: cursor_T ≥ rv, OR (highWater_T < rv AND cursor_T ≥
// highWater_T) — i.e. the tail consumed everything pre-watermark, or nothing that new
// exists and the tail is fully drained. A type with NO running tail is skipped: there is
// no freshness path to wait on (the type rides checkpoints; e.g. claimed-but-unfollowable),
// and waiting on something that cannot move would turn every finalize into a timeout. A
// non-numeric rv carries no orderable watermark, so there is nothing to wait for.
func (m *Manager) DrainTailsToWatermark(
	ctx context.Context, gitDest types.ResourceReference, rv string, timeout time.Duration,
) bool {
	watermark, err := strconv.ParseUint(strings.TrimSpace(rv), 10, 64)
	if err != nil {
		return true
	}
	gvrs := m.watchedGVRsForGitDest(gitDest)
	if len(gvrs) == 0 {
		return true
	}

	deadline := time.Now().Add(timeout)
	for {
		if m.tailsReachedWatermark(ctx, gvrs, watermark) {
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

// tailsReachedWatermark evaluates the per-type barrier condition once across all types.
func (m *Manager) tailsReachedWatermark(
	ctx context.Context, gvrs []schema.GroupVersionResource, watermark uint64,
) bool {
	for _, gvr := range gvrs {
		cursorID, running := m.auditTailCursor(gvr)
		if !running {
			continue // no tail to wait on; correctness rides the checkpoint
		}
		cursor, ok := streamIDRV(cursorID)
		if ok && cursor >= watermark {
			continue
		}
		highWater, hwKnown := m.typeAuditHighWater(ctx, gvr)
		drained := highWater == 0 || (ok && cursor >= highWater)
		if hwKnown && highWater < watermark && drained {
			continue // nothing pre-watermark exists beyond what the tail already applied
		}
		return false
	}
	return true
}

// typeAuditHighWater reads a type stream's high-water RV through the optional capability
// on the tail reader. known is false when the capability is absent, the stream is empty,
// or the RV is not numeric (an aggregated apiserver's opaque RVs never gate the barrier).
func (m *Manager) typeAuditHighWater(ctx context.Context, gvr schema.GroupVersionResource) (uint64, bool) {
	reader, ok := m.AuditTailReader.(auditHighWaterReader)
	if !ok {
		return 0, false
	}
	raw := reader.TypeAuditHighWater(ctx, gvr.Group, gvr.Resource)
	if raw == "" {
		// An empty stream genuinely has nothing pre-watermark: report 0/known.
		return 0, true
	}
	rv, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return rv, true
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

// watchedGVRsForGitDest returns the distinct types the GitTarget currently watches — the
// claim set the barrier must drain.
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
