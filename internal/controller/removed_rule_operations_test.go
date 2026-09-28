// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestRemovedRuleOperations_StaleManifestsAreRejectedOrPruned covers a manifest written before
// rules[].operations was removed from both rule kinds. The field is gone, so it must never be
// mistaken for deletion protection: a Strict request is refused outright, and a Warn or Ignore
// request is accepted with the field pruned, which leaves a rule that observes every event.
//
// Kubernetes decides between the two by the request's field validation, not the CRD, so all three
// modes are asserted rather than the rejection alone.
func TestRemovedRuleOperations_StaleManifestsAreRejectedOrPruned(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "start envtest control plane")
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	ctx := context.Background()

	staleRule := func(kind, name string) *unstructured.Unstructured {
		gitTargetRef := map[string]any{"name": "target"}
		metadata := map[string]any{"name": name}
		if kind == "ClusterWatchRule" {
			gitTargetRef["namespace"] = "default"
		} else {
			metadata["namespace"] = "default"
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       kind,
			"metadata":   metadata,
			"spec": map[string]any{
				"gitTargetRef": gitTargetRef,
				"rules": []any{map[string]any{
					"operations": []any{"CREATE"},
					"resources":  []any{"configmaps"},
				}},
			},
		}}
	}

	for _, kind := range []string{"WatchRule", "ClusterWatchRule"} {
		t.Run(kind+"/Strict", func(t *testing.T) {
			err := c.Create(ctx, staleRule(kind, "strict"), client.FieldValidation("Strict"))
			require.Error(t, err)
			require.Contains(t, err.Error(), `unknown field "spec.rules[0].operations"`)
		})

		for _, mode := range []string{"Warn", "Ignore"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				name := "lenient-" + strings.ToLower(mode)
				obj := staleRule(kind, name)
				require.NoError(t, c.Create(ctx, obj, client.FieldValidation(mode)))

				stored := &unstructured.Unstructured{}
				stored.SetGroupVersionKind(obj.GroupVersionKind())
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), stored))
				rules, found, err := unstructured.NestedSlice(stored.Object, "spec", "rules")
				require.NoError(t, err)
				require.True(t, found)
				require.Len(t, rules, 1)
				rule, ok := rules[0].(map[string]any)
				require.True(t, ok)
				require.NotContains(t, rule, "operations", "the removed field is pruned, not stored")
				require.Equal(t, []any{"configmaps"}, rule["resources"])
			})
		}
	}
}
