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

	"github.com/cespare/xxhash/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

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
type watchedTypeStore struct {
	refreshMu  sync.Mutex
	mu         sync.Mutex
	tables     map[string]WatchedTypeTable
	generation uint64
	rulesFP    uint64
	resolved   bool
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

	generation := m.apiResourceCatalog().Generation()
	fingerprint := m.rulesFingerprint()

	m.watchedTypes.mu.Lock()
	upToDate := m.watchedTypes.resolved &&
		m.watchedTypes.generation == generation &&
		m.watchedTypes.rulesFP == fingerprint
	m.watchedTypes.mu.Unlock()
	if upToDate {
		return
	}

	tables := m.resolveWatchedTypeTables(generation)

	m.watchedTypes.mu.Lock()
	previous := m.watchedTypes.tables
	m.watchedTypes.tables = tables
	m.watchedTypes.generation = generation
	m.watchedTypes.rulesFP = fingerprint
	m.watchedTypes.resolved = true
	m.watchedTypes.mu.Unlock()

	recordWatchedTypeMetrics(previous, tables)
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
// first under the same change gate. It is the union the informer set, the plan hash,
// and global visibility derive from.
func (m *Manager) allWatchedTypeTables() []WatchedTypeTable {
	m.refreshWatchedTypeTables()
	m.watchedTypes.mu.Lock()
	out := make([]WatchedTypeTable, 0, len(m.watchedTypes.tables))
	for _, table := range m.watchedTypes.tables {
		out = append(out, table)
	}
	m.watchedTypes.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].GitDest.Key() < out[j].GitDest.Key() })
	return out
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
