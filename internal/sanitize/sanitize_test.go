package sanitize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSanitize_BasicPod(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "my-pod",
				"namespace": "my-ns",
				"labels": map[string]interface{}{
					"app": "my-app",
				},
				"annotations": map[string]interface{}{
					"my-annotation": "my-value",
				},
				"creationTimestamp": "2025-01-01T00:00:00Z",
				"uid":               "1234-5678",
				"resourceVersion":   "12345",
				"generation":        int64(1),
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "my-container",
						"image": "my-image",
					},
				},
			},
			"status": map[string]interface{}{
				"phase": "Running",
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify preserved fields
	assert.Equal(t, "v1", sanitized.GetAPIVersion())
	assert.Equal(t, "Pod", sanitized.GetKind())
	assert.Equal(t, "my-pod", sanitized.GetName())
	assert.Equal(t, "my-ns", sanitized.GetNamespace())
	assert.Equal(t, map[string]string{"app": "my-app"}, sanitized.GetLabels())
	assert.Equal(t, map[string]string{"my-annotation": "my-value"}, sanitized.GetAnnotations())

	// Verify spec is preserved
	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.NotNil(t, spec)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	assert.NoError(t, err)

	// Verify server-generated metadata fields are not present
	metadata, found, err := unstructured.NestedMap(sanitized.Object, "metadata")
	require.True(t, found)
	require.NoError(t, err)
	
	_, exists := metadata["creationTimestamp"]
	assert.False(t, exists, "creationTimestamp should be removed")
	_, exists = metadata["uid"]
	assert.False(t, exists, "uid should be removed")
	_, exists = metadata["resourceVersion"]
	assert.False(t, exists, "resourceVersion should be removed")
	_, exists = metadata["generation"]
	assert.False(t, exists, "generation should be removed")
}

func TestSanitize_ConfigMapWithData(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "my-config",
				"namespace": "my-ns",
			},
			"data": map[string]interface{}{
				"config.yaml": "key: value",
				"app.properties": "debug=true",
			},
			"binaryData": map[string]interface{}{
				"binary-file": "base64encodeddata",
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify data is preserved
	data, found, err := unstructured.NestedMap(sanitized.Object, "data")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.Equal(t, map[string]interface{}{
		"config.yaml": "key: value",
		"app.properties": "debug=true",
	}, data)

	// Verify binaryData is preserved (it should be treated as part of data)
	binaryData, found, err := unstructured.NestedMap(sanitized.Object, "binaryData")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.Equal(t, map[string]interface{}{
		"binary-file": "base64encodeddata",
	}, binaryData)
}

func TestSanitize_ClusterScopedResource(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": "my-namespace",
				"labels": map[string]interface{}{
					"environment": "production",
				},
				"finalizers": []interface{}{"kubernetes"},
			},
			"spec": map[string]interface{}{
				"finalizers": []interface{}{"kubernetes"},
			},
			"status": map[string]interface{}{
				"phase": "Active",
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify namespace field is empty for cluster-scoped resources
	assert.Empty(t, sanitized.GetNamespace())
	assert.Equal(t, "my-namespace", sanitized.GetName())
	assert.Equal(t, "Namespace", sanitized.GetKind())

	// Verify spec is preserved
	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.NotNil(t, spec)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	assert.NoError(t, err)
}

func TestSanitize_EmptyLabelsAndAnnotations(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      "my-service",
				"namespace": "default",
				// No labels or annotations
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "my-app",
				},
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify empty labels and annotations are handled correctly
	assert.Nil(t, sanitized.GetLabels())
	assert.Nil(t, sanitized.GetAnnotations())
}

func TestSanitize_NilLabelsAndAnnotations(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":        "my-service",
				"namespace":   "default",
				"labels":      nil,
				"annotations": nil,
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "my-app",
				},
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify nil labels and annotations are handled correctly
	assert.Nil(t, sanitized.GetLabels())
	assert.Nil(t, sanitized.GetAnnotations())
}

func TestSanitize_NoSpecOrData(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Event",
			"metadata": map[string]interface{}{
				"name":      "my-event",
				"namespace": "default",
			},
			// Events typically don't have spec or data fields
			"involvedObject": map[string]interface{}{
				"kind": "Pod",
				"name": "my-pod",
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify object is still valid even without spec/data
	assert.Equal(t, "Event", sanitized.GetKind())
	assert.Equal(t, "my-event", sanitized.GetName())

	// Verify spec and data fields are not present
	_, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	assert.False(t, found)
	assert.NoError(t, err)

	_, found, err = unstructured.NestedMap(sanitized.Object, "data")
	assert.False(t, found)
	assert.NoError(t, err)

	// Verify other fields are preserved
	involvedObject, found, err := unstructured.NestedMap(sanitized.Object, "involvedObject")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.Equal(t, map[string]interface{}{
		"kind": "Pod",
		"name": "my-pod",
	}, involvedObject)
}

func TestSanitize_ComplexNestedSpec(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "my-deployment",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": "my-app",
					},
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "my-app",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:1.20",
								"ports": []interface{}{
									map[string]interface{}{
										"containerPort": int64(80),
									},
								},
							},
						},
					},
				},
			},
			"status": map[string]interface{}{
				"replicas":      int64(3),
				"readyReplicas": int64(2),
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify complex nested spec is preserved
	_, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	assert.True(t, found)
	assert.NoError(t, err)

	// Verify nested structure is intact
	replicas, found, err := unstructured.NestedInt64(sanitized.Object, "spec", "replicas")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.Equal(t, int64(3), replicas)

	// Verify deeply nested fields - access containers array manually
	containers, found, err := unstructured.NestedSlice(sanitized.Object, "spec", "template", "spec", "containers")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.Len(t, containers, 1)
	
	// Access first container
	container, ok := containers[0].(map[string]interface{})
	assert.True(t, ok)
	
	// Access ports array
	ports, ok := container["ports"].([]interface{})
	assert.True(t, ok)
	assert.Len(t, ports, 1)
	
	// Access first port
	port, ok := ports[0].(map[string]interface{})
	assert.True(t, ok)
	
	// Check containerPort
	containerPort, ok := port["containerPort"].(int64)
	assert.True(t, ok)
	assert.Equal(t, int64(80), containerPort)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	assert.NoError(t, err)
}

func TestSanitize_PreservesOriginalObject(t *testing.T) {
	original := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "my-pod",
				"namespace": "my-ns",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "my-container",
						"image": "my-image",
					},
				},
			},
			"status": map[string]interface{}{
				"phase": "Running",
			},
		},
	}

	// Make a copy to compare later
	originalCopy := original.DeepCopy()

	sanitized := Sanitize(original)

	// Verify original object is unchanged
	assert.Equal(t, originalCopy, original)

	// Verify sanitized is different
	assert.NotEqual(t, original, sanitized)

	// Verify status exists in original but not in sanitized
	_, found, _ := unstructured.NestedMap(original.Object, "status")
	assert.True(t, found, "Original should still have status")

	_, found, _ = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found, "Sanitized should not have status")
}
