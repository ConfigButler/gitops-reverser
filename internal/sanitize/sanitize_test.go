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
	require.NoError(t, err)
	assert.NotNil(t, spec)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	require.NoError(t, err)

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
				"config.yaml":    "key: value",
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
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{
		"config.yaml":    "key: value",
		"app.properties": "debug=true",
	}, data)

	// Verify binaryData is preserved (it should be treated as part of data)
	binaryData, found, err := unstructured.NestedMap(sanitized.Object, "binaryData")
	assert.True(t, found)
	require.NoError(t, err)
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
	require.NoError(t, err)
	assert.NotNil(t, spec)

	// Verify status is removed
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	require.NoError(t, err)
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
	require.NoError(t, err)

	_, found, err = unstructured.NestedMap(sanitized.Object, "data")
	assert.False(t, found)
	require.NoError(t, err)

	// Verify other fields are preserved
	involvedObject, found, err := unstructured.NestedMap(sanitized.Object, "involvedObject")
	assert.True(t, found)
	require.NoError(t, err)
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
	require.NoError(t, err)

	// Verify nested structure is intact
	replicas, found, err := unstructured.NestedInt64(sanitized.Object, "spec", "replicas")
	assert.True(t, found)
	require.NoError(t, err)
	assert.Equal(t, int64(3), replicas)

	// Verify deeply nested fields - access containers array manually
	containers, found, err := unstructured.NestedSlice(sanitized.Object, "spec", "template", "spec", "containers")
	assert.True(t, found)
	require.NoError(t, err)
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
	require.NoError(t, err)
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

func TestSanitize_RemoveAutoGenMetadata(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":                       "test-pod",
				"namespace":                  "default",
				"uid":                        "12345-67890",
				"resourceVersion":            "999",
				"generation":                 int64(5),
				"creationTimestamp":          "2025-01-01T00:00:00Z",
				"deletionTimestamp":          "2025-01-02T00:00:00Z",
				"deletionGracePeriodSeconds": int64(30),
				"selfLink":                   "/api/v1/namespaces/default/pods/test-pod",
				"managedFields": []interface{}{
					map[string]interface{}{"manager": "kubectl"},
				},
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "ReplicaSet",
						"name":       "my-rs",
					},
				},
				"labels": map[string]interface{}{
					"app": "test",
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "nginx", "image": "nginx:latest"},
				},
			},
		},
	}

	sanitized := Sanitize(obj)

	metadata, found, err := unstructured.NestedMap(sanitized.Object, "metadata")
	require.True(t, found)
	require.NoError(t, err)

	// All auto-generated fields should be removed
	assert.NotContains(t, metadata, "uid")
	assert.NotContains(t, metadata, "resourceVersion")
	assert.NotContains(t, metadata, "generation")
	assert.NotContains(t, metadata, "creationTimestamp")
	assert.NotContains(t, metadata, "deletionTimestamp")
	assert.NotContains(t, metadata, "deletionGracePeriodSeconds")
	assert.NotContains(t, metadata, "selfLink")
	assert.NotContains(t, metadata, "managedFields")
	assert.NotContains(t, metadata, "ownerReferences")

	// User-defined fields should be preserved
	assert.Contains(t, metadata, "name")
	assert.Contains(t, metadata, "namespace")
	assert.Contains(t, metadata, "labels")
}

func TestSanitize_Service_RemoveClusterIPFields(t *testing.T) {
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
				"clusterIP":             "10.96.0.1",
				"clusterIPs":            []interface{}{"10.96.0.1"},
				"healthCheckNodePort":   int64(30000),
				"ipFamilies":            []interface{}{"IPv4"},
				"ipFamilyPolicy":        "SingleStack",
				"internalTrafficPolicy": "Cluster",
			},
		},
	}

	sanitized := Sanitize(obj)

	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	require.True(t, found)
	require.NoError(t, err)

	// All cluster-assigned fields should be removed
	assert.NotContains(t, spec, "clusterIP")
	assert.NotContains(t, spec, "clusterIPs")
	assert.NotContains(t, spec, "healthCheckNodePort")
	assert.NotContains(t, spec, "ipFamilies")
	assert.NotContains(t, spec, "ipFamilyPolicy")
	assert.NotContains(t, spec, "internalTrafficPolicy")

	// User-defined fields should be preserved
	assert.Contains(t, spec, "selector")
	assert.Contains(t, spec, "ports")
}

func TestSanitize_Pod_RemoveNodeName(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "my-pod",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "nginx",
						"image": "nginx:latest",
					},
				},
				"nodeName": "worker-node-1", // Should be removed
			},
		},
	}

	sanitized := Sanitize(obj)

	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	require.True(t, found)
	require.NoError(t, err)

	// nodeName should be removed
	assert.NotContains(t, spec, "nodeName")

	// containers should be preserved
	assert.Contains(t, spec, "containers")
}

func TestSanitize_PVC_RemoveVolumeFields(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]interface{}{
				"name":      "my-pvc",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"accessModes": []interface{}{"ReadWriteOnce"},
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"storage": "10Gi",
					},
				},
				"volumeName": "pvc-abc123", // Should be removed
				"volumeMode": "Filesystem", // Should be removed
			},
		},
	}

	sanitized := Sanitize(obj)

	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	require.True(t, found)
	require.NoError(t, err)

	// Volume-specific fields should be removed
	assert.NotContains(t, spec, "volumeName")
	assert.NotContains(t, spec, "volumeMode")

	// User-defined fields should be preserved
	assert.Contains(t, spec, "accessModes")
	assert.Contains(t, spec, "resources")
}

func TestSanitize_OperationalAnnotations(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "my-deployment",
				"namespace": "default",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": "should-be-removed",
					"control-plane.alpha.kubernetes.io/leader":         "should-be-removed",
					"deployment.kubernetes.io/revision":                "should-be-removed",
					"autoscaling.alpha.kubernetes.io/conditions":       "should-be-removed",
					"autoscaling.alpha.kubernetes.io/current-metrics":  "should-be-removed",
					"app.kubernetes.io/name":                           "should-be-kept",
					"example.com/custom":                               "should-be-kept",
				},
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
			},
		},
	}

	sanitized := Sanitize(obj)

	annotations := sanitized.GetAnnotations()

	// Operational annotations should be removed
	assert.NotContains(t, annotations, "kubectl.kubernetes.io/last-applied-configuration")
	assert.NotContains(t, annotations, "control-plane.alpha.kubernetes.io/leader")
	assert.NotContains(t, annotations, "deployment.kubernetes.io/revision")
	assert.NotContains(t, annotations, "autoscaling.alpha.kubernetes.io/conditions")
	assert.NotContains(t, annotations, "autoscaling.alpha.kubernetes.io/current-metrics")

	// User annotations should be preserved
	assert.Equal(t, "should-be-kept", annotations["app.kubernetes.io/name"])
	assert.Equal(t, "should-be-kept", annotations["example.com/custom"])
}

func TestSanitize_AllOperationalAnnotationsRemoved(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": "removed",
					"deployment.kubernetes.io/revision":                "removed",
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "nginx", "image": "nginx:latest"},
				},
			},
		},
	}

	sanitized := Sanitize(obj)

	// If all annotations are operational, the map should be nil
	annotations := sanitized.GetAnnotations()
	assert.Nil(t, annotations)
}

func TestSanitize_CustomResource(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "example.com/v1alpha1",
			"kind":       "MyApp",
			"metadata": map[string]interface{}{
				"name":            "my-app-instance",
				"namespace":       "prod",
				"uid":             "auto-generated-uid",
				"resourceVersion": "12345",
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"version":  "1.0.0",
			},
			"status": map[string]interface{}{
				"phase": "Running",
			},
		},
	}

	sanitized := Sanitize(obj)

	// Verify spec is preserved (entire spec is user-defined for CRDs)
	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	require.True(t, found)
	require.NoError(t, err)
	assert.Equal(t, int64(3), spec["replicas"])
	assert.Equal(t, "1.0.0", spec["version"])

	// Verify status is removed (observed state)
	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	require.NoError(t, err)

	// Verify auto-generated metadata is removed
	metadata, found, err := unstructured.NestedMap(sanitized.Object, "metadata")
	require.True(t, found)
	require.NoError(t, err)
	assert.NotContains(t, metadata, "uid")
	assert.NotContains(t, metadata, "resourceVersion")
}
