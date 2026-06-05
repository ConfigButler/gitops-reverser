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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// defaultRemovalGrace is how long the watched-type store retains a still-selected type
// the catalog momentarily stopped serving before publishing its removal. It absorbs a
// transient discovery wobble (a CRD that flickers out of discovery, an apiserver that
// briefly lists fewer resources) so the absence must persist before it sweeps git.
const defaultRemovalGrace = 60 * time.Second

// watchedTypeStore is the Manager's resident set of per-GitTarget watched-type
// tables. It is the single source of "what each GitTarget watches", re-resolved
// only on a deliberate trigger (a rule-set change or a catalog generation bump)
// and read by the snapshot, informer, and plan-hash paths instead of each
// re-resolving inline. The fingerprint/generation pair is the change gate: a
// periodic reconcile that sees neither change reuses the resolved tables.
//
// Two locks: refreshMu serializes the whole compute-and-publish so two concurrent
// refreshes (ReconcileForRuleChange runs from both watch-rule controllers and the
// manager loop) cannot have a slow older resolution overwrite a newer one; mu
// guards the published fields for concurrent readers.
//
// The pending-removal state implements "trusted, persistent absence": a type the
// catalog momentarily stops serving while the rules still select it is held (retained
// in the published table, snapshot blocked) until the absence persists past
// removalGrace. pendingRemovals/removalGrace/now are only ever touched while refreshMu
// is held, so they need no separate lock; now is injectable for deterministic tests.
type watchedTypeStore struct {
	refreshMu  sync.Mutex
	mu         sync.Mutex
	tables     map[string]WatchedTypeTable
	generation uint64
	rulesFP    uint64
	resolved   bool

	pendingRemovals map[string]map[typeKey]pendingRemoval
	removalGrace    time.Duration
	now             func() time.Time
}

// typeKey is a watched type's identity for absence tracking: the (GVK, GVR, scope)
// triple. The namespace/operation scope is deliberately NOT part of the key — it lives
// inside the retained WatchedType — so a type held while the catalog wobbles is matched
// back to its candidate regardless of how its namespaces were last gathered.
type typeKey struct {
	gvk   schema.GroupVersionKind
	gvr   schema.GroupVersionResource
	scope configv1alpha1.ResourceScope
}

// pendingRemoval is the in-flight grace state for one held type: the retained
// WatchedType (carried so informers keep its exact namespace scope), when the absence
// was first observed (since), the catalog generation it was observed at, and the
// human-readable reason for the edge logs.
type pendingRemoval struct {
	wt         WatchedType
	since      time.Time
	generation uint64
	reason     string
}

// refreshWatchedTypeTables re-resolves the resident watched-type tables when a
// deliberate trigger has fired since the last resolution: a rule-set change (the
// rules fingerprint moved) or a catalog generation bump (a CRD installed, removed,
// or upgraded). A periodic reconcile with neither change reuses the tables, which
// is what keeps re-resolution off the hot path. Callers must have refreshed the
// catalog first so the generation read here reflects the latest discovery.
//
// The whole compute-and-publish runs under refreshMu, so refreshes are serialized:
// a slow older resolution can never overwrite a table a newer resolution already
// published. Because refreshes complete in order, the final published state always
// reflects the most recent (generation, fingerprint) any refresh observed.
func (m *Manager) refreshWatchedTypeTables() {
	m.ensureWatchedTypeStore()
	m.watchedTypes.refreshMu.Lock()
	defer m.watchedTypes.refreshMu.Unlock()

	if m.watchedTypes.now == nil {
		m.watchedTypes.now = time.Now
	}
	if m.watchedTypes.removalGrace == 0 {
		m.watchedTypes.removalGrace = defaultRemovalGrace
	}

	generation := m.apiResourceCatalog().Generation()
	fingerprint := m.rulesFingerprint()

	m.watchedTypes.mu.Lock()
	previous := m.watchedTypes.tables
	upToDate := m.watchedTypes.resolved &&
		m.watchedTypes.generation == generation &&
		m.watchedTypes.rulesFP == fingerprint
	m.watchedTypes.mu.Unlock()

	// A pending removal is a time-based hold, so the change gate must not short-circuit
	// while one is in flight even when neither the rules nor the catalog generation
	// moved: the grace timer may have elapsed since the last refresh and the type now
	// needs publishing as removed.
	if upToDate && !m.hasPendingRemovals() {
		return
	}

	candidate := m.resolveWatchedTypeTables(generation)
	tables := m.applyPersistentAbsence(previous, candidate, generation)

	m.watchedTypes.mu.Lock()
	m.watchedTypes.tables = tables
	m.watchedTypes.generation = generation
	m.watchedTypes.rulesFP = fingerprint
	m.watchedTypes.resolved = true
	m.watchedTypes.mu.Unlock()

	recordWatchedTypeMetrics(previous, tables)
}

// hasPendingRemovals reports whether any GitTarget is currently holding a type under a
// removal grace timer. Read under refreshMu (held by the only mutator), so it needs no
// extra lock.
func (m *Manager) hasPendingRemovals() bool {
	for _, byType := range m.watchedTypes.pendingRemovals {
		if len(byType) > 0 {
			return true
		}
	}
	return false
}

// applyPersistentAbsence reconciles the freshly resolved candidate tables against the
// previously published tables and the in-flight grace timers, then returns the tables to
// publish. It is the watched-type layer's "trusted, persistent absence" policy and the
// reason a discovery wobble (resources: 7 -> 0 -> 7) no longer sweeps git:
//
//   - A type the candidate still lists wins outright (fresh resolution). If it had been
//     held, the hold is cleared — the type reappeared.
//   - A previously published type the candidate no longer lists is judged by intent and
//     trust, never by the bare absence:
//   - the rules no longer select it          -> removed immediately (explicit untrack);
//   - the catalog is unavailable/degraded     -> retained indefinitely (unobservable,
//     snapshot already blocked by the table's blocking misses);
//   - the rules still select it, healthy catalog says absent -> held under the grace
//     timer, retained until the absence persists past removalGrace, then removed.
//   - A GitTarget entirely absent from the candidate (all its rules deleted) is dropped
//     with any pending state: rule deletion is an explicit, immediate untrack.
//
// It mutates the caller-owned candidate map in place and replaces the store's pending
// state; it runs under refreshMu.
func (m *Manager) applyPersistentAbsence(
	previous, candidate map[string]WatchedTypeTable,
	generation uint64,
) map[string]WatchedTypeTable {
	now := m.watchedTypes.now()
	grace := m.watchedTypes.removalGrace
	newPending := map[string]map[typeKey]pendingRemoval{}

	for key := range candidate {
		table := candidate[key]
		oldPending := m.watchedTypes.pendingRemovals[key]
		m.logReappearances(table, oldPending)

		prev, hadPrev := previous[key]
		if hadPrev {
			retained, pending := m.holdAbsentTypes(table, prev, oldPending, now, grace, generation)
			table.Types = append(table.Types, retained...)
			sortWatchedTypes(table.Types)
			table.PendingRemovals = pendingRemovalList(pending)
			if len(pending) > 0 {
				newPending[key] = pending
			}
		}
		candidate[key] = table
	}

	m.watchedTypes.pendingRemovals = newPending
	return candidate
}

// holdAbsentTypes walks the previously published types missing from the candidate and
// decides each one's fate per the persistent-absence policy (see applyPersistentAbsence).
// It returns the types to retain in the published table and the grace timers to keep
// running. Blocking (unobservable) retention carries no timer — it is released only when
// the catalog becomes trustworthy again — so it is retained but not added to pending.
func (m *Manager) holdAbsentTypes(
	cand, prev WatchedTypeTable,
	oldPending map[typeKey]pendingRemoval,
	now time.Time,
	grace time.Duration,
	generation uint64,
) ([]WatchedType, map[typeKey]pendingRemoval) {
	candByKey := indexTypesByKey(cand.Types)
	blocking := len(cand.BlockingMisses()) > 0
	var retained []WatchedType
	pending := map[typeKey]pendingRemoval{}

	for _, pt := range prev.Types {
		key := watchedTypeKey(pt)
		if _, present := candByKey[key]; present {
			continue // candidate still serves it; the fresh entry already wins
		}
		if !m.rulesStillSelectWatchedType(cand.GitDest, pt) {
			m.logAbsence(cand.GitDest, pt, "watched type no longer selected by any rule; removing immediately")
			continue // explicit rule removal -> immediate untrack
		}
		if blocking {
			m.logAbsence(cand.GitDest, pt,
				"watched type absent but discovery is degraded/unavailable; retaining indefinitely (snapshot blocked)")
			retained = append(retained, pt)
			continue // unobservable surface is never a trusted absence
		}
		pr, existed := oldPending[key]
		if !existed {
			pr = pendingRemoval{
				wt:         pt,
				since:      now,
				generation: generation,
				reason:     "catalog no longer serves a still-selected type",
			}
			m.logAbsence(cand.GitDest, pt, "watched type absent; holding removal")
		}
		if now.Sub(pr.since) >= grace {
			m.logAbsence(cand.GitDest, pt, "watched type absence persisted past grace; removing")
			continue // grace elapsed -> the absence is trusted, allow the removal
		}
		retained = append(retained, pt)
		pending[key] = pr
	}
	return retained, pending
}

// logReappearances emits the edge log for every held type the candidate now serves
// again, so a recovered wobble is observable. The actual clearing of the hold is
// implicit: a reappeared type is in the candidate, so holdAbsentTypes never re-adds it
// to the pending set.
func (m *Manager) logReappearances(cand WatchedTypeTable, oldPending map[typeKey]pendingRemoval) {
	if len(oldPending) == 0 {
		return
	}
	candByKey := indexTypesByKey(cand.Types)
	for key, pr := range oldPending {
		if _, present := candByKey[key]; present {
			m.logAbsence(cand.GitDest, pr.wt, "watched type reappeared; clearing pending removal")
		}
	}
}

// logAbsence emits one persistent-absence edge log identifying the GitTarget and type.
func (m *Manager) logAbsence(gitDest types.ResourceReference, wt WatchedType, msg string) {
	m.Log.Info(msg,
		"gitTarget", gitDest.String(),
		"gvk", wt.GVK.String(),
		"gvr", wt.GVR.String(),
		"scope", string(wt.Scope))
}

// watchedTypeKey projects a WatchedType onto its absence-tracking identity.
func watchedTypeKey(wt WatchedType) typeKey {
	return typeKey{gvk: wt.GVK, gvr: wt.GVR, scope: wt.Scope}
}

// indexTypesByKey indexes a type slice by its absence-tracking identity.
func indexTypesByKey(watched []WatchedType) map[typeKey]WatchedType {
	out := make(map[typeKey]WatchedType, len(watched))
	for _, wt := range watched {
		out[watchedTypeKey(wt)] = wt
	}
	return out
}

// pendingRemovalList renders the grace-timer map as the table's stable, sorted
// PendingRemovals slice.
func pendingRemovalList(pending map[typeKey]pendingRemoval) []PendingRemoval {
	out := make([]PendingRemoval, 0, len(pending))
	for _, pr := range pending {
		out = append(out, PendingRemoval{Type: pr.wt, Since: pr.since, Reason: pr.reason})
	}
	sort.Slice(out, func(i, j int) bool {
		return gvkSortKey(out[i].Type.GVK) < gvkSortKey(out[j].Type.GVK)
	})
	return out
}

// pendingRemovalSummary renders the held types for the snapshot fail-closed error,
// naming each GVK so a blocked gather log says exactly which wobbling types caused it.
func pendingRemovalSummary(pending []PendingRemoval) string {
	parts := make([]string, 0, len(pending))
	for _, pr := range pending {
		parts = append(parts, pr.Type.GVK.String())
	}
	if len(parts) == 1 {
		return "watched type " + parts[0]
	}
	return fmt.Sprintf("%d watched types [%s]", len(parts), strings.Join(parts, ", "))
}

// rulesStillSelectWatchedType reports whether the GitTarget's raw compiled rules still
// name the given watched type, using rule intent alone — never the catalog. This is what
// separates an explicit untrack (the user edited the rules so nothing selects the type
// any more -> remove immediately) from a catalog wobble (the rules still select it, the
// apiserver momentarily stopped serving it -> hold under grace). Matching is by the
// type's group/version/resource plus its cluster-wide-vs-namespaced shape: a cluster-wide
// type can only be (re)produced by a ClusterWatchRule at the same scope; a per-namespace
// type can only be (re)produced by a WatchRule in one of the namespaces it was gathered
// under.
func (m *Manager) rulesStillSelectWatchedType(gitDest types.ResourceReference, wt WatchedType) bool {
	if m.RuleStore == nil {
		return false
	}
	if wt.ClusterWide() {
		return m.clusterRulesSelectType(gitDest, wt)
	}
	return m.watchRulesSelectType(gitDest, wt)
}

func (m *Manager) clusterRulesSelectType(gitDest types.ResourceReference, wt WatchedType) bool {
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		if types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace).Key() != gitDest.Key() {
			continue
		}
		for _, rr := range rule.Rules {
			if rr.Scope == wt.Scope && resourceRuleSelectsType(rr.APIGroups, rr.APIVersions, rr.Resources, wt) {
				return true
			}
		}
	}
	return false
}

func (m *Manager) watchRulesSelectType(gitDest types.ResourceReference, wt WatchedType) bool {
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		if types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace).Key() != gitDest.Key() {
			continue
		}
		// A WatchRule scopes its resources to its own namespace, so it only keeps a
		// per-namespace type selected if it lives in a namespace that type was gathered
		// under.
		if _, ok := wt.NamespaceOps[rule.Source.Namespace]; !ok {
			continue
		}
		for _, rr := range rule.ResourceRules {
			if resourceRuleSelectsType(rr.APIGroups, rr.APIVersions, rr.Resources, wt) {
				return true
			}
		}
	}
	return false
}

// resourceRuleSelectsType reports whether one rule's (apiGroups, apiVersions, resources)
// selector names the watched type's GVR. Group/version selectors follow the resolver's
// semantics — empty or "*" matches anything — while the resource must be named explicitly
// or with "*", since a rule never selects every resource implicitly.
func resourceRuleSelectsType(groups, versions, resources []string, wt WatchedType) bool {
	return matchLookupValue(groups, wt.GVR.Group) &&
		matchLookupValue(versions, wt.GVR.Version) &&
		matchResourceSelector(resources, wt.GVR.Resource)
}

func matchResourceSelector(resources []string, resource string) bool {
	for _, r := range resources {
		n := normalizeResource(r)
		if n == "*" || n == resource {
			return true
		}
	}
	return false
}

// recordWatchedTypeMetrics publishes the basic per-GitTarget watched-type gauges (M10)
// after a re-resolution: the resolved type count and the count of GVKs refused for the
// GVK<->GVR 1:1 violation. A GitTarget present before but gone now is zeroed so its
// series does not linger. Recording only on a real re-resolution is correct for level
// gauges, which hold their last value across the gated no-change reconciles.
func recordWatchedTypeMetrics(previous, current map[string]WatchedTypeTable) {
	if telemetry.WatchedTypes == nil || telemetry.WatchedTypeConflicts == nil {
		return
	}
	ctx := context.Background()
	for _, table := range current {
		attrs := metric.WithAttributes(
			attribute.String("gittarget_namespace", table.GitDest.Namespace),
			attribute.String("gittarget_name", table.GitDest.Name),
		)
		telemetry.WatchedTypes.Record(ctx, int64(len(table.Types)), attrs)
		telemetry.WatchedTypeConflicts.Record(ctx, int64(len(table.Conflicts)), attrs)
		if telemetry.WatchedTypePendingRemovals != nil {
			telemetry.WatchedTypePendingRemovals.Record(ctx, int64(len(table.PendingRemovals)), attrs)
		}
	}
	for key, table := range previous {
		if _, ok := current[key]; ok {
			continue
		}
		attrs := metric.WithAttributes(
			attribute.String("gittarget_namespace", table.GitDest.Namespace),
			attribute.String("gittarget_name", table.GitDest.Name),
		)
		telemetry.WatchedTypes.Record(ctx, 0, attrs)
		telemetry.WatchedTypeConflicts.Record(ctx, 0, attrs)
		if telemetry.WatchedTypePendingRemovals != nil {
			telemetry.WatchedTypePendingRemovals.Record(ctx, 0, attrs)
		}
	}
}

// ensureWatchedTypeStore lazily initialises the resident store so a zero-value
// Manager (used widely in tests) does not need explicit setup.
func (m *Manager) ensureWatchedTypeStore() {
	m.watchedTypeInit.Do(func() {
		if m.watchedTypes == nil {
			m.watchedTypes = &watchedTypeStore{}
		}
	})
}

// watchedTypeTableForGitDest returns the resident table for a GitTarget. It refreshes
// the tables first — gated on a rule or catalog change, so the common no-change call is
// a cheap fingerprint compare — so a caller always reads resolution current with the
// rules, not a stale cache. The bool reports whether the GitTarget currently has a
// table (i.e. any rules at all); a target whose rules resolve to nothing still returns
// an empty table.
func (m *Manager) watchedTypeTableForGitDest(gitDest types.ResourceReference) (WatchedTypeTable, bool) {
	m.refreshWatchedTypeTables()
	m.watchedTypes.mu.Lock()
	defer m.watchedTypes.mu.Unlock()
	table, ok := m.watchedTypes.tables[gitDest.Key()]
	return table, ok
}

// allWatchedTypeTables returns every resident table in a stable order, refreshing
// first under the same change gate. It is the once-per-reconcile read the plan hash
// and the requested-GVR set derive from (and the entry point direct-call tests use).
func (m *Manager) allWatchedTypeTables() []WatchedTypeTable {
	m.refreshWatchedTypeTables()
	return m.residentWatchedTypeTables()
}

// residentWatchedTypeTables returns the currently published tables WITHOUT triggering a
// refresh. Callers on the reconcile hot path read this after ReconcileForRuleChange (or
// the snapshot gather) has already refreshed once, so per-read re-resolution and its
// refreshMu contention stay off the path that runs per watched type.
func (m *Manager) residentWatchedTypeTables() []WatchedTypeTable {
	m.ensureWatchedTypeStore()
	m.watchedTypes.mu.Lock()
	out := make([]WatchedTypeTable, 0, len(m.watchedTypes.tables))
	for _, table := range m.watchedTypes.tables {
		out = append(out, table)
	}
	m.watchedTypes.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].GitDest.Key() < out[j].GitDest.Key() })
	return out
}

// residentWatchedTypeTable returns one GitTarget's published table without refreshing,
// for callers that have already refreshed in the same operation.
func (m *Manager) residentWatchedTypeTable(gitDest types.ResourceReference) (WatchedTypeTable, bool) {
	m.ensureWatchedTypeStore()
	m.watchedTypes.mu.Lock()
	defer m.watchedTypes.mu.Unlock()
	table, ok := m.watchedTypes.tables[gitDest.Key()]
	return table, ok
}

// targetSelections accumulates one GitTarget's resolved selections, destination,
// and resolve misses while folding that target's rules.
type targetSelections struct {
	gitDest    types.ResourceReference
	dest       string
	selections []resolvedSelection
	misses     []ResolveMiss
}

// resolveWatchedTypeTables resolves every GitTarget's rules into a watched-type
// table. It mirrors the per-target rule iteration the snapshot path did inline
// (collectWatchRuleGVRs / collectClusterWatchRuleGVRs) but produces one resident
// table per GitTarget, folding types by GVK and enforcing the GVK<->GVR 1:1
// assumption. A GitTarget whose rules resolve to nothing is kept as an empty table
// so a transient discovery gap does not look like rule removal.
func (m *Manager) resolveWatchedTypeTables(generation uint64) map[string]WatchedTypeTable {
	if m.RuleStore == nil {
		return map[string]WatchedTypeTable{}
	}
	resolver := m.ruleGVRResolver()
	catalog := m.apiResourceCatalog()

	byTarget := map[string]*targetSelections{}
	get := func(ref types.ResourceReference, providerNS, provider, branch, path string) *targetSelections {
		key := ref.Key()
		ts := byTarget[key]
		if ts == nil {
			ts = &targetSelections{gitDest: ref}
			byTarget[key] = ts
		}
		ts.dest = watchPlanDest(providerNS, provider, branch, path)
		return ts
	}

	m.collectWatchRuleSelections(resolver, get)
	m.collectClusterWatchRuleSelections(resolver, get)

	tables := make(map[string]WatchedTypeTable, len(byTarget))
	for key, ts := range byTarget {
		table := buildWatchedTypeTable(ts.gitDest, generation, ts.selections, catalog)
		table.Dest = ts.dest
		table.Misses = ts.misses
		tables[key] = table
		m.logTypeConflicts(table)
	}
	return tables
}

// logTypeConflicts surfaces every GVK refused for the GVK<->GVR 1:1 assumption, so a
// type that is silently not watched because a kind is served by more than one resource
// is loud rather than invisible. This is the M10 observability surface for the refusal
// (paired with the gitopsreverser_watched_type_conflicts gauge); a bounded per-GitTarget
// status roll-up follows in M11.
func (m *Manager) logTypeConflicts(table WatchedTypeTable) {
	for _, conflict := range table.Conflicts {
		m.Log.Info("refusing to watch a type: its GVK resolves to more than one served "+
			"resource (GitOps Reverser requires GVK<->GVR to be 1:1) — disambiguate the watch "+
			"rules or fix the cluster API surface",
			"gitTarget", table.GitDest.String(),
			"gvk", conflict.GVK.String(),
			"resources", gvrStrings(conflict.GVRs))
	}
}

// gvrStrings renders conflicting GVRs for logging.
func gvrStrings(gvrs []schema.GroupVersionResource) []string {
	out := make([]string, len(gvrs))
	for i, gvr := range gvrs {
		out[i] = gvr.String()
	}
	return out
}

// collectWatchRuleSelections folds every namespaced WatchRule into its GitTarget's
// selections, scoping each resolved GVR to the rule's own namespace.
func (m *Manager) collectWatchRuleSelections(
	resolver *RuleGVRResolver,
	get func(types.ResourceReference, string, string, string, string) *targetSelections,
) {
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		ts := get(
			types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace),
			rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path,
		)
		for _, rr := range rule.ResourceRules {
			gvrs, misses := resolver.Resolve(rr.APIGroups, rr.APIVersions, rr.Resources,
				configv1alpha1.ResourceScopeNamespaced)
			ts.misses = append(ts.misses, misses...)
			for _, gvr := range gvrs {
				ts.selections = append(ts.selections, resolvedSelection{
					gvr: gvr, namespace: rule.Source.Namespace, ops: rr.Operations,
				})
			}
		}
	}
}

// collectClusterWatchRuleSelections folds every ClusterWatchRule into its
// GitTarget's selections as cluster-wide streams.
func (m *Manager) collectClusterWatchRuleSelections(
	resolver *RuleGVRResolver,
	get func(types.ResourceReference, string, string, string, string) *targetSelections,
) {
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		ts := get(
			types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace),
			rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path,
		)
		for _, rr := range rule.Rules {
			gvrs, misses := resolver.Resolve(rr.APIGroups, rr.APIVersions, rr.Resources, rr.Scope)
			ts.misses = append(ts.misses, misses...)
			for _, gvr := range gvrs {
				ts.selections = append(ts.selections, resolvedSelection{
					gvr: gvr, namespace: "", ops: rr.Operations,
				})
			}
		}
	}
}

// watchPlanDest renders a GitTarget's write destination fingerprint in the exact
// form the effective-plan hash uses, so the hash is byte-identical whether built
// from the table or inline.
func watchPlanDest(providerNS, provider, branch, path string) string {
	return fmt.Sprintf("provider=%s/%s|branch=%q|path=%q", providerNS, provider, branch, path)
}

// rulesFingerprint is a cheap, resolution-free hash of the raw rule inputs. It is
// the rule-change half of the watched-type re-resolution gate: it moves whenever
// any rule input that could change a resolved table changes, and is deliberately
// over-sensitive rather than ever under-sensitive (a spurious rebuild is harmless;
// a missed one would leave the mirror stale).
func (m *Manager) rulesFingerprint() uint64 {
	if m.RuleStore == nil {
		return 0
	}
	var parts []string
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		parts = append(parts, watchRuleFingerprint(rule))
	}
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		parts = append(parts, clusterWatchRuleFingerprint(rule))
	}
	sort.Strings(parts)
	return xxhash.Sum64String(strings.Join(parts, "\x00"))
}

func watchRuleFingerprint(rule rulestore.CompiledRule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wr|gt=%s/%s|src=%s|dest=%s",
		rule.GitTargetNamespace, rule.GitTargetRef, rule.Source.Namespace,
		watchPlanDest(rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path))
	for _, rr := range rule.ResourceRules {
		fmt.Fprintf(&b, "|rr[g=%s;v=%s;r=%s;op=%s]",
			strings.Join(rr.APIGroups, ","), strings.Join(rr.APIVersions, ","),
			strings.Join(rr.Resources, ","), operationsString(rr.Operations))
	}
	return b.String()
}

func clusterWatchRuleFingerprint(rule rulestore.CompiledClusterRule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cwr|gt=%s/%s|dest=%s",
		rule.GitTargetNamespace, rule.GitTargetRef,
		watchPlanDest(rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path))
	for _, rr := range rule.Rules {
		fmt.Fprintf(&b, "|rr[g=%s;v=%s;r=%s;op=%s;scope=%s]",
			strings.Join(rr.APIGroups, ","), strings.Join(rr.APIVersions, ","),
			strings.Join(rr.Resources, ","), operationsString(rr.Operations), rr.Scope)
	}
	return b.String()
}

// watchPlanFromTable reconstructs a GitTarget's effective-watch-plan hash input from
// its resident watched-type table. It re-emits the exact (GVR, scope, namespace,
// operations) entries the inline resolution used to build via addEntry, so the plan
// hash that drives snapshot selection is byte-identical to the pre-M10 hash. Each
// (type, namespace) pair maps to one plan entry; the empty namespace is a cluster-wide
// stream, matching how ClusterWatchRules recorded entries.
func watchPlanFromTable(table WatchedTypeTable) *targetWatchPlan {
	p := &targetWatchPlan{
		gitDest: table.GitDest,
		entries: make(map[string]map[string]struct{}),
		dest:    table.Dest,
	}
	for _, wt := range table.Types {
		gvr := GVR{Group: wt.GVR.Group, Version: wt.GVR.Version, Resource: wt.GVR.Resource, Scope: wt.Scope}
		for ns, opSet := range wt.NamespaceOps {
			p.addEntry(gvr, ns, operationSetToTypes(opSet))
		}
	}
	return p
}

// operationSetToTypes converts a normalised OperationSet back into the OperationType
// slice addEntry expects, mapping the "*" sentinel to OperationAll. addEntry
// re-normalises identically, so the round trip is lossless.
func operationSetToTypes(s OperationSet) []configv1alpha1.OperationType {
	out := make([]configv1alpha1.OperationType, 0, len(s))
	for op := range s {
		if op == "*" {
			out = append(out, configv1alpha1.OperationAll)
			continue
		}
		out = append(out, configv1alpha1.OperationType(op))
	}
	return out
}

func operationsString(ops []configv1alpha1.OperationType) string {
	if len(ops) == 0 {
		return ""
	}
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = string(op)
	}
	return strings.Join(out, ",")
}
