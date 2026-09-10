// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
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

const gitTargetCRDName = "gittargets.configbutler.ai"

// legacyGitTarget is the shape a GitTarget had before liveTemplate replaced the two
// per-event templates: the documents this migration has to keep working for.
func legacyGitTarget(name string) *unstructured.Unstructured {
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

// startCRDEnv brings up an envtest apiserver with the project CRDs and returns a client.
func startCRDEnv(t *testing.T) client.Client {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	sch := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(sch))
	require.NoError(t, apiextv1.AddToScheme(sch))
	c, err := client.New(cfg, client.Options{Scheme: sch})
	require.NoError(t, err)

	return c
}

// TestLiveMessageValidation_RejectsLegacyAcceptsMigrated pins what the SHIPPED schema does:
// it refuses the pre-migration shape and accepts the migrated one.
//
// It runs against the CRD exactly as installed, and never edits it. That is the point. This
// assertion used to live at the end of the migration test below, which got there by stripping
// the validation and then putting it back -- and waiting for the restore to take effect is a
// race the apiserver loses often enough to matter. The apiextensions apiserver rebuilds a CRD's
// serving handler asynchronously after a spec write, and under CPU pressure that rebuild is
// sometimes missed outright: the new schema reads back correctly while the previously compiled
// one goes on validating, and nothing retriggers it. Measured under load: 571 polls across 60s
// with the restored rules present in the live CRD and still unenforced. A longer timeout does
// not help, because the wait is not slow, it is stuck -- every run that passed did so within
// 6.3s, and every run that failed used its whole budget. Asserting against an unmodified CRD
// removes the rebuild from the picture rather than waiting on it.
func TestLiveMessageValidation_RejectsLegacyAcceptsMigrated(t *testing.T) {
	ctx := context.Background()
	c := startCRDEnv(t)

	err := applyIgnoringUnknownFields(ctx, c, legacyGitTarget("reject-legacy"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "liveTemplate")

	migrated := legacyGitTarget("accept-migrated")
	require.NoError(t, unstructured.SetNestedMap(migrated.Object,
		map[string]any{"liveTemplate": "chore: sync {{.Count}}"}, "spec", "commit", "message"))
	require.NoError(t, applyIgnoringUnknownFields(ctx, c, migrated))
}

func TestLiveMessageMigration_StoredValuesAndRemoval(t *testing.T) {
	ctx := context.Background()
	c := startCRDEnv(t)
	legacy := legacyGitTarget

	// Storing the pre-migration shape means getting it past the validation that forbids it, and
	// the only way in is to take that validation off the CRD. It is never put back: proving the
	// shipped schema still rejects this shape is TestLiveMessageValidation_RejectsLegacyAcceptsMigrated's
	// job, against an untouched CRD, precisely so that this test does not have to wait on a
	// second handler rebuild it cannot make the apiserver perform.
	var crd apiextv1.CustomResourceDefinition
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: gitTargetCRDName}, &crd))
	message := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["commit"].Properties["message"]
	message.XValidations = nil
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["commit"].Properties["message"] = message
	require.NoError(t, c.Update(ctx, &crd))

	// The first successful apply IS the evidence that the relaxed schema is being served.
	for _, name := range []string{"migrate-apply", "migrate-patch"} {
		obj := legacy(name)
		require.Eventually(t, func() bool {
			return applyIgnoringUnknownFields(ctx, c, obj) == nil
		}, 10*time.Second, 100*time.Millisecond)
	}
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
			patch := []byte(`{"spec":{"commit":{"message":{` +
				`"eventTemplate":null,"groupTemplate":null,` +
				`"liveTemplate":"chore: sync {{.Count}}"}}}}`)
			require.NoError(t, c.Patch(ctx, &target, client.RawPatch(types.MergePatchType, patch),
				&client.PatchOptions{Raw: &metav1.PatchOptions{FieldValidation: "Ignore"}}))
		}
		require.NoError(t, c.Get(ctx, key, &target))
		require.Equal(t, "chore: sync {{.Count}}", target.Spec.Commit.Message.LiveTemplate)
		ok, msg = validateCommitConfig(&target)
		require.True(t, ok, msg)
	}
}
