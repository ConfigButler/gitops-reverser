// SPDX-License-Identifier: Apache-2.0

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
			name: "object with operational flux labels filtered",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"name":      "test-pod",
						"namespace": "default",
						"labels": map[string]interface{}{
							"kustomize.toolkit.fluxcd.io/name":      "live-sync",
							"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
							"user-label":                            "should-be-kept",
						},
					},
				},
			},
			expected: PartialObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Labels: map[string]string{
					"user-label": "should-be-kept",
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

func TestCleanLabels(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		expected map[string]string
	}{
		{
			name:     "nil labels",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty labels",
			input:    map[string]string{},
			expected: nil,
		},
		{
			name: "remove flux kustomize labels",
			input: map[string]string{
				"kustomize.toolkit.fluxcd.io/name":      "removed",
				"kustomize.toolkit.fluxcd.io/namespace": "removed",
				"user-label":                            "kept",
			},
			expected: map[string]string{
				"user-label": "kept",
			},
		},
		{
			name: "all labels operational - return nil",
			input: map[string]string{
				"kustomize.toolkit.fluxcd.io/name": "removed",
			},
			expected: nil,
		},
		{
			name: "remove kro labels",
			input: map[string]string{
				"kro.run/owned-by": "removed",
				"user-label":       "kept",
			},
			expected: map[string]string{
				"user-label": "kept",
			},
		},
		{
			name: "remove applyset labels",
			input: map[string]string{
				"applyset.kubernetes.io/part-of": "removed",
				"user-label":                     "kept",
			},
			expected: map[string]string{
				"user-label": "kept",
			},
		},
		{
			name: "keep user labels",
			input: map[string]string{
				"app.kubernetes.io/name": "kept",
				"example.com/custom":     "kept",
			},
			expected: map[string]string{
				"app.kubernetes.io/name": "kept",
				"example.com/custom":     "kept",
			},
		},
		{
			// Argo CD's `label` / `annotation+label` tracking methods stamp
			// app.kubernetes.io/instance, but so do Helm and Kustomize for entirely
			// legitimate reasons. The sanitizer cannot tell them apart, so it must
			// keep the label. docs/bi-directional.md tells users to leave Argo CD on
			// its default `annotation` tracking for reverser-managed paths.
			name: "keep app.kubernetes.io/instance even though Argo CD may own it",
			input: map[string]string{
				"app.kubernetes.io/instance": "kept",
			},
			expected: map[string]string{
				"app.kubernetes.io/instance": "kept",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cleanLabels(tt.input)
			assert.Equal(t, tt.expected, result)
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
			name: "remove applyset annotations",
			input: map[string]string{
				"applyset.kubernetes.io/tooling": "removed",
				"user-annotation":                "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			// Argo CD's repo-server stamps tracking-id onto every non-CRD object it
			// applies. It is controller bookkeeping; committing it to Git makes a
			// second Application fail to sync with "Shared resource found", because
			// Argo never validates a tracking-id against the object carrying it.
			name: "remove Argo CD resource-tracking bookkeeping",
			input: map[string]string{
				"argocd.argoproj.io/tracking-id":     "my-app:example.com/IceCreamOrder:ns/order-1",
				"argocd.argoproj.io/installation-id": "removed",
				"user-annotation":                    "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			// The exact-key set exists precisely so these survive. A blanket
			// `argocd.argoproj.io/` prefix strip would silently drop a user's sync
			// ordering and hook declarations.
			name: "keep Argo CD annotations that are user intent",
			input: map[string]string{
				"argocd.argoproj.io/sync-wave":       "kept",
				"argocd.argoproj.io/sync-options":    "kept",
				"argocd.argoproj.io/compare-options": "kept",
				"argocd.argoproj.io/hook":            "kept",
			},
			expected: map[string]string{
				"argocd.argoproj.io/sync-wave":       "kept",
				"argocd.argoproj.io/sync-options":    "kept",
				"argocd.argoproj.io/compare-options": "kept",
				"argocd.argoproj.io/hook":            "kept",
			},
		},
		{
			// kcp names the logical cluster an object was read from. It is an address,
			// not intent: in Git it would pin the manifest to one workspace and travel
			// with it into every other.
			name: "remove kcp logical-cluster annotation",
			input: map[string]string{
				"kcp.io/cluster":  "root:org:team",
				"user-annotation": "kept",
			},
			expected: map[string]string{
				"user-annotation": "kept",
			},
		},
		{
			// Exact-key, not a `kcp.io/` prefix strip: sibling keys under the prefix are
			// not assumed to be bookkeeping.
			name: "keep other kcp annotations",
			input: map[string]string{
				"kcp.io/path": "kept",
			},
			expected: map[string]string{
				"kcp.io/path": "kept",
			},
		},
		{
			name: "all annotations operational - return nil",
			input: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "removed",
				"control-plane.alpha.kubernetes.io/leader":         "removed",
				"argocd.argoproj.io/tracking-id":                   "removed",
				"kcp.io/cluster":                                   "removed",
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
