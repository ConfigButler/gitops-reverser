// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReasonCollectionOverlap is the ResourcesResolved reason for a rule refused because one of its
// collections overlaps a different collection the same GitTarget already selects.
const ReasonCollectionOverlap = "CollectionOverlap"

// Rule kinds, as a selectingRule names them in a refusal message.
const (
	ruleKindWatchRule        = "WatchRule"
	ruleKindClusterWatchRule = "ClusterWatchRule"
)

// selectingRule identifies the rule a selection came from, with what decides which of two
// conflicting rules keeps its collections: the older creation timestamp, then the lower name.
type selectingRule struct {
	kind      string
	namespace string
	name      string
	createdAt metav1.Time
}

// String renders the rule for a refusal message: "WatchRule team-a/secrets".
func (r selectingRule) String() string {
	if r.namespace == "" {
		return r.kind + " " + r.name
	}
	return r.kind + " " + r.namespace + "/" + r.name
}

// olderThan orders rules oldest first. Kind and namespace are compared after the name only to
// make the order total; a WatchRule and a ClusterWatchRule never select one type, since one is
// namespaced-only and the other cluster-only.
func (r selectingRule) olderThan(other selectingRule) bool {
	if !r.createdAt.Equal(&other.createdAt) {
		return r.createdAt.Before(&other.createdAt)
	}
	if r.name != other.name {
		return r.name < other.name
	}
	if r.namespace != other.namespace {
		return r.namespace < other.namespace
	}
	return r.kind < other.kind
}

// describeSelection renders one collection for a refusal message.
func describeSelection(c types.CollectionKey) string {
	name := schema.GroupResource{Group: c.Group, Resource: c.Resource}.String()
	if c.Namespace == "" {
		name += " in all namespaces"
	} else {
		name += " in " + c.Namespace
	}
	if c.LabelSelector == "" {
		return name + " with no objectSelector"
	}
	return fmt.Sprintf("%s with objectSelector %q", name, c.LabelSelector)
}

// refuseCollectionOverlaps drops the selections of every rule whose collections overlap a
// different collection of the same GitTarget, and returns the refused rules with the reason for
// each.
//
// One GitTarget watches each object through at most one stream. Two streams over one object would
// deliver it twice, in no defined order: a delayed copy can restore content the other stream has
// already moved past, or split another author's commit window, and their snapshots would prune
// each other's documents. So overlapping collections are refused structurally — same type, and the
// same namespace or one of them all namespaces — whatever their object selectors, because a
// snapshot owns its whole structural scope. An exact duplicate is one collection, so one stream,
// and is not an overlap. Disjoint namespaces never overlap, and another GitTarget has its own
// streams.
//
// Rules are admitted oldest first, so the rule that got there first keeps its collections and a
// newer rule cannot take a running mirror away. A rule whose own items overlap each other is
// refused whatever its age. A refusal is whole-rule, as a denied source namespace is: mirroring
// part of what a rule asked for is worse than a loud failure.
func refuseCollectionOverlaps(selections []watchSelection) ([]watchSelection, map[selectingRule]string) {
	byRule := map[selectingRule][]watchSelection{}
	var rules []selectingRule
	for _, sel := range selections {
		if _, seen := byRule[sel.rule]; !seen {
			rules = append(rules, sel.rule)
		}
		byRule[sel.rule] = append(byRule[sel.rule], sel)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].olderThan(rules[j]) })

	var held []heldCollection
	var admitted []watchSelection
	var refused map[selectingRule]string
	for _, rule := range rules {
		own := collectionsOf(byRule[rule])
		if reason := collectionOverlap(own, held); reason != "" {
			if refused == nil {
				refused = map[selectingRule]string{}
			}
			refused[rule] = reason
			continue
		}
		for _, c := range own {
			held = append(held, heldCollection{collection: c, rule: rule})
		}
		admitted = append(admitted, byRule[rule]...)
	}
	return admitted, refused
}

// heldCollection is a collection an admitted rule keeps, with that rule for a refusal to name.
type heldCollection struct {
	collection types.CollectionKey
	rule       selectingRule
}

// overlapsDistinct reports whether two collections can deliver one object on two streams.
func overlapsDistinct(a, b types.CollectionKey) bool {
	return a != b && a.Overlaps(b)
}

// collectionOverlap returns why a rule's collections cannot be admitted beside the ones already
// held, or "" when they can.
func collectionOverlap(own []types.CollectionKey, held []heldCollection) string {
	for i, c := range own {
		for _, other := range own[i+1:] {
			if overlapsDistinct(c, other) {
				return fmt.Sprintf("its items select overlapping collections: %s, and %s; "+
					"a GitTarget watches each object through one collection",
					describeSelection(c), describeSelection(other))
			}
		}
		for _, h := range held {
			if overlapsDistinct(c, h.collection) {
				return fmt.Sprintf("it selects %s, which overlaps %s already selected by the older %s; "+
					"a GitTarget watches each object through one collection",
					describeSelection(c), describeSelection(h.collection), h.rule)
			}
		}
	}
	return ""
}

// collectionsOf returns the distinct collections a rule's selections ask for, in a stable order so
// a refusal message names the same pair on every resolution.
func collectionsOf(selections []watchSelection) []types.CollectionKey {
	seen := map[types.CollectionKey]struct{}{}
	var out []types.CollectionKey
	for _, sel := range selections {
		c := types.CollectionKeyFor(sel.record.Identity.GVR, sel.namespace)
		c.LabelSelector = sel.labelSelector
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sortCollections(out)
	return out
}
