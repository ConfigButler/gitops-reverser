// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	api "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// applyIgnoringUnknownFields server-side applies obj as a client that ignores unknown fields.
// client.Client.Apply cannot express fieldValidation: Ignore, so the apply patch type is sent
// directly; the migration contract has to hold for exactly these lenient clients.
func applyIgnoringUnknownFields(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}

	return c.Patch(ctx, obj, client.RawPatch(types.ApplyPatchType, data), client.FieldOwner("migration-test"),
		&client.PatchOptions{Raw: &metav1.PatchOptions{FieldValidation: "Ignore"}})
}

func TestLiveMessageMigration_StoredValuesAndRemoval(t *testing.T) {
	ctx := context.Background()
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	sch := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(sch))
	require.NoError(t, apiextv1.AddToScheme(sch))
	c, err := client.New(cfg, client.Options{Scheme: sch})
	require.NoError(t, err)
	var crd apiextv1.CustomResourceDefinition
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "gittargets.configbutler.ai"}, &crd))
	current := crd.DeepCopy()
	message := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["commit"].Properties["message"]
	message.XValidations = nil
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["commit"].Properties["message"] = message
	require.NoError(t, c.Update(ctx, &crd))
	legacy := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3", "kind": "GitTarget",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec": map[string]any{
				"gitProviderRef": map[string]any{"name": "provider"},
				"branch":         "main",
				"path":           name,
				"commit": map[string]any{
					"message": map[string]any{"eventTemplate": "old event", "groupTemplate": "old group"},
				},
			},
		}}
	}
	for _, name := range []string{"migrate-apply", "migrate-patch"} {
		obj := legacy(name)
		require.Eventually(t, func() bool {
			return applyIgnoringUnknownFields(ctx, c, obj) == nil
		}, 10*time.Second, 100*time.Millisecond)
	}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&crd), &crd))
	crd.Spec = current.Spec
	require.NoError(t, c.Update(ctx, &crd))
	require.Eventually(t, func() bool {
		err := applyIgnoringUnknownFields(ctx, c, legacy("reject-new"))
		return err != nil && strings.Contains(err.Error(), "liveTemplate")
	}, 10*time.Second, 100*time.Millisecond)
	for _, name := range []string{"migrate-apply", "migrate-patch"} {
		var target api.GitTarget
		key := client.ObjectKey{Name: name, Namespace: "default"}
		require.NoError(t, c.Get(ctx, key, &target))
		ok, msg := validateCommitConfig(&target)
		require.False(t, ok)
		require.Contains(t, msg, "liveTemplate")
		target.Status.Conditions = []metav1.Condition{
			{
				Type:               "Validated",
				Status:             metav1.ConditionFalse,
				Reason:             "InvalidConfig",
				Message:            msg,
				ObservedGeneration: target.Generation,
				LastTransitionTime: metav1.Now(),
			},
		}
		require.NoError(t, c.Status().Update(ctx, &target))
		if name == "migrate-apply" {
			obj := legacy(name)
			require.NoError(
				t,
				unstructured.SetNestedMap(obj.Object, map[string]any{"liveTemplate": "chore: sync {{.Count}}"},
					"spec", "commit", "message"),
			)
			require.NoError(t, applyIgnoringUnknownFields(ctx, c, obj))
		} else {
			patch := []byte(
				`{"spec":{"commit":{"message":{"eventTemplate":null,"groupTemplate":null,"liveTemplate":"chore: sync {{.Count}}"}}}}`,
			)
			require.NoError(t, c.Patch(ctx, &target, client.RawPatch(types.MergePatchType, patch),
				&client.PatchOptions{Raw: &metav1.PatchOptions{FieldValidation: "Ignore"}}))
		}
		require.NoError(t, c.Get(ctx, key, &target))
		require.Equal(t, "chore: sync {{.Count}}", target.Spec.Commit.Message.LiveTemplate)
		ok, msg = validateCommitConfig(&target)
		require.True(t, ok, msg)
	}
}
