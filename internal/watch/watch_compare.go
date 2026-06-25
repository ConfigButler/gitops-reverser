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
	"reflect"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// The watch-audit comparison is the PAYOFF of Phase 1 (build watch state in parallel,
// docs/design/watch-only-ingestion-architecture.md): once both the audit-derived and watch-derived
// desired sets exist for a type, periodically diff them and meter the divergence. It answers the one
// question the corpus cannot — "does a continuous watch reproduce the same desired manifests the
// audit log does, in a live cluster?" — and it changes NO Git write. It runs only when the parallel
// watch-state stream is enabled (m.WatchStateSplicer set by --watch-state-stream).

// watchCompareInterval is how often the comparator diffs every serviceable type's two desired sets.
// A minute is far below the checkpoint re-anchor cadence, so each pass sees a settled checkpoint plus
// whatever each log accumulated since.
const watchCompareInterval = time.Minute

// StateSplicer folds a type's checkpoint with the parallel :watch:stream into the watch-derived
// desired set (queue.RedisTypeSplicer.SpliceWatchType). It is the read side of the Phase 1
// comparison; the Manager diffs it against the audit-derived set (TypeSplicer.SpliceType). Optional
// on the Manager: nil disables the comparison (the default, unless --watch-state-stream is set).
type StateSplicer interface {
	SpliceWatchType(ctx context.Context, group, resource string) ([]*unstructured.Unstructured, string, error)
}

// Divergence is the per-type result of comparing the audit-derived and watch-derived desired sets.
// Identity is "<namespace>/<name>" (cluster-scoped: "<name>"); equality is a deep compare of the
// sanitized object bodies (both splices sanitize), so a Mismatch means watch and audit would write
// different manifests for the same object.
type Divergence struct {
	AuditCount int
	WatchCount int
	AuditOnly  []string // identities in audit's set but not watch's (watch missed/lost an object)
	WatchOnly  []string // identities in watch's set but not audit's (watch carries an extra object)
	Mismatch   []string // identities in both, sanitized bodies differ
	Agree      int      // identities in both, bodies equal
}

// diverged reports whether the two sets differ in any way.
func (d Divergence) diverged() bool {
	return len(d.AuditOnly) > 0 || len(d.WatchOnly) > 0 || len(d.Mismatch) > 0
}

// compareDesiredSets diffs two desired object sets by identity and sanitized body. It is pure so the
// diff is unit-testable without Redis or a cluster.
func compareDesiredSets(auditSet, watchSet []*unstructured.Unstructured) Divergence {
	auditByID := indexByIdentity(auditSet)
	watchByID := indexByIdentity(watchSet)
	div := Divergence{AuditCount: len(auditByID), WatchCount: len(watchByID)}
	for id, a := range auditByID {
		w, ok := watchByID[id]
		switch {
		case !ok:
			div.AuditOnly = append(div.AuditOnly, id)
		case reflect.DeepEqual(a.Object, w.Object):
			div.Agree++
		default:
			div.Mismatch = append(div.Mismatch, id)
		}
	}
	for id := range watchByID {
		if _, ok := auditByID[id]; !ok {
			div.WatchOnly = append(div.WatchOnly, id)
		}
	}
	sort.Strings(div.AuditOnly)
	sort.Strings(div.WatchOnly)
	sort.Strings(div.Mismatch)
	return div
}

// indexByIdentity keys a desired set by object identity, last-writer-wins on a duplicate identity
// (the splice already dedups, so duplicates are not expected).
func indexByIdentity(objs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	m := make(map[string]*unstructured.Unstructured, len(objs))
	for _, o := range objs {
		m[objectIdentity(o)] = o
	}
	return m
}

// startWatchComparator launches the periodic watch-audit comparison loop when the parallel stream is
// enabled (both splices wired). It is a no-op otherwise, so the default build pays nothing.
func (m *Manager) startWatchComparator(ctx context.Context, log logr.Logger) {
	if m.WatchStateSplicer == nil || m.TypeSplicer == nil {
		return
	}
	interval := watchCompareInterval
	if m.watchCompareIntervalOverride > 0 {
		interval = m.watchCompareIntervalOverride
	}
	log.Info("watch-audit comparator started (experimental; diffs watch vs audit desired sets, no effect on Git)",
		"interval", interval.String())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.compareWatchAuditAllTypes(ctx, log)
			}
		}
	}()
}

// compareWatchAuditAllTypes diffs every serviceable type's two desired sets in one pass. A type is
// serviceable once it holds a checkpoint (Synced/Resyncing/Failing-with-prior), which is exactly when
// both splices can answer; types without a checkpoint are skipped (nothing to compare yet).
func (m *Manager) compareWatchAuditAllTypes(ctx context.Context, log logr.Logger) {
	for _, t := range m.materializerInstance().Inventory() {
		if !t.Serviceable() {
			continue
		}
		div, ok := m.computeWatchAuditDivergence(ctx, t.GVR)
		if !ok {
			continue
		}
		m.recordWatchAuditDivergence(ctx, t.GVR, div)
		m.logWatchAuditDivergence(log, t.GVR, div)
	}
}

// computeWatchAuditDivergence reads both desired sets for a type and diffs them. It returns ok=false
// when either splice cannot answer (no checkpoint yet, or a transient read error) so the caller skips
// the type this pass rather than reporting a spurious divergence.
func (m *Manager) computeWatchAuditDivergence(
	ctx context.Context, gvr schema.GroupVersionResource,
) (Divergence, bool) {
	auditSet, _, _, aErr := m.TypeSplicer.SpliceType(ctx, gvr.Group, gvr.Resource)
	if aErr != nil {
		return Divergence{}, false
	}
	watchSet, _, wErr := m.WatchStateSplicer.SpliceWatchType(ctx, gvr.Group, gvr.Resource)
	if wErr != nil {
		return Divergence{}, false
	}
	return compareDesiredSets(auditSet, watchSet), true
}

// recordWatchAuditDivergence publishes the per-reason divergence gauge and the agree/diverge counter.
// The gauge is re-recorded every pass (including zeros) so a healed divergence reads 0, not stale.
func (m *Manager) recordWatchAuditDivergence(
	ctx context.Context, gvr schema.GroupVersionResource, div Divergence,
) {
	if telemetry.WatchAuditDivergence != nil {
		record := func(reason string, n int) {
			telemetry.WatchAuditDivergence.Record(ctx, int64(n), metric.WithAttributes(
				attribute.String("gvr", gvr.String()), attribute.String("reason", reason)))
		}
		record("audit_only", len(div.AuditOnly))
		record("watch_only", len(div.WatchOnly))
		record("mismatch", len(div.Mismatch))
	}
	if telemetry.WatchAuditComparisonsTotal != nil {
		result := "agree"
		if div.diverged() {
			result = "diverge"
		}
		telemetry.WatchAuditComparisonsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gvr", gvr.String()), attribute.String("result", result)))
	}
}

// logWatchAuditDivergence logs one comparison: a bounded sample of the diverging identities at Info
// when they differ (the interesting case to eyeball), or a terse agreement line at V(1).
func (m *Manager) logWatchAuditDivergence(log logr.Logger, gvr schema.GroupVersionResource, div Divergence) {
	if !div.diverged() {
		log.V(1).Info("watch-audit agree", "gvr", gvr.String(), "objects", div.Agree)
		return
	}
	log.Info("watch-audit divergence",
		"gvr", gvr.String(),
		"auditCount", div.AuditCount, "watchCount", div.WatchCount,
		"auditOnly", len(div.AuditOnly), "watchOnly", len(div.WatchOnly),
		"mismatch", len(div.Mismatch), "agree", div.Agree,
		"auditOnlySample", sampleIdentities(div.AuditOnly),
		"watchOnlySample", sampleIdentities(div.WatchOnly),
		"mismatchSample", sampleIdentities(div.Mismatch))
}

// sampleIdentities returns at most the first few identities, so a large divergence does not flood a
// log line while still naming concrete objects to investigate.
func sampleIdentities(ids []string) []string {
	const limit = 5
	if len(ids) <= limit {
		return ids
	}
	return ids[:limit]
}
