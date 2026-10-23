// SPDX-License-Identifier: Apache-2.0

package argosim

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// The rules Serve applies are the ones internal/controller/argocdtrigger_apiserver_test.go measures
// on a real API server; these pin that the fake keeps them.

func application(syncPolicy map[string]any) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": "checkout"},
		"spec":     map[string]any{"source": map[string]any{"repoURL": "https://github.com/acme/platform"}},
	}}
	if syncPolicy != nil {
		_ = unstructured.SetNestedMap(app.Object, syncPolicy, "spec", "syncPolicy")
	}
	return app
}

func served(t *testing.T, app *unstructured.Unstructured, opts ...Option) dynamic.ResourceInterface {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ApplicationGVR(): "ApplicationList"}, app)
	Serve(client, opts...)
	return client.Resource(ApplicationGVR()).Namespace("argocd")
}

func applySwitch(t *testing.T, apps dynamic.ResourceInterface, manager string, enabled *bool, dryRun bool) error {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("argoproj.io/v1alpha1")
	obj.SetKind("Application")
	obj.SetNamespace("argocd")
	obj.SetName("checkout")
	if enabled != nil {
		require.NoError(t, unstructured.SetNestedField(obj.Object, *enabled, switchPath()...))
	}
	options := metav1.ApplyOptions{FieldManager: manager, Force: true}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	_, err := apps.Apply(context.Background(), "checkout", obj, options)
	return err
}

func read(t *testing.T, apps dynamic.ResourceInterface) (*unstructured.Unstructured, []string) {
	t.Helper()
	app, err := apps.Get(context.Background(), "checkout", metav1.GetOptions{})
	require.NoError(t, err)
	var owners []string
	for _, owner := range switchOwners(app) {
		owners = append(owners, owner.manager+"/"+string(owner.operation))
	}
	return app, owners
}

func ptr(b bool) *bool { return &b }

func TestServe_AnApplyCreatesAndGivesUpTheMapsItMade(t *testing.T) {
	apps := served(t, application(nil))
	require.NoError(t, applySwitch(t, apps, "gitops-reverser", ptr(false), false))
	app, owners := read(t, apps)
	enabled, _, _ := unstructured.NestedBool(app.Object, switchPath()...)
	assert.False(t, enabled)
	assert.Equal(t, []string{"gitops-reverser/Apply"}, owners)

	require.NoError(t, applySwitch(t, apps, "gitops-reverser", nil, false))
	app, owners = read(t, apps)
	_, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "syncPolicy")
	assert.False(t, found, "the maps the apply made go with the switch")
	assert.Empty(t, owners)
}

func TestServe_AMapSomebodyElseWroteStays(t *testing.T) {
	apps := served(t, application(map[string]any{"automated": map[string]any{}}))
	require.NoError(t, applySwitch(t, apps, "gitops-reverser", ptr(false), false))
	require.NoError(t, applySwitch(t, apps, "gitops-reverser", nil, false))
	app, _ := read(t, apps)
	automated, found, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	assert.True(t, found)
	assert.Empty(t, automated, "automated: {} stays, which is automated sync on")
}

func TestServe_OwnershipFollowsWhatChanged(t *testing.T) {
	apps := served(t, application(map[string]any{"automated": map[string]any{"prune": true}}))
	require.NoError(t, applySwitch(t, apps, "gitops-reverser", ptr(false), false))

	merge := []byte(`{"spec":{"syncPolicy":{"automated":{"enabled":false}}}}`)
	_, err := apps.Patch(context.Background(), "checkout", types.MergePatchType, merge,
		metav1.PatchOptions{FieldManager: "argocd-server"})
	require.NoError(t, err)
	_, owners := read(t, apps)
	assert.Equal(t, []string{"gitops-reverser/Apply"}, owners, "an update of the same value takes nothing")

	require.NoError(t, applySwitch(t, apps, "other", ptr(false), false))
	_, owners = read(t, apps)
	assert.Equal(t, []string{"gitops-reverser/Apply", "other/Apply"}, owners, "an apply of the same value shares")

	_, err = apps.Patch(
		context.Background(),
		"checkout",
		types.MergePatchType,
		[]byte(
			`{"spec":{"syncPolicy":{"automated":{"enabled":true}}}}`,
		),
		metav1.PatchOptions{FieldManager: "argocd-server"},
	)
	require.NoError(t, err)
	_, owners = read(t, apps)
	assert.Equal(t, []string{"argocd-server/Update"}, owners, "another value takes it from every owner")

	_, err = apps.Patch(context.Background(), "checkout", types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"team":"a"}},"spec":{"syncPolicy":{"automated":{"enabled":null}}}}`),
		metav1.PatchOptions{})
	require.NoError(t, err)
	app, owners := read(t, apps)
	_, found, _ := unstructured.NestedFieldNoCopy(app.Object, switchPath()...)
	assert.False(t, found, "null removes it")
	assert.Empty(t, owners)
	assert.Equal(t, map[string]string{"team": "a"}, app.GetLabels(), "the rest of a merge patch is merged")
}

func TestServe_ADryRunWritesNothing(t *testing.T) {
	apps := served(t, application(nil))
	require.NoError(t, applySwitch(t, apps, "gitops-reverser", ptr(false), true))
	app, owners := read(t, apps)
	_, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "syncPolicy")
	assert.False(t, found)
	assert.Empty(t, owners)
}

func TestServe_WithoutSwitch(t *testing.T) {
	apps := served(t, application(nil), WithoutSwitch())

	err := applySwitch(t, apps, "gitops-reverser", ptr(false), false)
	require.Error(t, err, "a server-side apply of an undeclared field is refused")
	assert.True(t, apierrors.IsInternalError(err))
	assert.Contains(t, err.Error(), "field not declared in schema")
	app, _ := read(t, apps)
	_, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "syncPolicy")
	assert.False(t, found, "nothing is written")

	_, err = apps.Patch(context.Background(), "checkout", types.MergePatchType,
		[]byte(`{"spec":{"syncPolicy":{"automated":{"enabled":false}}}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	app, _ = read(t, apps)
	automated, found, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	assert.True(t, found)
	assert.Empty(t, automated, "a merge patch is pruned, and leaves automated: {}")
}

func TestOwnSwitch(t *testing.T) {
	app := OwnSwitch(t, application(nil), true, "kustomize-controller", metav1.ManagedFieldsOperationApply)
	enabled, _, _ := unstructured.NestedBool(app.Object, switchPath()...)
	assert.True(t, enabled)
	assert.Equal(t, []owner{{manager: "kustomize-controller", operation: metav1.ManagedFieldsOperationApply}},
		switchOwners(app))
}
