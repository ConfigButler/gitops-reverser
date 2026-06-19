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
	"strings"

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
// Hc is a FULL Redis stream position "<rv>-<seq>", not a bare rv. Distinct audit entries can share
// an rv — an rv-less DELETE/Status rides the high-water as "<rv>-<seq>", and duplicate or same-rv
// writes get fresh seqs — so the gate compares full positions. A bare-rv compare would suppress a
// legitimate same-rv live entry that arrived after the reconcile's fold (the stale-high watermark
// hazard, §7.3), which is more dangerous than the over-routing it was meant to prevent.
//
// Lifecycle (§7.1/§7.3): NotReconciled (no map entry) -> Reconciling/LiveFrom(Hc) (an entry). The
// gate treats Reconciling and LiveFrom identically (both suppress id <= Hc, route id > Hc), so the
// boundary is modelled as one Hc value rather than two states. Publishing happens only after the
// scoped reconcile is enqueued; advances are monotonic; clearing (the safe NotReconciled reset) is
// the only backward move and happens on delete/recreate.

// publishTargetTypeWatermark records the coverage head Hc (a full "<rv>-<seq>" stream position) a
// GitTarget's reconcile for a type has covered, after that reconcile has been enqueued
// (event_router.EmitTypeReconcileForGitDest). It advances the boundary monotonically: a lower Hc
// than the one already held is ignored, because a stale-high-then-lowered watermark could suppress a
// legitimate live event and leave a file missing until the next heal (§7.3). An unparseable Hc is a
// no-op — never publish a boundary the gate cannot compare against, so the target stays NotReconciled
// (suppress all) rather than degrading to route-all. The mutex is shared with the fan-out gate so the
// worker queue gets the happens-before edge described in §7.4.
func (m *Manager) publishTargetTypeWatermark(
	gitDest types.ResourceReference, gvr schema.GroupVersionResource, hc string,
) {
	if _, _, ok := parseStreamID(hc); !ok {
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
	prior, had := byGVR[gvr]
	if had && !streamIDStrictlyAfter(hc, prior) {
		// Already at or beyond hc — hold the higher boundary (monotonic, §7.3).
		// Diagnostic (residual-e2e-flakes-2026-06-19.md, Flake B): a held (not advanced) boundary is
		// where a late-join object can land below Hc and be tail-suppressed — log so a repro shows it.
		m.Log.V(1).Info("target-type watermark held (not advanced)",
			"gitDest", key, "gvr", gvr.String(), "held", prior, "offered", hc)
		return
	}
	byGVR[gvr] = hc
	m.Log.Info("target-type watermark published",
		"gitDest", key, "gvr", gvr.String(), "prior", prior, "new", hc)
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

// pruneTargetTypeWatermarks drops a GitTarget's watermarks for every GVR not in keep — the set it
// currently claims. A GitTarget that stops watching a type (a rule change) keeps no boundary for it,
// so if it later re-adds that GVR it restarts at NotReconciled rather than gating against a stale
// boundary the fan-out would honor before the fresh reconcile re-publishes one (§7.3.7). It is
// called from DeclareForGitTarget with the authoritative claimed set, so it only ever runs on a
// successful, observable resolve — a transient discovery gap declares nothing and prunes nothing.
func (m *Manager) pruneTargetTypeWatermarks(gitDest types.ResourceReference, keep []schema.GroupVersionResource) {
	m.targetTypeWatermarkMu.Lock()
	defer m.targetTypeWatermarkMu.Unlock()
	byGVR := m.targetTypeWatermark[gitDest.String()]
	if byGVR == nil {
		return
	}
	keepSet := make(map[schema.GroupVersionResource]struct{}, len(keep))
	for _, gvr := range keep {
		keepSet[gvr] = struct{}{}
	}
	for gvr := range byGVR {
		if _, ok := keepSet[gvr]; !ok {
			delete(byGVR, gvr)
		}
	}
}

// streamIDAfterWatermark reports whether an audit-tail entry at stream position entryID is strictly
// after the coverage head hc — i.e. live for this target and routable. The decision belongs to the
// API-rv-ordered stream position (§8.2): entryID <= hc was already covered by the target's reconcile
// and must be suppressed; entryID > hc is live. The comparison is by (rv, seq), so a same-rv entry
// that arrived after the fold (a higher seq than hc) is correctly treated as live. When the
// comparison cannot be made safely (an unparseable id), it prefers routing over suppressing — a
// redundant commit is recoverable, a wrongly-suppressed live event needs the next heal to fix (§7.3).
func streamIDAfterWatermark(entryID, hc string) bool {
	order, ok := streamIDOrder(entryID, hc)
	if !ok {
		return true
	}
	return order > 0
}

// streamIDStrictlyAfter reports whether stream position a is strictly after b, used to keep the
// published watermark monotonic. An unparseable a is treated as not-after so the watermark never
// advances onto a boundary it cannot later compare against (§7.3: never advance from an uncertain
// position); an unparseable b (should not occur — only parseable ids are stored) yields after=true.
func streamIDStrictlyAfter(a, b string) bool {
	order, ok := streamIDOrder(a, b)
	if !ok {
		// Distinguish "a is bad" (do not advance) from "b is bad" (a wins).
		if _, _, aok := parseStreamID(a); !aok {
			return false
		}
		return true
	}
	return order > 0
}

// streamIDOrder compares two FULL Redis stream positions "<rv>-<seq>" by (rv, then seq), returning
// -1/0/1 and ok=false when either side is not a parseable "<uint64>-<uint64>".
func streamIDOrder(a, b string) (int, bool) {
	am, as, aok := parseStreamID(a)
	bm, bs, bok := parseStreamID(b)
	if !aok || !bok {
		return 0, false
	}
	switch {
	case am != bm:
		if am > bm {
			return 1, true
		}
		return -1, true
	case as != bs:
		if as > bs {
			return 1, true
		}
		return -1, true
	default:
		return 0, true
	}
}

// parseStreamID splits a Redis stream ID "<rv>-<seq>" into its rv and seq uint64 components, with
// ok=false for anything that is not a non-negative decimal in each present component. A bare "<rv>"
// (no "-seq") is accepted as seq 0 for robustness, though XRANGE/XREAD always report the full form.
func parseStreamID(id string) (uint64, uint64, bool) {
	major, sub, found := strings.Cut(id, "-")
	rv, errMajor := strconv.ParseUint(major, 10, 64)
	if errMajor != nil {
		return 0, 0, false
	}
	if !found {
		return rv, 0, true
	}
	seq, errSeq := strconv.ParseUint(sub, 10, 64)
	if errSeq != nil {
		return 0, 0, false
	}
	return rv, seq, true
}
