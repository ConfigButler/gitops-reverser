// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestObjectSelectorSchema_RejectsMalformedSelectorsAtTheAPIServer checks the admission half the
// CRD schema carries for both rule kinds: the operator enum, the values/operator pairing, and the
// requirement key and value syntax. matchLabels is left to the admission webhook: a CEL check over
// a map of unbounded strings under the unbounded rules[] list exceeds the CRD cost budget. The rule
// compiler re-checks all of it. Asserted against a real API server, the only thing that can
// evaluate the schema and its CEL.
func TestObjectSelectorSchema_RejectsMalformedSelectorsAtTheAPIServer(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "start envtest control plane (the CRD's CEL cost must fit the budget)")
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	ctx := context.Background()

	rule := func(kind, name string, selector map[string]any) *unstructured.Unstructured {
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
					"resources":      []any{"configmaps"},
					"objectSelector": selector,
				}},
			},
		}}
	}
	expression := func(key, operator string, values ...any) map[string]any {
		e := map[string]any{"key": key, "operator": operator}
		if values != nil {
			e["values"] = values
		}
		return map[string]any{"matchExpressions": []any{e}}
	}

	rejected := map[string]map[string]any{
		"unknown operator":      expression("team", "Gt", "1"),
		"In without values":     expression("team", "In"),
		"Exists with values":    expression("team", "Exists", "a"),
		"invalid key":           expression("bad key", "Exists"),
		"invalid value":         expression("team", "In", "not valid!"),
		"value longer than 63":  expression("team", "In", fmt.Sprintf("%064d", 0)),
		"empty requirement key": expression("", "Exists"),
	}
	for _, kind := range []string{"WatchRule", "ClusterWatchRule"} {
		for name, selector := range rejected {
			t.Run(kind+"/rejects "+name, func(t *testing.T) {
				require.Error(t, c.Create(ctx, rule(kind, "rejected", selector)))
			})
		}
		t.Run(kind+"/accepts a valid selector", func(t *testing.T) {
			valid := map[string]any{
				"matchLabels": map[string]any{"internal.cozystack.io/tenantresource": "true"},
				"matchExpressions": []any{
					map[string]any{"key": "team", "operator": "In", "values": []any{"a", "b"}},
					map[string]any{"key": "legacy", "operator": "DoesNotExist"},
				},
			}
			require.NoError(t, c.Create(ctx, rule(kind, "accepted", valid)))
		})
	}
}
