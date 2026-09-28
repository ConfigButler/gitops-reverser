// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ReasonObjectSelectorConflict is the ResourcesResolved reason for a rule refused because one of
// its collections overlaps a collection the same GitTarget already selects with a different
// object selector.
const ReasonObjectSelectorConflict = "ObjectSelectorConflict"

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

// selectedCollection is one collection a selection asks for, as the overlap check compares it:
// the served version is not part of it, for the reason it is not part of types.CollectionKey.
type selectedCollection struct {
	groupResource schema.GroupResource
	namespace     string
	labelSelector string
}

// overlaps reports whether two collections can hold the same object with different selections:
// the same type, and the same namespace or one of them all namespaces.
func (c selectedCollection) overlaps(other selectedCollection) bool {
	if c.groupResource != other.groupResource {
		return false
	}
	return c.namespace == other.namespace || c.namespace == "" || other.namespace == ""
}

func (c selectedCollection) String() string {
	name := c.groupResource.String()
	if c.namespace == "" {
		name += " in all namespaces"
	} else {
		name += " in " + c.namespace
	}
	if c.labelSelector == "" {
		return name + " with no objectSelector"
	}
	return fmt.Sprintf("%s with objectSelector %q", name, c.labelSelector)
}

// refuseSelectorConflicts drops the selections of every rule whose collections overlap a
// collection another rule of the same GitTarget selects with a different object selector, and
// returns the refused rules with the reason for each.
//
// Overlapping collections must share one selector. Two selections of one object would be two
// watches that disagree about whether it is in the mirror: one would delete a document the other
// still selects, and their snapshots would prune each other's documents on every replay.
// Different selectors in disjoint namespaces are fine.
//
// Rules are admitted oldest first, so the rule that got there first keeps its collections and a
// newer rule cannot take a running mirror away by asking for a different selector. A rule whose
// own items conflict with each other is refused whatever its age. A refusal is whole-rule, as a
// denied source namespace is: mirroring part of what a rule asked for is worse than a loud failure.
func refuseSelectorConflicts(selections []watchSelection) ([]watchSelection, map[selectingRule]string) {
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
		if reason := selectorConflict(own, held); reason != "" {
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
	collection selectedCollection
	rule       selectingRule
}

// selectorConflict returns why a rule's collections cannot be admitted beside the ones already
// held, or "" when they can.
func selectorConflict(own []selectedCollection, held []heldCollection) string {
	for i, c := range own {
		for _, other := range own[i+1:] {
			if c.overlaps(other) && c.labelSelector != other.labelSelector {
				return fmt.Sprintf("its items select overlapping collections differently: %s, and %s; "+
					"overlapping collections must use one objectSelector", c, other)
			}
		}
		for _, h := range held {
			if c.overlaps(h.collection) && c.labelSelector != h.collection.labelSelector {
				return fmt.Sprintf("it selects %s, which overlaps %s already selected by the older %s; "+
					"overlapping collections on one GitTarget must use one objectSelector", c, h.collection, h.rule)
			}
		}
	}
	return ""
}

// collectionsOf returns the distinct collections a rule's selections ask for, in a stable order so
// a refusal message names the same pair on every resolution.
func collectionsOf(selections []watchSelection) []selectedCollection {
	seen := map[selectedCollection]struct{}{}
	var out []selectedCollection
	for _, sel := range selections {
		c := selectedCollection{
			groupResource: sel.record.Identity.GVR.GroupResource(),
			namespace:     sel.namespace,
			labelSelector: sel.labelSelector,
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.groupResource != b.groupResource {
			return a.groupResource.String() < b.groupResource.String()
		}
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		return a.labelSelector < b.labelSelector
	})
	return out
}
