// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"encoding/json"
	"reflect"
	"sort"

	"gopkg.in/yaml.v3"
)

// mergeCtx carries the injected merge strategies down the node walk: the
// field-ownership predicate and the list-match strategy. It is the seam where
// keyed-list matching and declared-subset ownership plug in; the defaults
// reproduce today's whole-object, index-based behavior.
type mergeCtx struct {
	// owns reports whether a field path is owned by the reverser. Nil means
	// "own everything", today's whole-object truth.
	owns func(path FieldPath) bool
	// list aligns sequence items. The zero value matches by index.
	list ListMatchStrategy
}

// ownsPath reports whether the reverser owns a field path. An unowned path is
// left in Git even when absent from desired, so a field present in Git but
// absent from desired is deleted only when owned.
func (m mergeCtx) ownsPath(path FieldPath) bool {
	if m.owns == nil {
		return true
	}
	return m.owns(path)
}

// mergeMapping merges a desired map onto an existing mapping node. Existing key
// order is preserved, owned keys absent from desired are deleted, and desired-only
// keys are appended in sorted order. It returns whether anything changed, and
// whether the merge stayed unambiguous (false asks the caller to fall back).
func mergeMapping(ctx mergeCtx, path FieldPath, node *yaml.Node, desired map[string]interface{}) (bool, bool) {
	changed := false
	rebuilt := make([]*yaml.Node, 0, len(node.Content))
	present := make(map[string]bool, len(desired))

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]

		childPath := make(FieldPath, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = keyNode.Value
		desiredVal, want := desired[keyNode.Value]
		if !want {
			if ctx.ownsPath(childPath) {
				changed = true // owned field present in Git, absent from desired: delete it
				continue
			}
			rebuilt = append(rebuilt, keyNode, valNode) // unowned: leave it in Git
			continue
		}
		present[keyNode.Value] = true

		c, sub := mergeValue(ctx, childPath, valNode, desiredVal)
		if !sub {
			return changed, false
		}
		changed = changed || c
		rebuilt = append(rebuilt, keyNode, valNode)
	}

	extra := make([]string, 0)
	for k := range desired {
		if !present[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		valNode, err := encodeValue(desired[k])
		if err != nil {
			return changed, false
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
		rebuilt = append(rebuilt, keyNode, valNode)
		changed = true
	}

	node.Content = rebuilt
	return changed, true
}

// mergeValue merges a desired value onto an existing node, recursing for maps and
// sequences and replacing scalars only when their value actually differs.
func mergeValue(ctx mergeCtx, path FieldPath, node *yaml.Node, desired interface{}) (bool, bool) {
	switch d := desired.(type) {
	case map[string]interface{}:
		if node.Kind == yaml.MappingNode {
			return mergeMapping(ctx, path, node, d)
		}
		changed := replaceNode(node, desired)
		return changed, changed
	case []interface{}:
		if node.Kind == yaml.SequenceNode {
			return mergeSequence(ctx, path, node, d)
		}
		changed := replaceNode(node, desired)
		return changed, changed
	default:
		if nodeEqualsValue(node, desired) {
			return false, true // unchanged: leave the node (and its style) untouched
		}
		// replaceNode returns false only when the value cannot be encoded. Report
		// that as a failed sub-merge (ok=false) so the caller falls back to a whole-
		// document replace instead of silently dropping the edit.
		changed := replaceNode(node, desired)
		return changed, changed
	}
}

// mergeSequence merges a desired slice onto an existing sequence node. With a
// keyed list-match strategy it matches items by their key field, so an item is
// compared to its counterpart (carrying its comments and style) rather than to
// whatever happens to share its slot. When the strategy does not apply — no key
// configured, or the items are not uniformly keyed mappings — it falls back to
// index-based matching.
func mergeSequence(ctx mergeCtx, path FieldPath, node *yaml.Node, desired []interface{}) (bool, bool) {
	if ctx.list.KeyField != "" {
		if changed, ok, applied := mergeSequenceKeyed(ctx, path, node, desired); applied {
			return changed, ok
		}
	}
	return mergeSequenceByIndex(ctx, path, node, desired)
}

// mergeSequenceByIndex merges a desired slice onto an existing sequence node by
// index: slot i in Git is compared to slot i in desired. A reorder therefore
// rewrites slots (a recorded limitation keyed matching fixes).
func mergeSequenceByIndex(ctx mergeCtx, path FieldPath, node *yaml.Node, desired []interface{}) (bool, bool) {
	changed := false
	if len(desired) < len(node.Content) {
		node.Content = node.Content[:len(desired)]
		changed = true
	}
	for i := range node.Content {
		c, sub := mergeValue(ctx, path, node.Content[i], desired[i])
		if !sub {
			return changed, false
		}
		changed = changed || c
	}
	for i := len(node.Content); i < len(desired); i++ {
		valNode, err := encodeValue(desired[i])
		if err != nil {
			return changed, false
		}
		node.Content = append(node.Content, valNode)
		changed = true
	}
	return changed, true
}

// mergeSequenceKeyed merges a desired slice onto an existing sequence node by a
// key field instead of by index. The result is rebuilt in desired order, each
// matched item carrying its existing node (so comments and style travel with the
// item, not the slot); desired-only items are encoded fresh, and Git items absent
// from desired are dropped.
//
// The returns are (changed, ok, applied). applied is false when keyed matching
// cannot be applied cleanly — items are not all mappings carrying a non-empty,
// unique key — so the caller falls back to index matching. ok is false (with
// applied true) when a matched item's sub-merge is ambiguous, so the caller falls
// back to a whole-document replace, exactly as with index matching.
func mergeSequenceKeyed(ctx mergeCtx, path FieldPath, node *yaml.Node, desired []interface{}) (bool, bool, bool) {
	key := ctx.list.KeyField

	existingByKey, indexable := indexNodesByKey(node.Content, key)
	if !indexable {
		return false, false, false
	}
	desiredMaps, desiredKeys, keyable := keyedDesiredItems(desired, key)
	if !keyable {
		return false, false, false
	}

	changed := false
	rebuilt := make([]*yaml.Node, 0, len(desired))
	for i, dm := range desiredMaps {
		existing, found := existingByKey[desiredKeys[i]]
		if !found {
			fresh, err := encodeValue(dm)
			if err != nil {
				return changed, false, true
			}
			rebuilt = append(rebuilt, fresh)
			continue
		}
		itemPath := make(FieldPath, len(path)+1)
		copy(itemPath, path)
		itemPath[len(path)] = desiredKeys[i]
		c, sub := mergeMapping(ctx, itemPath, existing, dm)
		if !sub {
			return changed, false, true
		}
		changed = changed || c
		rebuilt = append(rebuilt, existing)
	}

	changed = changed || sequenceReordered(node.Content, rebuilt)
	node.Content = rebuilt
	return changed, true, true
}

// indexNodesByKey maps each item node to its key value, requiring every item to
// be a mapping with a non-empty, unique value for key. indexable is false
// otherwise, so the caller falls back to index matching.
func indexNodesByKey(items []*yaml.Node, key string) (map[string]*yaml.Node, bool) {
	byKey := make(map[string]*yaml.Node, len(items))
	for _, item := range items {
		if item.Kind != yaml.MappingNode {
			return nil, false
		}
		kv := scalarOf(nodeMapGet(item, key))
		if kv == "" {
			return nil, false
		}
		if _, dup := byKey[kv]; dup {
			return nil, false
		}
		byKey[kv] = item
	}
	return byKey, true
}

// keyedDesiredItems extracts the desired items as maps with their key values,
// requiring every item to be a map with a non-empty, unique string key. keyable
// is false otherwise.
func keyedDesiredItems(desired []interface{}, key string) ([]map[string]interface{}, []string, bool) {
	maps := make([]map[string]interface{}, 0, len(desired))
	keys := make([]string, 0, len(desired))
	seen := make(map[string]bool, len(desired))
	for _, d := range desired {
		dm, isMap := d.(map[string]interface{})
		if !isMap {
			return nil, nil, false
		}
		kv, isString := dm[key].(string)
		if !isString || kv == "" || seen[kv] {
			return nil, nil, false
		}
		seen[kv] = true
		maps = append(maps, dm)
		keys = append(keys, kv)
	}
	return maps, keys, true
}

// sequenceReordered reports whether rebuilt differs from original in length or in
// the identity/order of its nodes, which (together with any sub-merge change)
// tells keyed matching whether the sequence actually changed.
func sequenceReordered(original, rebuilt []*yaml.Node) bool {
	if len(original) != len(rebuilt) {
		return true
	}
	for i := range rebuilt {
		if rebuilt[i] != original[i] {
			return true
		}
	}
	return false
}

// replaceNode overwrites a node with a freshly encoded value, keeping the old
// node's comments and, for strings, its quoting/block style when sensible.
func replaceNode(node *yaml.Node, desired interface{}) bool {
	fresh, err := encodeValue(desired)
	if err != nil {
		return false
	}
	if node.Kind == yaml.ScalarNode && fresh.Kind == yaml.ScalarNode {
		if _, isString := desired.(string); isString && isPreservableStringStyle(node.Style) {
			fresh.Style = node.Style
		}
	}
	fresh.HeadComment = node.HeadComment
	fresh.LineComment = node.LineComment
	fresh.FootComment = node.FootComment
	*node = *fresh
	return true
}

// isPreservableStringStyle reports whether a scalar style is worth carrying over
// to a changed string value (quoting and block styles, but not plain or tagged).
func isPreservableStringStyle(s yaml.Style) bool {
	return s == yaml.SingleQuotedStyle || s == yaml.DoubleQuotedStyle ||
		s == yaml.LiteralStyle || s == yaml.FoldedStyle
}

// nodeEqualsValue reports whether a node already represents the desired scalar
// value, comparing through a JSON round-trip so int/float typing does not matter.
func nodeEqualsValue(node *yaml.Node, desired interface{}) bool {
	var got interface{}
	if err := node.Decode(&got); err != nil {
		return false
	}
	return reflect.DeepEqual(normalizeJSON(got), normalizeJSON(desired))
}

// encodeValue builds a fresh node tree representing a Go value.
func encodeValue(v interface{}) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// normalizeJSON round-trips a value through JSON so numeric and structural types
// from different decoders compare equal.
func normalizeJSON(v interface{}) interface{} {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}
