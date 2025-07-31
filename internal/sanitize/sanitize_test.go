package sanitize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSanitize(t *testing.T) {
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

	assert.Equal(t, "v1", sanitized.GetAPIVersion())
	assert.Equal(t, "Pod", sanitized.GetKind())
	assert.Equal(t, "my-pod", sanitized.GetName())
	assert.Equal(t, "my-ns", sanitized.GetNamespace())
	assert.Equal(t, map[string]string{"app": "my-app"}, sanitized.GetLabels())
	assert.Equal(t, map[string]string{"my-annotation": "my-value"}, sanitized.GetAnnotations())

	spec, found, err := unstructured.NestedMap(sanitized.Object, "spec")
	assert.True(t, found)
	assert.NoError(t, err)
	assert.NotNil(t, spec)

	_, found, err = unstructured.NestedMap(sanitized.Object, "status")
	assert.False(t, found)
	assert.NoError(t, err)
}
