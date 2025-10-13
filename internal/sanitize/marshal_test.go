/*
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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestMarshalToOrderedYAML_FieldOrder(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "test-deployment",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Verify field order by checking line positions
	apiVersionPos := strings.Index(yamlStr, "apiVersion:")
	kindPos := strings.Index(yamlStr, "kind:")
	metadataPos := strings.Index(yamlStr, "metadata:")
	specPos := strings.Index(yamlStr, "spec:")

	assert.Less(t, apiVersionPos, kindPos, "apiVersion should come before kind")
	assert.Less(t, kindPos, metadataPos, "kind should come before metadata")
	assert.Less(t, metadataPos, specPos, "metadata should come before spec")
}

func TestMarshalToOrderedYAML_Pod(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "nginx",
				"namespace": "default",
				"labels": map[string]interface{}{
					"app": "nginx",
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "nginx",
						"image": "nginx:latest",
					},
				},
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Verify it's valid YAML
	var parsed map[string]interface{}
	err = yaml.Unmarshal(yamlBytes, &parsed)
	require.NoError(t, err)

	// Verify field order
	lines := strings.Split(yamlStr, "\n")
	assert.Equal(t, "apiVersion: v1", lines[0])
	assert.Equal(t, "kind: Pod", lines[1])
	assert.True(t, strings.HasPrefix(lines[2], "metadata:"))
}

func TestMarshalToOrderedYAML_Service(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "my-service",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "my-app",
				},
				"ports": []interface{}{
					map[string]interface{}{
						"port":       int64(80),
						"targetPort": int64(8080),
					},
				},
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	// Verify valid Kubernetes YAML
	var reparsed unstructured.Unstructured
	err = yaml.Unmarshal(yamlBytes, &reparsed.Object)
	require.NoError(t, err)

	assert.Equal(t, "v1", reparsed.GetAPIVersion())
	assert.Equal(t, "Service", reparsed.GetKind())
	assert.Equal(t, "my-service", reparsed.GetName())
	assert.Equal(t, "default", reparsed.GetNamespace())
}

func TestMarshalToOrderedYAML_ConfigMap(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "app-config",
				"namespace": "production",
			},
			"data": map[string]interface{}{
				"config.json": `{"key": "value"}`,
				"app.yaml":    "setting: true",
			},
			"binaryData": map[string]interface{}{
				"logo.png": "iVBORw0KG...",
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Verify both data and binaryData are in payload
	assert.Contains(t, yamlStr, "data:")
	assert.Contains(t, yamlStr, "binaryData:")
	assert.Contains(t, yamlStr, "config.json")
	assert.Contains(t, yamlStr, "logo.png")
}

func TestMarshalToOrderedYAML_ClusterScopedResource(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata": map[string]interface{}{
				"name": "admin",
			},
			"rules": []interface{}{
				map[string]interface{}{
					"apiGroups": []interface{}{"*"},
					"resources": []interface{}{"*"},
					"verbs":     []interface{}{"*"},
				},
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Verify no namespace field for cluster-scoped resources
	assert.NotContains(t, yamlStr, "namespace:")

	// Verify rules are in payload
	assert.Contains(t, yamlStr, "rules:")
}

func TestMarshalToOrderedYAML_EmptyMetadata(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name": "minimal-pod",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "nginx",
						"image": "nginx",
					},
				},
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Should have minimal metadata
	assert.Contains(t, yamlStr, "metadata:")
	assert.Contains(t, yamlStr, "name: minimal-pod")
}

func TestMarshalToOrderedYAML_StatusRemoved(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "nginx",
						"image": "nginx",
					},
				},
			},
			"status": map[string]interface{}{
				"phase": "Running",
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Status should not be in output
	assert.NotContains(t, yamlStr, "status:")
	assert.NotContains(t, yamlStr, "phase:")
}

func TestMarshalToOrderedYAML_RoundTrip(t *testing.T) {
	original := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "my-service",
				"namespace": "default",
				"labels": map[string]interface{}{
					"app": "test",
				},
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "test",
				},
				"ports": []interface{}{
					map[string]interface{}{
						"port":       int64(80),
						"targetPort": int64(8080),
					},
				},
			},
		},
	}

	// Marshal to YAML
	yamlBytes, err := MarshalToOrderedYAML(original)
	require.NoError(t, err)

	// Unmarshal back
	var reparsed unstructured.Unstructured
	err = yaml.Unmarshal(yamlBytes, &reparsed.Object)
	require.NoError(t, err)

	// Verify core fields match
	assert.Equal(t, original.GetAPIVersion(), reparsed.GetAPIVersion())
	assert.Equal(t, original.GetKind(), reparsed.GetKind())
	assert.Equal(t, original.GetName(), reparsed.GetName())
	assert.Equal(t, original.GetNamespace(), reparsed.GetNamespace())
	assert.Equal(t, original.GetLabels(), reparsed.GetLabels())

	// Verify spec content exists (YAML unmarshaling converts numbers to float64)
	_, found, _ := unstructured.NestedMap(original.Object, "spec")
	require.True(t, found)
	reparsedSpec, found, _ := unstructured.NestedMap(reparsed.Object, "spec")
	require.True(t, found)

	// Compare structure, not exact types (YAML converts int64 to float64)
	assert.Contains(t, reparsedSpec, "selector")
	assert.Contains(t, reparsedSpec, "ports")
}

func TestMarshalToOrderedYAML_CustomResource(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "example.com/v1alpha1",
			"kind":       "MyApp",
			"metadata": map[string]interface{}{
				"name":      "app-instance",
				"namespace": "prod",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"version":  "1.0.0",
				"config": map[string]interface{}{
					"feature1": true,
					"feature2": false,
				},
			},
		},
	}

	yamlBytes, err := MarshalToOrderedYAML(obj)
	require.NoError(t, err)

	yamlStr := string(yamlBytes)

	// Verify field order
	assert.Less(t, strings.Index(yamlStr, "apiVersion:"), strings.Index(yamlStr, "kind:"))
	assert.Less(t, strings.Index(yamlStr, "kind:"), strings.Index(yamlStr, "metadata:"))
	assert.Less(t, strings.Index(yamlStr, "metadata:"), strings.Index(yamlStr, "spec:"))

	// Verify it's valid YAML
	var parsed map[string]interface{}
	err = yaml.Unmarshal(yamlBytes, &parsed)
	require.NoError(t, err)
	assert.Equal(t, "example.com/v1alpha1", parsed["apiVersion"])
}
