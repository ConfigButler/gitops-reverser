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
	"strconv"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// This file owns the per-(GitTarget, GVR) coverage watermark Hc that makes the type-global audit
// tail's delivery target-LOCAL (signing-snapshot-tail-replay-failure-investigation.md §7). A
// type-global tail reads one shared cursor for every watcher of a GVR; without a target-local
// boundary it re-delivers entries a late-joining or recreated target already covered with its
// reconcile, which a commitWindow:0s GitProvider then turns into stray per-event commits in an
// otherwise reconcile-only history. The watermark closes that: each target carries the highest rv
// its own reconcile for the type covered, and the fan-out routes only strictly-newer entries to it.
//
// Lifecycle (§7.1/§7.3): NotReconciled (no map entry) -> Reconciling/LiveFrom(Hc) (an entry). The
// gate treats Reconciling and LiveFrom identically (both suppress rv <= Hc, route rv > Hc), so the
// boundary is modelled as one Hc value rather than two states. Publishing happens only after the
// scoped reconcile is enqueued; advances are monotonic; clearing (the safe NotReconciled reset) is
// the only backward move and happens on delete/recreate.

// publishTargetTypeWatermark records the coverage head Hc a GitTarget's reconcile for a type has
// covered, after that reconcile has been enqueued (event_router.EmitTypeReconcileForGitDest). It
// advances the boundary monotonically: a lower (or unparseable) Hc than the one already held is
// ignored, because a stale-high-then-lowered watermark could suppress a legitimate live event and
// leave a file missing until the next heal (§7.3). A blank Hc is a no-op — never publish a boundary
// the gate cannot compare against. The mutex is shared with the fan-out gate so the worker queue
// gets the happens-before edge described in §7.4.
func (m *Manager) publishTargetTypeWatermark(
	gitDest types.ResourceReference, gvr schema.GroupVersionResource, hc string,
) {
	if hc == "" {
		return
	}
	key := gitDest.String()
	m.targetTypeWatermarkMu.Lock()
	defer m.targetTypeWatermarkMu.Unlock()
	if m.targetTypeWatermark == nil {
		m.targetTypeWatermark = map[string]map[schema.GroupVersionResource]string{}
	}
	byGVR := m.targetTypeWatermark[key]
	if byGVR == nil {
		byGVR = map[schema.GroupVersionResource]string{}
		m.targetTypeWatermark[key] = byGVR
	}
	if prior, ok := byGVR[gvr]; ok && !rvGreater(hc, prior) {
		// Already at or beyond hc — hold the higher boundary (monotonic, §7.3).
		return
	}
	byGVR[gvr] = hc
}

// targetTypeWatermarkFor reports the coverage head a GitTarget holds for a type and whether a
// boundary exists at all. ok=false is the NotReconciled state: the tail must suppress every entry
// for that target until its first reconcile publishes a boundary (§7.1, the WAIT branch of §7.2).
func (m *Manager) targetTypeWatermarkFor(
	gitDest types.ResourceReference, gvr schema.GroupVersionResource,
) (string, bool) {
	key := gitDest.String()
	m.targetTypeWatermarkMu.Lock()
	defer m.targetTypeWatermarkMu.Unlock()
	byGVR := m.targetTypeWatermark[key]
	if byGVR == nil {
		return "", false
	}
	hc, ok := byGVR[gvr]
	return hc, ok
}

// clearTargetTypeWatermarks drops every coverage watermark a GitTarget held, resetting it to
// NotReconciled. It is called from ForgetGitTargetDeclaration on GitTarget delete so a recreate
// with the same namespaced name never inherits a dead boundary: a reused stale-high Hc would
// suppress the recreated target's legitimate live events until its first reconcile, which is the
// dangerous direction (§7.3). A no-op for an unknown GitTarget.
func (m *Manager) clearTargetTypeWatermarks(gitDest types.ResourceReference) {
	m.targetTypeWatermarkMu.Lock()
	defer m.targetTypeWatermarkMu.Unlock()
	delete(m.targetTypeWatermark, gitDest.String())
}

// rvAboveWatermark reports whether an audit-tail entry at rvE is strictly newer than the coverage
// head hc — i.e. live for this target and routable. The decision belongs to the API resourceVersion
// boundary (§8.2): rvE <= hc was already covered by the target's reconcile and must be suppressed.
// When the comparison cannot be made safely (an unparseable rv), it prefers routing over
// suppressing — a redundant commit is recoverable, a wrongly-suppressed live event needs the next
// heal to fix (§7.3). In practice both are uint64 stream-ID components, so the fallback is defensive.
func rvAboveWatermark(rvE, hc string) bool {
	e, errE := strconv.ParseUint(rvE, 10, 64)
	h, errH := strconv.ParseUint(hc, 10, 64)
	if errE != nil || errH != nil {
		return true
	}
	return e > h
}

// rvGreater reports whether decimal resourceVersion a is numerically greater than b. An unparseable
// a is treated as not-greater so publishTargetTypeWatermark never advances onto a boundary it
// cannot later compare against (§7.3: never advance from an uncertain RV).
func rvGreater(a, b string) bool {
	av, aerr := strconv.ParseUint(a, 10, 64)
	bv, berr := strconv.ParseUint(b, 10, 64)
	if aerr != nil {
		return false
	}
	if berr != nil {
		return true
	}
	return av > bv
}
