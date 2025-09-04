package git

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func createTestPodWithResourceVersion(name, namespace, resourceVersion string) *unstructured.Unstructured {
	pod := createTestPod(name, namespace)
	pod.SetResourceVersion(resourceVersion)
	return pod
}

func createTestPod(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "test-container",
						"image": "nginx",
					},
				},
			},
		},
	}
}
