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

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// OperationSet is the set of operation filters recorded for a watched type in one
// namespace. The sentinel "*" means all operations and subsumes the rest, exactly
// as the effective-plan hash encodes operations today.
type OperationSet map[string]struct{}

// add folds a rule's operation slice into the set, normalising an empty slice and
// the explicit OperationAll to the "*" sentinel.
func (s OperationSet) add(ops []configv1alpha2.OperationType) {
	if len(ops) == 0 {
		s["*"] = struct{}{}
		return
	}
	for _, op := range ops {
		if op == configv1alpha2.OperationAll {
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
	Scope         configv1alpha2.ResourceScope
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

// WatchedTypeTable is a GitTarget's resident, resolved-once set of watched types: the
// subset of the type registry's followable set its WatchRules and ClusterWatchRules
// select. It is re-resolved only on a deliberate trigger (a rule-set change or a
// catalog/registry generation bump) and read by the snapshot, informer, and plan-hash
// paths instead of each re-resolving inline.
type WatchedTypeTable struct {
	GitDest types.ResourceReference
	// Dest is the GitTarget's write destination fingerprint (provider/branch/path),
	// carried so the effective-plan hash can be derived from the table alone.
	Dest       string
	Types      []WatchedType
	ResolvedAt uint64
}

// watchSelection is one followable registry record a rule selected for a GitTarget,
// with the namespace it was selected under ("" = cluster-wide stream) and the rule's
// operation filters.
type watchSelection struct {
	record    typeset.TypeRecord
	namespace string
	ops       []configv1alpha2.OperationType
}

// watchedTypeAccum accumulates one followable record's namespace/operation scope while
// folding a GitTarget's selections.
type watchedTypeAccum struct {
	record       typeset.TypeRecord
	namespaceOps map[string]OperationSet
}

// buildWatchedTypeTable folds a GitTarget's selected followable records into its
// watched-type table, unioning each record's per-namespace operation filters. Identity
// and followability are already settled by the registry, so this is a pure fold with no
// catalog lookup and no conflict decision.
func buildWatchedTypeTable(
	gitDest types.ResourceReference,
	generation uint64,
	selections []watchSelection,
) WatchedTypeTable {
	byGVR := map[schema.GroupVersionResource]*watchedTypeAccum{}
	for _, sel := range selections {
		gvr := sel.record.Identity.GVR
		acc := byGVR[gvr]
		if acc == nil {
			acc = &watchedTypeAccum{record: sel.record, namespaceOps: map[string]OperationSet{}}
			byGVR[gvr] = acc
		}
		opSet := acc.namespaceOps[sel.namespace]
		if opSet == nil {
			opSet = OperationSet{}
			acc.namespaceOps[sel.namespace] = opSet
		}
		opSet.add(sel.ops)
	}

	table := WatchedTypeTable{GitDest: gitDest, ResolvedAt: generation}
	for _, acc := range byGVR {
		table.Types = append(table.Types, watchedTypeFromRecord(acc.record, acc.namespaceOps))
	}
	sortWatchedTypes(table.Types)
	return table
}

// watchedTypeFromRecord copies a followable registry record's identity into a
// WatchedType, attaching the per-namespace operation scope the rules folded.
func watchedTypeFromRecord(rec typeset.TypeRecord, namespaceOps map[string]OperationSet) WatchedType {
	return WatchedType{
		GVK:           rec.Identity.GVK,
		GVR:           rec.Identity.GVR,
		Namespaced:    rec.Identity.Scope == typeset.ScopeNamespaced,
		Scope:         resourceScopeFor(rec.Identity.Scope),
		ServedVersion: rec.Identity.GVR.Version,
		Preferred:     rec.Preferred,
		NamespaceOps:  namespaceOps,
	}
}

// resourceScopeFor maps a typeset scope onto the API's ResourceScope. A followable
// record always carries a concrete scope, so Unknown never reaches the table.
func resourceScopeFor(scope typeset.Scope) configv1alpha2.ResourceScope {
	if scope == typeset.ScopeCluster {
		return configv1alpha2.ResourceScopeCluster
	}
	return configv1alpha2.ResourceScopeNamespaced
}

func sortWatchedTypes(watched []WatchedType) {
	sort.Slice(watched, func(i, j int) bool {
		return gvkSortKey(watched[i].GVK) < gvkSortKey(watched[j].GVK)
	})
}

// gvkSortKey builds a stable group|version|kind ordering key.
func gvkSortKey(gvk schema.GroupVersionKind) string {
	return gvk.Group + "|" + gvk.Version + "|" + gvk.Kind
}
