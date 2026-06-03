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

package manifestedit

import (
	"encoding/json"
	"reflect"
	"sort"

	"gopkg.in/yaml.v3"
)

// mergeMapping merges a desired map onto an existing mapping node. Existing key
// order is preserved, keys absent from desired are deleted, and desired-only keys
// are appended in sorted order. It returns whether anything changed, and whether
// the merge stayed unambiguous (false asks the caller to fall back).
func mergeMapping(node *yaml.Node, desired map[string]interface{}) (bool, bool) {
	changed := false
	rebuilt := make([]*yaml.Node, 0, len(node.Content))
	present := make(map[string]bool, len(desired))

	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]

		desiredVal, want := desired[keyNode.Value]
		if !want {
			changed = true // field present in Git, absent from desired: delete it
			continue
		}
		present[keyNode.Value] = true

		c, sub := mergeValue(valNode, desiredVal)
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
func mergeValue(node *yaml.Node, desired interface{}) (bool, bool) {
	switch d := desired.(type) {
	case map[string]interface{}:
		if node.Kind == yaml.MappingNode {
			return mergeMapping(node, d)
		}
		return replaceNode(node, desired), true
	case []interface{}:
		if node.Kind == yaml.SequenceNode {
			return mergeSequence(node, d)
		}
		return replaceNode(node, desired), true
	default:
		if nodeEqualsValue(node, desired) {
			return false, true // unchanged: leave the node (and its style) untouched
		}
		return replaceNode(node, desired), true
	}
}

// mergeSequence merges a desired slice onto an existing sequence node by index.
// Field-keyed matching (for example by container name) is a later nicety.
func mergeSequence(node *yaml.Node, desired []interface{}) (bool, bool) {
	changed := false
	if len(desired) < len(node.Content) {
		node.Content = node.Content[:len(desired)]
		changed = true
	}
	for i := range node.Content {
		c, sub := mergeValue(node.Content[i], desired[i])
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
