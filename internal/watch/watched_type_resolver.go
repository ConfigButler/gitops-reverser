// SPDX-License-Identifier: Apache-2.0

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

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// watchedTypeStore is the Manager's resident set of per-GitTarget watched-type tables.
// It is the single source of "what each GitTarget watches", read by the snapshot,
// informer, and plan-hash paths instead of each re-resolving inline. Re-projection is
// gated on a deliberate trigger — a rule-set change or a type-registry generation bump —
// so the common no-change reconcile is a cheap fingerprint compare rather than a rescan.
//
// Two locks: refreshMu serializes the whole resolve-and-publish so two concurrent
// refreshes (ReconcileForRuleChange runs from both watch-rule controllers and the
// manager loop) cannot have a slow older resolution overwrite a newer one; mu guards the
// published fields for concurrent readers. The registry owns the live-set removal grace,
// so nothing in this layer tracks absence.
type watchedTypeStore struct {
	refreshMu sync.Mutex
	mu        sync.Mutex
	tables    map[string]WatchedTypeTable
	// registriesFP fingerprints each active cluster's id paired with its registry revision, so a
	// discovery change on any source cluster re-projects. Unlike a naked sum of revisions it also
	// moves when the SET of active clusters changes even if the totals coincide.
	registriesFP uint64
	rulesFP      uint64
	// clusterFP fingerprints the GitTarget->source-cluster mapping. It is the third leg of the
	// re-projection gate: registriesFP knows the active clusters but not which GitTarget maps to
	// which, so it cannot see two GitTargets swapping clusters at a fixed active set; and the rules
	// fingerprint knows nothing about clusters at all. Without it a retargeted GitTarget could keep
	// the previous cluster's GVR table.
	clusterFP uint64
	resolved  bool
}

// refreshWatchedTypeTables re-projects the resident watched-type tables from the type
// registry's followable set when a deliberate trigger has fired since the last
// resolution: a rule-set change (the rules fingerprint moved) or a registry generation
// bump (discovery changed). A reconcile with neither change reuses the tables, which is
// what keeps the scan→registry→projection work off the hot path.
//
// Production callers refresh the catalog (and thus the registry) first via
// RefreshAPIResourceCatalog, so this never rebuilds the registry itself; it only does so
// lazily the first time, for unit tests that drive the store directly. The whole
// resolve-and-publish runs under refreshMu, so concurrent refreshes are serialized.
func (m *Manager) refreshWatchedTypeTables() {
	m.ensureWatchedTypeStore()
	m.watchedTypes.refreshMu.Lock()
	defer m.watchedTypes.refreshMu.Unlock()

	// Lazily populate each active cluster's registry the first time (unit tests drive this
	// path without RefreshAPIResourceCatalog); in production the catalog refresh keeps them
	// current, so the heavy scan→registry rebuild stays off this path.
	for _, id := range m.activeClusterIDs() {
		cc := m.cluster(id)
		if !cc.registry.Ready() {
			m.refreshClusterTypeRegistry(cc)
		}
	}

	// registriesFP spans every active cluster's (id, revision), so a remote CRD change — or a
	// change in the set of active clusters — re-projects the tables even when the local registry
	// is unchanged.
	registriesFP := m.activeRegistriesFingerprint()
	fingerprint := m.rulesFingerprint()
	// The cluster mapping fingerprint catches a GitTarget switching source clusters at a fixed
	// active set, which neither registriesFP nor the rules fingerprint can see.
	clusterFP := m.clusterMappingFingerprint()

	m.watchedTypes.mu.Lock()
	upToDate := m.watchedTypes.resolved &&
		m.watchedTypes.registriesFP == registriesFP &&
		m.watchedTypes.rulesFP == fingerprint &&
		m.watchedTypes.clusterFP == clusterFP
	m.watchedTypes.mu.Unlock()
	if upToDate {
		return
	}

	tables := m.resolveWatchedTypeTables()

	m.watchedTypes.mu.Lock()
	previous := m.watchedTypes.tables
	m.watchedTypes.tables = tables
	m.watchedTypes.registriesFP = registriesFP
	m.watchedTypes.rulesFP = fingerprint
	m.watchedTypes.clusterFP = clusterFP
	m.watchedTypes.resolved = true
	m.watchedTypes.mu.Unlock()

	recordWatchedTypeMetrics(previous, tables)
}

// recordWatchedTypeMetrics publishes the per-GitTarget watched-type count gauge after a
// re-resolution. A GitTarget present before but gone now is zeroed so its series does
// not linger.
func recordWatchedTypeMetrics(previous, current map[string]WatchedTypeTable) {
	if telemetry.WatchedTypes == nil {
		return
	}
	ctx := context.Background()
	for _, table := range current {
		telemetry.WatchedTypes.Record(ctx, int64(len(table.Types)), gitTargetAttrs(table.GitDest))
	}
	for key, table := range previous {
		if _, ok := current[key]; ok {
			continue
		}
		telemetry.WatchedTypes.Record(ctx, 0, gitTargetAttrs(table.GitDest))
	}
}

func gitTargetAttrs(gitDest types.ResourceReference) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
	)
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

// watchedTypeTableForGitDest returns the resident table for a GitTarget, refreshing the
// tables first. The bool reports whether the GitTarget currently has a table (i.e. any
// rules at all); a target whose rules resolve to nothing still returns an empty table.
func (m *Manager) watchedTypeTableForGitDest(gitDest types.ResourceReference) (WatchedTypeTable, bool) {
	m.refreshWatchedTypeTables()
	m.watchedTypes.mu.Lock()
	defer m.watchedTypes.mu.Unlock()
	table, ok := m.watchedTypes.tables[gitDest.Key()]
	return table, ok
}

// allWatchedTypeTables returns every resident table in a stable order, refreshing first.
// It is the once-per-reconcile read the plan hash and the requested-GVR set derive from.
func (m *Manager) allWatchedTypeTables() []WatchedTypeTable {
	m.refreshWatchedTypeTables()
	return m.residentWatchedTypeTables()
}

// residentWatchedTypeTables returns the currently published tables WITHOUT triggering a
// refresh. Callers on the reconcile hot path read this after ReconcileForRuleChange (or
// the snapshot gather) has already refreshed once, so per-read re-resolution stays off
// the path that runs per watched type.
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
// for callers that have already refreshed in the same operation. A target with no resident
// table (no rules) yields the zero table, which projects to an empty watch set.
func (m *Manager) residentWatchedTypeTable(gitDest types.ResourceReference) WatchedTypeTable {
	m.ensureWatchedTypeStore()
	m.watchedTypes.mu.Lock()
	defer m.watchedTypes.mu.Unlock()
	return m.watchedTypes.tables[gitDest.Key()]
}

// targetSelections accumulates one GitTarget's selected followable records and write
// destination while folding that target's rules.
type targetSelections struct {
	gitDest    types.ResourceReference
	dest       string
	selections []watchSelection
}

// activeRegistriesFingerprint hashes each active cluster's id paired with its registry revision,
// so the watched-type re-projection gate fires whenever ANY source cluster's discovery changes —
// and, unlike a naked SUM of revisions, it also moves when the SET of active clusters changes even
// if the totals happen to coincide (a cluster added and another removed at the same revision).
// Registry revisions are monotonic per cluster, so (sorted id, revision) pairs are a sound change
// detector. In a single-cluster install it is just the local registry's (id, revision).
func (m *Manager) activeRegistriesFingerprint() uint64 {
	ids := m.activeClusterIDs() // already sorted
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s=%d", id, m.cluster(id).registry.Revision()))
	}
	return xxhash.Sum64String(strings.Join(parts, "\x00"))
}

// clusterMappingFingerprint hashes the GitTarget->source-cluster mapping so the watched-type
// re-projection gate fires whenever a GitTarget's source cluster changes. The combined registry
// revision is a sum across active clusters and the rules fingerprint carries no cluster identity,
// so neither notices a GitTarget being retargeted (delete + recreate against a different cluster)
// when the old and new clusters happen to have equal registry revisions. Folding this in means a
// recreated GitTarget always re-resolves its table against the cluster it now mirrors, never
// keeping the previous cluster's GVRs. In a single-cluster install the mapping is empty (every
// target is local) and this is a constant, so the gate behaves exactly as before.
func (m *Manager) clusterMappingFingerprint() uint64 {
	m.gitTargetClustersMu.Lock()
	parts := make([]string, 0, len(m.gitTargetClusters))
	for gitTargetKey, clusterID := range m.gitTargetClusters {
		parts = append(parts, gitTargetKey+"\x1f"+clusterID)
	}
	m.gitTargetClustersMu.Unlock()
	sort.Strings(parts)
	return xxhash.Sum64String(strings.Join(parts, "\x00"))
}

// resolveWatchedTypeTables projects every GitTarget's rules onto ITS OWN source cluster's
// followable set — never a union: a WatchRule scopes its records to its own namespace, a
// ClusterWatchRule streams them cluster-wide, and a GitTarget that mirrors a remote resolves
// against that remote's registry. A GitTarget whose rules select nothing followable is kept as
// an empty table so a transient discovery gap does not look like rule removal. The per-target
// table is stamped with its own cluster's registry generation.
func (m *Manager) resolveWatchedTypeTables() map[string]WatchedTypeTable {
	if m.RuleStore == nil {
		return map[string]WatchedTypeTable{}
	}

	// Followable records are resolved per source cluster and cached, so several GitTargets
	// sharing one cluster fold against one snapshot.
	recordsByCluster := map[string][]typeset.TypeRecord{}
	recordsFor := func(clusterID string) []typeset.TypeRecord {
		if r, ok := recordsByCluster[clusterID]; ok {
			return r
		}
		r := m.cluster(clusterID).registry.Followable()
		recordsByCluster[clusterID] = r
		return r
	}

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

	m.collectWatchRuleSelections(recordsFor, get)
	m.collectClusterWatchRuleSelections(recordsFor, get)

	tables := make(map[string]WatchedTypeTable, len(byTarget))
	for key, ts := range byTarget {
		generation := m.cluster(m.clusterIDForGitTarget(ts.gitDest)).registry.Generation()
		table := buildWatchedTypeTable(ts.gitDest, generation, ts.selections)
		table.Dest = ts.dest
		tables[key] = table
	}
	return tables
}

// collectWatchRuleSelections folds every namespaced WatchRule into its GitTarget's selected
// records, scoping each record to the rule's own namespace and resolving against the
// GitTarget's own source cluster's followable set.
func (m *Manager) collectWatchRuleSelections(
	recordsFor func(clusterID string) []typeset.TypeRecord,
	get func(types.ResourceReference, string, string, string, string) *targetSelections,
) {
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		targetRef := types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		records := recordsFor(m.clusterIDForGitTarget(targetRef))
		ts := get(targetRef, rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path)
		for _, rr := range rule.ResourceRules {
			matched := matchFollowableRecords(
				records, rr.APIGroups, rr.APIVersions, rr.Resources, configv1alpha3.ResourceScopeNamespaced)
			for _, rec := range matched {
				// The rule's SOURCE namespace, NOT rule.Source.Namespace (which names the
				// WatchRule object in the control plane). These differ whenever the rule sets
				// spec.sourceNamespace, and this is the one place a config-plane namespace ever
				// became a data-plane "namespace" — a watch selector only, discarded once the
				// stream is open because every event's identity is rebuilt from the object's own
				// metadata.namespace. So changing it never moves anything in Git.
				ts.selections = append(ts.selections, watchSelection{
					record: rec, namespace: rule.SourceNamespace, ops: rr.Operations,
				})
			}
		}
	}
}

// collectClusterWatchRuleSelections folds every ClusterWatchRule into its GitTarget's selected
// records as cluster-wide streams, resolving against the GitTarget's own source cluster.
func (m *Manager) collectClusterWatchRuleSelections(
	recordsFor func(clusterID string) []typeset.TypeRecord,
	get func(types.ResourceReference, string, string, string, string) *targetSelections,
) {
	for _, rule := range m.RuleStore.SnapshotClusterWatchRules() {
		targetRef := types.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		records := recordsFor(m.clusterIDForGitTarget(targetRef))
		ts := get(targetRef, rule.GitProviderNamespace, rule.GitProviderRef, rule.Branch, rule.Path)
		for _, rr := range rule.Rules {
			matched := matchFollowableRecords(records, rr.APIGroups, rr.APIVersions, rr.Resources, rr.Scope)
			for _, rec := range matched {
				ts.selections = append(ts.selections, watchSelection{
					record: rec, namespace: "", ops: rr.Operations,
				})
			}
		}
	}
}

// matchFollowableRecords returns the followable records a rule selector names, applying
// group/version/resource/scope semantics over the registry's already-followable set — so
// a refused type (gvk-not-unique, denied-by-policy, verb-poor) simply never matches. It is
// the single rule-matching surface, shared by the per-GitTarget watched-type tables and by
// WatchRule/ClusterWatchRule status. It resolves one resource entry at a time (deduping
// records across entries) so it can preserve the resolver's ambiguity rule: when
// apiGroups is omitted and a *named* resource is served in more than one group, it is
// refused (watched in no group) rather than silently expanded across groups. A
// version-less entry collapses to the preferred version per (group, resource) so the same
// object is never watched under two versions; a "*" or explicit-version entry keeps every
// matched version.
func matchFollowableRecords(
	records []typeset.TypeRecord,
	groups, versions, resources []string,
	scope configv1alpha3.ResourceScope,
) []typeset.TypeRecord {
	var out []typeset.TypeRecord
	seen := map[schema.GroupVersionResource]struct{}{}
	for _, resource := range resources {
		resource = normalizeResource(resource)
		matched := recordsForResourceEntry(records, groups, versions, resource, scope)
		for _, rec := range matched {
			if _, dup := seen[rec.Identity.GVR]; dup {
				continue
			}
			seen[rec.Identity.GVR] = struct{}{}
			out = append(out, rec)
		}
	}
	return out
}

// recordsForResourceEntry resolves one (groups, versions, resource, scope) entry against
// the followable records, returning the records to watch — or nothing when the entry is
// ambiguous (omitted apiGroups, a named resource served in more than one group).
func recordsForResourceEntry(
	records []typeset.TypeRecord,
	groups, versions []string,
	resource string,
	scope configv1alpha3.ResourceScope,
) []typeset.TypeRecord {
	var matched []typeset.TypeRecord
	for _, rec := range records {
		gvr := rec.Identity.GVR
		if !matchesScope(rec.Identity.Scope == typeset.ScopeNamespaced, scope) {
			continue
		}
		if resource != "*" && gvr.Resource != resource {
			continue
		}
		if !matchLookupValue(groups, gvr.Group) {
			continue
		}
		if !matchLookupValue(versions, gvr.Version) {
			continue
		}
		matched = append(matched, rec)
	}
	if ambiguousAcrossGroups(groups, resource, matched) {
		return nil // omitted apiGroups can't disambiguate a multi-group resource: watch nothing
	}
	return choosePreferredRecordVersions(matched, versions)
}

// matchesScope reports whether a discovery namespaced flag aligns with a declared
// resource scope.
func matchesScope(namespaced bool, scope configv1alpha3.ResourceScope) bool {
	switch scope {
	case configv1alpha3.ResourceScopeNamespaced:
		return namespaced
	case configv1alpha3.ResourceScopeCluster:
		return !namespaced
	default:
		return false
	}
}

// ambiguousAcrossGroups is the omitted-apiGroups ambiguity rule: a named resource (not
// "*") selected without an apiGroups filter, matching records in more than one group, is
// ambiguous — the operator must name the group, so it is watched in none.
func ambiguousAcrossGroups(groups []string, resource string, matched []typeset.TypeRecord) bool {
	if len(groups) != 0 || resource == "*" {
		return false
	}
	distinct := map[string]struct{}{}
	for _, rec := range matched {
		distinct[rec.Identity.GVR.Group] = struct{}{}
	}
	return len(distinct) > 1
}

// choosePreferredRecordVersions collapses a version-less match to one record per
// (group, resource) — the preferred served version, else the first by version — so the
// same object is not watched under two served versions. When the selector names versions
// (explicitly or "*"), every matched version is kept.
func choosePreferredRecordVersions(records []typeset.TypeRecord, versions []string) []typeset.TypeRecord {
	if len(versions) != 0 {
		return records
	}
	byGroupResource := map[string][]typeset.TypeRecord{}
	for _, rec := range records {
		key := groupResourceKey(rec.Identity.GVR.Group, rec.Identity.GVR.Resource)
		byGroupResource[key] = append(byGroupResource[key], rec)
	}
	out := make([]typeset.TypeRecord, 0, len(byGroupResource))
	for _, candidates := range byGroupResource {
		out = append(out, preferredRecord(candidates))
	}
	return out
}

// preferredRecord picks the preferred served version among records for one
// (group, resource), falling back to the lowest version string for determinism.
func preferredRecord(records []typeset.TypeRecord) typeset.TypeRecord {
	sort.Slice(records, func(i, j int) bool {
		return records[i].Identity.GVR.Version < records[j].Identity.GVR.Version
	})
	selected := records[0]
	for _, rec := range records {
		if rec.Preferred {
			return rec
		}
	}
	return selected
}

// watchPlanDest renders a GitTarget's write destination fingerprint in the exact form
// the effective-plan hash uses, so the hash is byte-identical whether built from the
// table or inline.
func watchPlanDest(providerNS, provider, branch, path string) string {
	return fmt.Sprintf("provider=%s/%s|branch=%q|path=%q", providerNS, provider, branch, path)
}

// rulesFingerprint is a cheap, resolution-free hash of the raw rule inputs — the
// rule-change half of the re-projection gate. It moves whenever any rule input that
// could change a resolved table changes, and is deliberately over-sensitive rather than
// ever under-sensitive (a spurious rebuild is harmless; a missed one would leave the
// mirror stale).
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

// watchRuleFingerprint hashes everything about a compiled WatchRule that can change what it
// watches. The src= component MUST be the rule's SOURCE namespace, not the WatchRule object's own
// namespace: those diverge as soon as spec.sourceNamespace is set, and hashing the wrong one means
// an edit to that field does not re-project the watched-type table. That failure is silent — the
// old watch simply keeps running — which is why it is called out here rather than left to the
// field name.
func watchRuleFingerprint(rule rulestore.CompiledRule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wr|gt=%s/%s|src=%s|dest=%s",
		rule.GitTargetNamespace, rule.GitTargetRef, rule.SourceNamespace,
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

func operationsString(ops []configv1alpha3.OperationType) string {
	if len(ops) == 0 {
		return ""
	}
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = string(op)
	}
	return strings.Join(out, ",")
}
