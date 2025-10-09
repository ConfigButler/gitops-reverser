package sanitize

import (
	"bytes"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// MarshalToOrderedYAML converts an unstructured object to YAML with guaranteed field order.
// Field order: apiVersion, kind, metadata, then payload (spec, data, rules, etc.)
func MarshalToOrderedYAML(obj *unstructured.Unstructured) ([]byte, error) {
	var buf bytes.Buffer

	// 1. apiVersion (first)
	apiVersion, err := yaml.Marshal(map[string]interface{}{"apiVersion": obj.GetAPIVersion()})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal apiVersion: %w", err)
	}
	buf.Write(apiVersion)

	// 2. kind (second)
	kind, err := yaml.Marshal(map[string]interface{}{"kind": obj.GetKind()})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal kind: %w", err)
	}
	buf.Write(kind)

	// 3. metadata (third) - use PartialObjectMeta for type safety and cleaning
	var metadata PartialObjectMeta
	metadata.FromUnstructured(obj)

	metadataMap := make(map[string]interface{})
	if metadata.Name != "" {
		metadataMap["name"] = metadata.Name
	}
	if metadata.Namespace != "" {
		metadataMap["namespace"] = metadata.Namespace
	}
	if len(metadata.Labels) > 0 {
		metadataMap["labels"] = metadata.Labels
	}
	if len(metadata.Annotations) > 0 {
		metadataMap["annotations"] = metadata.Annotations
	}

	metadataYAML, err := yaml.Marshal(map[string]interface{}{"metadata": metadataMap})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	buf.Write(metadataYAML)

	// 4. payload fields (spec, data, rules, etc.) - everything except apiVersion, kind, metadata, status
	payload := make(map[string]interface{})
	for k, v := range obj.Object {
		if k != "apiVersion" && k != "kind" && k != "metadata" && k != "status" {
			payload[k] = v
		}
	}

	if len(payload) > 0 {
		payloadYAML, err := yaml.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
		buf.Write(payloadYAML)
	}

	return buf.Bytes(), nil
}
