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

package sanitize

import (
	"bytes"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// MarshalToOrderedYAML converts an unstructured object to YAML with guaranteed field order.
// Field order: apiVersion, kind, metadata, then payload (spec, data, rules, etc.)
func MarshalToOrderedYAML(obj *unstructured.Unstructured) ([]byte, error) {
	var buf bytes.Buffer

	// Header: apiVersion, kind, metadata
	if err := marshalHeader(&buf, obj); err != nil {
		return nil, err
	}

	// Payload: everything except apiVersion, kind, metadata, status
	payload := extractPayload(obj)
	if err := marshalPayload(&buf, payload); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// marshalHeader writes apiVersion, kind, and metadata in order.
func marshalHeader(buf *bytes.Buffer, obj *unstructured.Unstructured) error {
	if err := writeYAMLMap(buf, map[string]interface{}{"apiVersion": obj.GetAPIVersion()}); err != nil {
		return fmt.Errorf("failed to marshal apiVersion: %w", err)
	}
	if err := writeYAMLMap(buf, map[string]interface{}{"kind": obj.GetKind()}); err != nil {
		return fmt.Errorf("failed to marshal kind: %w", err)
	}

	var metadata PartialObjectMeta
	metadata.FromUnstructured(obj)
	metadataMap := buildMetadataMap(metadata)

	if err := writeYAMLMap(buf, map[string]interface{}{"metadata": metadataMap}); err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	return nil
}

// marshalPayload writes the payload map if present, with keys sorted for consistency.
func marshalPayload(buf *bytes.Buffer, payload map[string]interface{}) error {
	if len(payload) == 0 {
		return nil
	}

	// Sort keys for consistent ordering
	sortedPayload := make(map[string]interface{})
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sortedPayload[k] = payload[k]
	}

	b, err := yaml.Marshal(sortedPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	buf.Write(b)
	return nil
}

// writeYAMLMap marshals a map to YAML and writes it to the buffer.
func writeYAMLMap(buf *bytes.Buffer, m map[string]interface{}) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	buf.Write(b)
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
