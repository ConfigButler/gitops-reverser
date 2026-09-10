// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

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

const crdBaseDir = "../../config/crd/bases"

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

// startEnv brings up an envtest apiserver holding exactly the given CRDs and returns a client.
// Passing the CRDs as objects rather than a directory is what lets a caller install a MODIFIED
// schema, without ever editing one that is already being served.
func startEnv(t *testing.T, crds ...*apiextv1.CustomResourceDefinition) client.Client {
	t.Helper()
	env := &envtest.Environment{}
	if len(crds) == 0 {
		env.CRDDirectoryPaths = []string{crdBaseDir}
		env.ErrorIfCRDPathMissing = true
	} else {
		env.CRDs = crds
	}
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

// gitTargetCRDWithoutMessageValidation reads the shipped GitTarget CRD and returns it with the
// retirement rules on spec.commit.message removed.
//
// The point is WHEN this happens: before the apiserver has ever seen the CRD, so the relaxed
// schema is the only one it ever compiles. Editing a live CRD instead means asking it to rebuild
// a serving handler, and under CPU pressure that rebuild is sometimes missed outright -- the new
// schema reads back correctly while the previously compiled one goes on validating, with nothing
// to retrigger it. Doing the edit here removes the rebuild from the test rather than waiting on
// one, which is not something a timeout can fix: measured under load, every run that converged
// did so within 6.3s and every run that failed used its entire budget, whether that was 10
// seconds or 60.
func gitTargetCRDWithoutMessageValidation(t *testing.T) *apiextv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(crdBaseDir, "configbutler.ai_gittargets.yaml"))
	require.NoError(t, err)
	var crd apiextv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	for i := range crd.Spec.Versions {
		root := crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		spec := root.Properties["spec"]
		commit := spec.Properties["commit"]
		msg := commit.Properties["message"]
		require.NotEmpty(t, msg.XValidations, "shipped CRD no longer carries the rules this test relaxes")
		msg.XValidations = nil
		commit.Properties["message"] = msg
		spec.Properties["commit"] = commit
		root.Properties["spec"] = spec
	}

	return &crd
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
	c := startEnv(t)

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
	legacy := legacyGitTarget

	// Storing the pre-migration shape means getting it past the validation that forbids it. That
	// relaxation is applied to the CRD BEFORE it is installed, so the apiserver compiles it once
	// and nothing here ever waits on a schema change. Proving the SHIPPED schema still rejects
	// this shape is TestLiveMessageValidation_RejectsLegacyAcceptsMigrated's job, against a CRD
	// that is never modified at all.
	c := startEnv(t, gitTargetCRDWithoutMessageValidation(t))
	for _, name := range []string{"migrate-apply", "migrate-patch"} {
		require.NoError(t, applyIgnoringUnknownFields(ctx, c, legacy(name)))
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
