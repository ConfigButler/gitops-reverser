package sanitize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPartialObjectMeta_FromUnstructured(t *testing.T) {
	tests := []struct {
		name     string
		obj      *unstructured.Unstructured
		expected PartialObjectMeta
	}{
		{
			name: "basic object with all fields",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"name":      "test-pod",
						"namespace": "default",
						"labels": map[string]interface{}{
							"app": "test",
							"env": "prod",
						},
						"annotations": map[string]interface{}{
							"description": "test annotation",
						},
					},
				},
			},
			expected: PartialObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Labels: map[string]string{
					"app": "test",
					"env": "prod",
				},
				Annotations: map[string]string{
					"description": "test annotation",
				},
			},
		},
		{
			name: "object with kubectl annotations filtered",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"name":      "test-pod",
						"namespace": "default",
						"annotations": map[string]interface{}{
							"kubectl.kubernetes.io/last-applied-configuration": "should-be-removed",
							"user-annotation": "should-be-kept",
						},
					},
				},
			},
			expected: PartialObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"user-annotation": "should-be-kept",
				},
			},
		},
		{
			name: "cluster-scoped object (no namespace)",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"name": "test-clusterrole",
						"labels": map[string]interface{}{
							"rbac": "admin",
						},
					},
				},
			},
			expected: PartialObjectMeta{
				Name:      "test-clusterrole",
				Namespace: "",
				Labels: map[string]string{
					"rbac": "admin",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var meta PartialObjectMeta
			meta.FromUnstructured(tt.obj)
			assert.Equal(t, tt.expected, meta)
		})
	}
}

func TestCleanAnnotations(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		expected map[string]string
	}{
		{
			name:     "nil annotations",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty annotations",
			input:    map[string]string{},
			expected: nil,
		},
		{
			name: "remove kubectl annotations",
			input: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "removed",
				"user-annotation": "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			name: "remove control-plane annotations",
			input: map[string]string{
				"control-plane.alpha.kubernetes.io/leader": "removed",
				"user-annotation":                          "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			name: "remove deployment annotations",
			input: map[string]string{
				"deployment.kubernetes.io/revision": "removed",
				"user-annotation":                   "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			name: "remove autoscaling annotations",
			input: map[string]string{
				"autoscaling.alpha.kubernetes.io/conditions":      "removed",
				"autoscaling.alpha.kubernetes.io/current-metrics": "removed",
				"user-annotation": "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			name: "all annotations operational - return nil",
			input: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "removed",
				"control-plane.alpha.kubernetes.io/leader":         "removed",
			},
			expected: nil,
		},
		{
			name: "keep user annotations",
			input: map[string]string{
				"app.kubernetes.io/name":         "kept",
				"app.kubernetes.io/version":      "kept",
				"example.com/custom":             "kept",
				"prometheus.io/scrape":           "kept",
				"cert-manager.io/cluster-issuer": "kept",
			},
			expected: map[string]string{
				"app.kubernetes.io/name":         "kept",
				"app.kubernetes.io/version":      "kept",
				"example.com/custom":             "kept",
				"prometheus.io/scrape":           "kept",
				"cert-manager.io/cluster-issuer": "kept",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cleanAnnotations(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
