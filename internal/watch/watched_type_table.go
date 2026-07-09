// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// OperationSet is the set of operation filters recorded for a watched type in one
// namespace. The sentinel "*" means all operations and subsumes the rest, exactly
// as the effective-plan hash encodes operations today.
type OperationSet map[string]struct{}

// add folds a rule's operation slice into the set, normalising an empty slice and
// the explicit OperationAll to the "*" sentinel.
func (s OperationSet) add(ops []configv1alpha3.OperationType) {
	if len(ops) == 0 {
		s["*"] = struct{}{}
		return
	}
	for _, op := range ops {
		if op == configv1alpha3.OperationAll {
			s["*"] = struct{}{}
			continue
		}
		s[string(op)] = struct{}{}
	}
}

// Sorted returns the operations in a stable order, collapsing to ["*"] when the
// all-operations sentinel is present.
func (s OperationSet) Sorted() []string {
	if _, all := s["*"]; all {
		return []string{"*"}
	}
	out := make([]string, 0, len(s))
	for op := range s {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// WatchedType is one followable type a GitTarget watches: a (GVK, GVR, scope) triple
// plus the namespace scope and served-version metadata, projected straight from the
// type registry's followable set. The registry owns identity (GVK<->GVR is 1:1 there),
// followability, and the removal grace, so a WatchedType is a copy of a registry fact,
// never a re-decision.
type WatchedType struct {
	GVK           schema.GroupVersionKind
	GVR           schema.GroupVersionResource
	Namespaced    bool
	Scope         configv1alpha3.ResourceScope
	ServedVersion string
	Preferred     bool

	// NamespaceSelections maps each watched namespace to the rule clauses that select
	// this type there: an operation set plus that rule's write exclusions. The
	// empty-string key is a cluster-wide stream: a cluster-scoped resource, or a
	// namespaced resource a ClusterWatchRule follows across every namespace.
	//
	// Clauses are kept per-rule rather than merged, because an exclusion vetoes only
	// within its own rule — a merged view could not express "rule A excludes Flux, rule
	// B does not", which must admit Flux (rules are a logical OR).
	NamespaceSelections map[string]RuleSelections
}

// NamespaceOps is the per-namespace union of operation filters — what each watch must
// stream, independent of which rule admits a given event.
func (t WatchedType) NamespaceOps(namespace string) OperationSet {
	return t.NamespaceSelections[namespace].Ops()
}

// ClusterWide reports whether this type is gathered with a single cluster-wide
// stream, true for a cluster-scoped resource and for a namespaced resource a
// ClusterWatchRule follows across all namespaces.
func (t WatchedType) ClusterWide() bool {
	_, ok := t.NamespaceSelections[""]
	return ok
}

// SnapshotNamespaces returns the namespaces this type is gathered under for the
// streaming snapshot and informers: an empty slice means cluster-wide. A
// cluster-wide selection overrides any named namespaces, matching the historic
// gvrSnapshotEntry collapse.
func (t WatchedType) SnapshotNamespaces() []string {
	if t.ClusterWide() {
		return nil
	}
	out := make([]string, 0, len(t.NamespaceSelections))
	for ns := range t.NamespaceSelections {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// WatchedTypeTable is a GitTarget's resident, resolved-once set of watched types: the
// subset of the type registry's followable set its WatchRules and ClusterWatchRules
// select. It is re-resolved only on a deliberate trigger (a rule-set change or a
// catalog/registry generation bump) and read by the snapshot, informer, and plan-hash
// paths instead of each re-resolving inline.
type WatchedTypeTable struct {
	GitDest types.ResourceReference
	// Dest is the GitTarget's write destination fingerprint (provider/branch/path),
	// carried so the effective-plan hash can be derived from the table alone.
	Dest string
	// ClusterID is the source cluster this GitTarget mirrors FROM: the cluster its watches
	// open against and its types were resolved on. LocalClusterID is the cluster the
	// operator runs in.
	ClusterID  string
	Types      []WatchedType
	ResolvedAt uint64
}

// watchSelection is one followable registry record a rule selected for a GitTarget,
// with the namespace it was selected under ("" = cluster-wide stream), the rule's
// operation filters, and the rule's write exclusions.
type watchSelection struct {
	record    typeset.TypeRecord
	namespace string
	ops       []configv1alpha3.OperationType
	exclusion WriteExclusion
}

// watchedTypeAccum accumulates one followable record's namespace scope while folding a
// GitTarget's selections. Within a namespace, clauses are keyed by their exclusion
// fingerprint so two rules that decline the same writers fold their operations together,
// while rules that decline different writers stay distinct.
type watchedTypeAccum struct {
	record       typeset.TypeRecord
	namespaces   map[string]map[string]*RuleSelection
	nsOrder      []string
	clauseOrders map[string][]string
}

// buildWatchedTypeTable folds a GitTarget's selected followable records into its
// watched-type table. Identity and followability are already settled by the registry, so
// this is a pure fold with no catalog lookup and no conflict decision.
func buildWatchedTypeTable(
	gitDest types.ResourceReference,
	generation uint64,
	selections []watchSelection,
) WatchedTypeTable {
	byGVR := map[schema.GroupVersionResource]*watchedTypeAccum{}
	var gvrOrder []schema.GroupVersionResource
	for _, sel := range selections {
		gvr := sel.record.Identity.GVR
		acc := byGVR[gvr]
		if acc == nil {
			acc = &watchedTypeAccum{
				record:       sel.record,
				namespaces:   map[string]map[string]*RuleSelection{},
				clauseOrders: map[string][]string{},
			}
			byGVR[gvr] = acc
			gvrOrder = append(gvrOrder, gvr)
		}
		acc.add(sel)
	}

	table := WatchedTypeTable{GitDest: gitDest, ResolvedAt: generation}
	for _, gvr := range gvrOrder {
		acc := byGVR[gvr]
		table.Types = append(table.Types, watchedTypeFromRecord(acc.record, acc.namespaceSelections()))
	}
	sortWatchedTypes(table.Types)
	return table
}

// add folds one rule's selection into the accumulator, merging it with any earlier rule
// that declines exactly the same writers.
func (a *watchedTypeAccum) add(sel watchSelection) {
	clauses := a.namespaces[sel.namespace]
	if clauses == nil {
		clauses = map[string]*RuleSelection{}
		a.namespaces[sel.namespace] = clauses
		a.nsOrder = append(a.nsOrder, sel.namespace)
	}
	key := sel.exclusion.Key()
	clause := clauses[key]
	if clause == nil {
		clause = &RuleSelection{Ops: OperationSet{}, Exclusion: sel.exclusion}
		clauses[key] = clause
		a.clauseOrders[sel.namespace] = append(a.clauseOrders[sel.namespace], key)
	}
	clause.Ops.add(sel.ops)
}

// namespaceSelections materializes the accumulated clauses in a deterministic order, so
// the watch-spec fingerprint is stable across reconciles.
func (a *watchedTypeAccum) namespaceSelections() map[string]RuleSelections {
	out := make(map[string]RuleSelections, len(a.namespaces))
	for _, ns := range a.nsOrder {
		keys := append([]string(nil), a.clauseOrders[ns]...)
		sort.Strings(keys)
		selections := make(RuleSelections, 0, len(keys))
		for _, key := range keys {
			selections = append(selections, *a.namespaces[ns][key])
		}
		out[ns] = selections
	}
	return out
}

// watchedTypeFromRecord copies a followable registry record's identity into a
// WatchedType, attaching the per-namespace rule clauses the rules folded.
func watchedTypeFromRecord(rec typeset.TypeRecord, namespaceSelections map[string]RuleSelections) WatchedType {
	return WatchedType{
		GVK:                 rec.Identity.GVK,
		GVR:                 rec.Identity.GVR,
		Namespaced:          rec.Identity.Scope == typeset.ScopeNamespaced,
		Scope:               resourceScopeFor(rec.Identity.Scope),
		ServedVersion:       rec.Identity.GVR.Version,
		Preferred:           rec.Preferred,
		NamespaceSelections: namespaceSelections,
	}
}

// resourceScopeFor maps a typeset scope onto the API's ResourceScope. A followable
// record always carries a concrete scope, so Unknown never reaches the table.
func resourceScopeFor(scope typeset.Scope) configv1alpha3.ResourceScope {
	if scope == typeset.ScopeCluster {
		return configv1alpha3.ResourceScopeCluster
	}
	return configv1alpha3.ResourceScopeNamespaced
}

func sortWatchedTypes(watched []WatchedType) {
	sort.Slice(watched, func(i, j int) bool {
		return gvkSortKey(watched[i].GVK) < gvkSortKey(watched[j].GVK)
	})
}

// tableWatchesGVR reports whether a GitTarget's resident table includes the given type.
func tableWatchesGVR(table WatchedTypeTable, gvr schema.GroupVersionResource) bool {
	for _, wt := range table.Types {
		if wt.GVR == gvr {
			return true
		}
	}
	return false
}

// gvkSortKey builds a stable group|version|kind ordering key.
func gvkSortKey(gvk schema.GroupVersionKind) string {
	return gvk.Group + "|" + gvk.Version + "|" + gvk.Kind
}
