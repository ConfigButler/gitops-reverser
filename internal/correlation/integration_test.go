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

package correlation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestSanitizationPipeline_KeyOrderIndependence verifies that different YAML key orders
// produce identical keys after going through the sanitization pipeline.
func TestSanitizationPipeline_KeyOrderIndependence(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	// YAML with keys in one order
	yaml1 := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "my-app",
			"namespace": "default",
			"labels": map[string]interface{}{
				"app":     "my-app",
				"version": "v1",
			},
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "my-app",
				},
			},
		},
	}

	// YAML with keys in different order (metadata fields reversed, spec fields reversed)
	yaml2 := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"namespace": "default",
			"name":      "my-app",
			"labels": map[string]interface{}{
				"version": "v1",
				"app":     "my-app",
			},
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app": "my-app",
				},
			},
			"replicas": int64(3),
		},
	}

	// Convert to unstructured and sanitize
	obj1 := &unstructured.Unstructured{Object: yaml1}
	obj2 := &unstructured.Unstructured{Object: yaml2}

	sanitized1 := sanitize.Sanitize(obj1)
	sanitized2 := sanitize.Sanitize(obj2)

	// Marshal to ordered YAML
	orderedYAML1, err := sanitize.MarshalToOrderedYAML(sanitized1)
	require.NoError(t, err, "Failed to marshal first object")

	orderedYAML2, err := sanitize.MarshalToOrderedYAML(sanitized2)
	require.NoError(t, err, "Failed to marshal second object")

	// Generate keys
	key1 := GenerateKey(id, operation, orderedYAML1)
	key2 := GenerateKey(id, operation, orderedYAML2)

	// Keys should be identical despite different input key order
	assert.Equal(t, key1, key2,
		"Sanitization pipeline should produce identical keys regardless of input key order")
}

// TestSanitizationPipeline_WhitespaceIndependence verifies that different
// whitespace/indentation in raw YAML produces identical keys after sanitization.
func TestSanitizationPipeline_WhitespaceIndependence(t *testing.T) {
	id := types.NewResourceIdentifier("", "v1", "configmaps", "default", "my-config")
	operation := "CREATE"

	// Compact YAML
	rawYAML1 := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: default
data:
  key1: value1
  key2: value2
`)

	// Excessive whitespace
	rawYAML2 := []byte(`apiVersion:  v1
kind:   ConfigMap
metadata:
  name:    my-config
  namespace:   default
data:
  key1:   value1
  key2:   value2
`)

	// Different indentation
	rawYAML3 := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
    name: my-config
    namespace: default
data:
    key1: value1
    key2: value2
`)

	// Parse all variations
	obj1 := parseYAML(t, rawYAML1)
	obj2 := parseYAML(t, rawYAML2)
	obj3 := parseYAML(t, rawYAML3)

	// Sanitize and marshal
	key1 := generateKeyFromObject(t, obj1, id, operation)
	key2 := generateKeyFromObject(t, obj2, id, operation)
	key3 := generateKeyFromObject(t, obj3, id, operation)

	// All should produce identical keys
	assert.Equal(t, key1, key2, "Excessive whitespace should not affect key")
	assert.Equal(t, key1, key3, "Different indentation should not affect key")
}

// TestSanitizationPipeline_ManagedFieldsRemoval verifies that managed fields
// and other runtime metadata don't affect correlation.
func TestSanitizationPipeline_ManagedFieldsRemoval(t *testing.T) {
	id := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "my-app")
	operation := "UPDATE"

	// Object without managed fields
	yaml1 := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "my-app",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
		},
	}

	// Same object with managed fields, resourceVersion, uid, etc.
	yaml2 := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":              "my-app",
			"namespace":         "default",
			"uid":               "12345678-1234-1234-1234-123456789abc",
			"resourceVersion":   "98765",
			"generation":        int64(5),
			"creationTimestamp": "2025-01-15T10:00:00Z",
			"managedFields": []interface{}{
				map[string]interface{}{
					"manager":   "kubectl",
					"operation": "Update",
				},
			},
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
		},
		"status": map[string]interface{}{
			"replicas": int64(3),
		},
	}

	obj1 := &unstructured.Unstructured{Object: yaml1}
	obj2 := &unstructured.Unstructured{Object: yaml2}

	key1 := generateKeyFromObject(t, obj1, id, operation)
	key2 := generateKeyFromObject(t, obj2, id, operation)

	// Should produce identical keys after sanitization removes runtime fields
	assert.Equal(t, key1, key2,
		"Sanitization should remove managed fields, resourceVersion, status, etc.")
}

// parseYAML is a helper to parse YAML into an unstructured object.
func parseYAML(t *testing.T, yamlBytes []byte) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	err := yaml.Unmarshal(yamlBytes, &obj.Object)
	require.NoError(t, err, "Failed to parse YAML")
	return obj
}

// generateKeyFromObject is a helper that runs the full sanitization pipeline
// and generates a correlation key.
func generateKeyFromObject(
	t *testing.T,
	obj *unstructured.Unstructured,
	id types.ResourceIdentifier,
	operation string,
) string {
	t.Helper()

	// Run sanitization pipeline (same as production code)
	sanitized := sanitize.Sanitize(obj)
	orderedYAML, err := sanitize.MarshalToOrderedYAML(sanitized)
	require.NoError(t, err, "Failed to marshal sanitized object")

	return GenerateKey(id, operation, orderedYAML)
}
