// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"errors"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/yamlstyle"
)

// MarshalToOrderedYAML converts an unstructured object to YAML with guaranteed field order.
// Field order: apiVersion, kind, metadata, then payload (spec, data, rules, etc.)
//
// It encodes through [yamlstyle], which is the same encoder that re-serializes a document
// edited in place. That is not an implementation detail: this function produces the bytes a
// CREATE commits, manifestedit produces the bytes an UPDATE commits, and when the two
// styles differ the first update after a create rewrites every sequence line in the file to
// carry one changed field. TestCreateAndUpdateAgreeByteForByte pins them together.
func MarshalToOrderedYAML(obj *unstructured.Unstructured) ([]byte, error) {
	if obj == nil {
		return nil, errors.New("object is nil")
	}

	// One mapping node, built in the order we want the keys emitted, encoded once. The
	// previous shape — four independent Marshal calls concatenated into a buffer — is why
	// the style was easy to diverge in the first place: nothing about it was one encoder.
	root := &yaml.Node{Kind: yaml.MappingNode}

	var metadata PartialObjectMeta
	metadata.FromUnstructured(obj)

	if err := appendField(root, "apiVersion", obj.GetAPIVersion()); err != nil {
		return nil, err
	}
	if err := appendField(root, "kind", obj.GetKind()); err != nil {
		return nil, err
	}
	if err := appendField(root, "metadata", buildMetadataMap(metadata)); err != nil {
		return nil, err
	}

	// Payload: everything except apiVersion, kind, metadata, status, keys sorted so the
	// same object always renders the same way.
	payload := extractPayload(obj)
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := appendField(root, k, payload[k]); err != nil {
			return nil, err
		}
	}

	return yamlstyle.Encode(root)
}

// appendField adds one key/value pair to a mapping node, encoding the value with the same
// encoder that will write it.
func appendField(root *yaml.Node, key string, value any) error {
	valueNode, err := yamlstyle.NodeFor(value)
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %w", key, err)
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		valueNode,
	)
	return nil
}

// buildMetadataMap constructs the metadata map with only non-empty fields.
func buildMetadataMap(md PartialObjectMeta) map[string]interface{} {
	out := make(map[string]interface{})
	if md.Name != "" {
		out["name"] = md.Name
	}
	if md.Namespace != "" {
		out["namespace"] = md.Namespace
	}
	if len(md.Labels) > 0 {
		out["labels"] = md.Labels
	}
	if len(md.Annotations) > 0 {
		out["annotations"] = md.Annotations
	}
	return out
}

// extractPayload returns the subset of top-level fields to be included in the payload.
func extractPayload(obj *unstructured.Unstructured) map[string]interface{} {
	payload := make(map[string]interface{})
	for k, v := range obj.Object {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
			// skip
		default:
			payload[k] = v
		}
	}
	return payload
}
