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
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// OperationSet is the set of operation filters recorded for a watched type in one
// namespace. The sentinel "*" means all operations and subsumes the rest, exactly
// as the effective-plan hash encodes operations today.
type OperationSet map[string]struct{}

// add folds a rule's operation slice into the set, normalising an empty slice and
// the explicit OperationAll to the "*" sentinel.
func (s OperationSet) add(ops []configv1alpha1.OperationType) {
	if len(ops) == 0 {
		s["*"] = struct{}{}
		return
	}
	for _, op := range ops {
		if op == configv1alpha1.OperationAll {
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

// WatchedType is one resolved type a GitTarget follows: a (GVK, GVR, scope) triple
// plus the namespace scope and served-version metadata. It is the unit the post-M8
// per-type reconcile (M12+) will iterate; in M10 it is the resolved-once, resident
// description of "what this GitTarget watches" that the snapshot, informer, and
// plan-hash paths all derive from.
//
// GitOps Reverser treats GVK and GVR as a 1:1 relationship: a type whose GVK does
// not resolve to exactly one served GVR is refused, not watched (see
// buildWatchedTypeTable), so a WatchedType always carries exactly one GVR for its
// GVK.
type WatchedType struct {
	GVK           schema.GroupVersionKind
	GVR           schema.GroupVersionResource
	Namespaced    bool
	Scope         configv1alpha1.ResourceScope
	ServedVersion string
	Preferred     bool

	// NamespaceOps maps each watched namespace to the union of operation filters
	// for this type in that namespace. The empty-string key is a cluster-wide
	// stream: a cluster-scoped resource, or a namespaced resource a ClusterWatchRule
	// follows across every namespace.
	NamespaceOps map[string]OperationSet
}

// ClusterWide reports whether this type is gathered with a single cluster-wide
// stream, true for a cluster-scoped resource and for a namespaced resource a
// ClusterWatchRule follows across all namespaces.
func (t WatchedType) ClusterWide() bool {
	_, ok := t.NamespaceOps[""]
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
	out := make([]string, 0, len(t.NamespaceOps))
	for ns := range t.NamespaceOps {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// TypeConflict is a GVK refused for violating the GVK<->GVR 1:1 assumption: the
// GitTarget's rules resolved it to more than one served resource (a pathological
// cluster serving one kind from several resources). The type is not watched; the
// operator must disambiguate. It is recorded so the refusal is observable rather
// than a silent gap in the mirror.
type TypeConflict struct {
	GVK  schema.GroupVersionKind
	GVRs []schema.GroupVersionResource
}

// WatchedTypeTable is a GitTarget's resident, resolved-once set of watched types.
// It replaces the per-gather re-resolution the snapshot, informer, and plan-hash
// paths each did inline; it is re-resolved only on a deliberate trigger (a rule-set
// change or a catalog generation bump). It is the spine the per-type reconcile,
// untracking sweep, and visibility surfaces (M11-M14) hang on.
type WatchedTypeTable struct {
	GitDest types.ResourceReference
	// Dest is the GitTarget's write destination fingerprint (provider/branch/path),
	// carried so the effective-plan hash can be derived from the table alone.
	Dest      string
	Types     []WatchedType
	Conflicts []TypeConflict
	// Misses are the rule resources that did not resolve to a watched type. The
	// blocking subset (catalog unavailable / discovery degraded) makes the snapshot
	// gather fail closed; the rest (not served / ambiguous / disallowed) are
	// reporting facts for visibility.
	Misses     []ResolveMiss
	ResolvedAt uint64
}

// BlockingMisses returns the misses that must make a snapshot gather fail closed
// rather than sweep on a partial view: an unavailable catalog or degraded discovery
// is an unobservable surface, never a trusted absence.
func (t WatchedTypeTable) BlockingMisses() []ResolveMiss {
	return blockingSnapshotMisses(t.Misses)
}

// resolvedSelection is one (resolved GVR, namespace, operations) tuple a rule
// contributed to a GitTarget, before types are folded by GVK. The namespace is ""
// for a cluster-wide stream.
type resolvedSelection struct {
	gvr       GVR
	namespace string
	ops       []configv1alpha1.OperationType
}

// gvrAccum accumulates one GVR's namespace/operation scope while folding a
// GitTarget's resolved selections.
type gvrAccum struct {
	gvr          GVR
	namespaceOps map[string]OperationSet
}

// buildWatchedTypeTable folds a GitTarget's resolved selections into its watched-type
// table, enforcing the GVK<->GVR 1:1 assumption against trusted catalog data.
//
// It runs in three steps: fold selections into per-GVR namespace/operation scope;
// map each GVR to its served GVK (GVR->GVK is single-valued in the catalog); group
// GVRs by GVK and refuse any GVK claimed by more than one GVR as a TypeConflict.
// A selection whose GVR no longer resolves against the catalog is skipped — the
// resolver only emits served GVRs, so this only happens if discovery changed under
// us, and the missing type is simply not watched rather than guessed.
func buildWatchedTypeTable(
	gitDest types.ResourceReference,
	generation uint64,
	selections []resolvedSelection,
	catalog *APIResourceCatalog,
) WatchedTypeTable {
	byGVR := foldSelectionsByGVR(selections)

	byGVK := map[schema.GroupVersionKind][]*gvrAccum{}
	gvkEntry := map[schema.GroupVersionKind]APIResourceEntry{}
	for key, acc := range byGVR {
		entry, ok := lookupServedEntry(catalog, key)
		if !ok {
			continue
		}
		byGVK[entry.GVK] = append(byGVK[entry.GVK], acc)
		gvkEntry[entry.GVK] = entry
	}

	table := WatchedTypeTable{GitDest: gitDest, ResolvedAt: generation}
	for gvk, accs := range byGVK {
		if len(accs) > 1 {
			table.Conflicts = append(table.Conflicts, typeConflict(gvk, accs))
			continue
		}
		acc := accs[0]
		entry := gvkEntry[gvk]
		table.Types = append(table.Types, WatchedType{
			GVK:           gvk,
			GVR:           acc.gvr.schema(),
			Namespaced:    entry.Namespaced,
			Scope:         acc.gvr.Scope,
			ServedVersion: acc.gvr.Version,
			Preferred:     entry.Preferred,
			NamespaceOps:  acc.namespaceOps,
		})
	}
	sortWatchedTypes(table.Types)
	sortTypeConflicts(table.Conflicts)
	return table
}

// foldSelectionsByGVR groups selections by GVR, unioning each GVR's per-namespace
// operation sets.
func foldSelectionsByGVR(selections []resolvedSelection) map[schema.GroupVersionResource]*gvrAccum {
	byGVR := map[schema.GroupVersionResource]*gvrAccum{}
	for _, sel := range selections {
		key := sel.gvr.schema()
		acc := byGVR[key]
		if acc == nil {
			acc = &gvrAccum{gvr: sel.gvr, namespaceOps: map[string]OperationSet{}}
			byGVR[key] = acc
		}
		opSet := acc.namespaceOps[sel.namespace]
		if opSet == nil {
			opSet = OperationSet{}
			acc.namespaceOps[sel.namespace] = opSet
		}
		opSet.add(sel.ops)
	}
	return byGVR
}

// lookupServedEntry returns the single served catalog entry for a GVR, if present.
// GVR->GVK is single-valued in the catalog, so the lookup yields zero or one entry.
func lookupServedEntry(
	catalog *APIResourceCatalog,
	gvr schema.GroupVersionResource,
) (APIResourceEntry, bool) {
	if catalog == nil {
		return APIResourceEntry{}, false
	}
	lookup := catalog.LookupGVR(gvr)
	if len(lookup.Entries) == 0 {
		return APIResourceEntry{}, false
	}
	return lookup.Entries[0], true
}

// typeConflict renders the refused GVRs of a conflicting GVK in a stable order.
func typeConflict(gvk schema.GroupVersionKind, accs []*gvrAccum) TypeConflict {
	gvrs := make([]schema.GroupVersionResource, 0, len(accs))
	for _, acc := range accs {
		gvrs = append(gvrs, acc.gvr.schema())
	}
	sort.Slice(gvrs, func(i, j int) bool { return gvrs[i].String() < gvrs[j].String() })
	return TypeConflict{GVK: gvk, GVRs: gvrs}
}

func sortWatchedTypes(watched []WatchedType) {
	sort.Slice(watched, func(i, j int) bool {
		return gvkSortKey(watched[i].GVK) < gvkSortKey(watched[j].GVK)
	})
}

func sortTypeConflicts(conflicts []TypeConflict) {
	sort.Slice(conflicts, func(i, j int) bool {
		return gvkSortKey(conflicts[i].GVK) < gvkSortKey(conflicts[j].GVK)
	})
}

// gvkSortKey builds a stable group|version|kind ordering key.
func gvkSortKey(gvk schema.GroupVersionKind) string {
	return gvk.Group + "|" + gvk.Version + "|" + gvk.Kind
}
