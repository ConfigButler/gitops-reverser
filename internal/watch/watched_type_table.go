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

	// NamespaceOps maps each watched namespace to the union of operation filters
	// for this type in that namespace. The empty-string key is a cluster-wide
	// stream: a cluster-scoped resource, or a namespaced resource a ClusterWatchRule
	// follows across every namespace.
	NamespaceOps map[string]OperationSet
}

// ClusterWide reports whether this type is gathered under a cluster-wide scope: true for a
// cluster-scoped resource and for a namespaced resource a ClusterWatchRule follows across all
// namespaces. It reports the presence of that scope, NOT that it is the only one — a
// namespaced type may carry the cluster-wide scope alongside named ones, and each is streamed
// in its own right. Use WatchScopes to enumerate them.
func (t WatchedType) ClusterWide() bool {
	_, ok := t.NamespaceOps[""]
	return ok
}

// WatchScopes returns the distinct namespace scopes this type is gathered under — one
// per stream — in a stable order. The empty string is the cluster-wide scope (a
// cluster-scoped resource, or a namespaced resource a ClusterWatchRule follows across
// every namespace) and sorts first.
//
// A cluster-wide selection does NOT suppress co-resident named namespaces. A WatchRule
// scoped to one namespace and a ClusterWatchRule scoped cluster-wide, on the same GVR and
// the same GitTarget, stay two scopes here, each keeping its own operation filters. This
// previously collapsed to a single cluster-wide scope, which silently widened the named
// rule's stream to every namespace the credential could read and discarded its operation
// set — a gate bypass once a WatchRule declares the source namespaces it is authorized
// for. See docs/design/watchrule-source-namespace/pr2-stream-scope-collapse.md.
//
// Every read site must project the same scope set, because a gather's scope becomes the
// mark-and-sweep's scope: a gather wider than the stream that triggered it deletes
// managed documents that were never in scope (see git.ResyncScope).
func (t WatchedType) WatchScopes() []string {
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
	ops       []configv1alpha3.OperationType
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
