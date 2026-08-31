// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// layoutStore builds a store over an in-memory folder, using the same registry the rest of
// this package's tests resolve types against.
func layoutStore(t *testing.T, files map[string]string) *ManifestStore {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return BuildStore(context.Background(), fsys, typeset.NewSnapshotRegistry(sampleClusterSnapshot()))
}

const layoutRootWithNamespace = "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
	"kind: Kustomization\nnamespace: shop\nresources:\n  - web.yaml\n"

const layoutWebDoc = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\ndata:\n  k: v\n"

// A folder with one supported kustomization resolves to that root, and the root's own
// namespace: transformer is what makes the folder namespace-free: a document placed there
// omits metadata.namespace because something in the repository supplies it.
func TestResolveLayout_SingleKustomizationRoot(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"kustomization.yaml": layoutRootWithNamespace,
		"web.yaml":           layoutWebDoc,
	})

	got := ResolveLayout(store, nil, "", nil)

	assert.Equal(t, LayoutSingleKustomization, got.Reason)
	assert.Equal(t, ".", got.RenderRoot)
	require.NotNil(t, got.SerializeNamespace)
	assert.False(t, *got.SerializeNamespace, "the root supplies namespace: shop, so documents omit it")
}

// The same folder with a root that assigns no namespace resolves the other way: nothing
// supplies the namespace, so every namespaced document carries its own.
func TestResolveLayout_RootWithoutNamespaceSerializesIt(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - web.yaml\n",
		"web.yaml": layoutWebDoc,
	})

	got := ResolveLayout(store, nil, "", nil)

	assert.Equal(t, LayoutSingleKustomization, got.Reason)
	require.NotNil(t, got.SerializeNamespace)
	assert.True(t, *got.SerializeNamespace)
}

// A folder with no kustomization at all resolves to None: the ladder falls through to a
// declared template or the canonical path, and the document carries its namespace.
func TestResolveLayout_NoKustomization(t *testing.T) {
	store := layoutStore(t, map[string]string{"web.yaml": layoutWebDoc})

	got := ResolveLayout(store, nil, "", nil)

	assert.Equal(t, LayoutNone, got.Reason)
	assert.Empty(t, got.RenderRoot)
	require.NotNil(t, got.SerializeNamespace)
	assert.True(t, *got.SerializeNamespace)
}

// The rule this PR ships. A target covering two overlays covers two render roots, and the
// folder-wide questions have no single answer: renderRoot is empty rather than an arbitrary
// pick, and serializeNamespace is absent rather than one of the two roots' answers.
func TestResolveLayout_TwoRenderRootsIsAmbiguous(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"overlays/prod/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-prod\nresources:\n  - cm.yaml\n",
		"overlays/prod/cm.yaml": layoutWebDoc,
		"overlays/test/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-test\nresources:\n  - cm.yaml\n",
		"overlays/test/cm.yaml": layoutWebDoc,
	})

	got := ResolveLayout(store, nil, "", nil)

	assert.Equal(t, LayoutAmbiguous, got.Reason)
	assert.Empty(t, got.RenderRoot, "an ambiguous folder must not report one of its roots as THE root")
	assert.Nil(t, got.SerializeNamespace, "with two roots the answer is per document, not per folder")
	assert.Equal(t, []string{"overlays/prod", "overlays/test"}, got.RenderRoots,
		"the roots are named so the message can say what the folder actually covers")
}

// Render-root scoping: a leaf overlay that reads a base outside spec.path is scanned from the
// common ancestor, so the store holds both kustomizations. Only the one inside the write jail
// is a candidate — read scope is wider than write scope — so the leaf resolves to its own
// single root instead of reporting the base as a second one.
func TestResolveLayout_BaseOutsideTheWriteJailIsNotARoot(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"base/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - deployment.yaml\n",
		"base/deployment.yaml": layoutWebDoc,
		"overlays/prod/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-prod\nresources:\n  - ../../base\n",
	})

	got := ResolveLayout(store, nil, "overlays/prod", nil)

	assert.Equal(t, LayoutSingleKustomization, got.Reason)
	assert.Equal(t, ".", got.RenderRoot, "the leaf's own root, expressed relative to spec.path")
}

// The reported layout and the layout placement takes are one predicate, so an example can
// never claim a destination the writer would not choose. This asserts the pairing directly:
// the example's path is what LocateNew resolves for the same request.
func TestResolveLayout_ExamplesComeFromTheRealLadder(t *testing.T) {
	store := layoutStore(t, map[string]string{"web.yaml": layoutWebDoc})
	policy := &PlacementPolicy{
		ByType:  map[string]string{"v1/secrets": "secrets/{name}.yaml"},
		Default: "{namespace}/{resource}/{name}.yaml",
	}

	got := ResolveLayout(store, policy, "", []string{"v1/secrets", "apps/v1/deployments"})

	require.Len(t, got.Examples, 2)
	assert.Equal(t, "v1/secrets", got.Examples[0].Type)
	assert.Equal(t, "secrets/example.yaml", got.Examples[0].Path)
	assert.Equal(t, PlacementSourceDeclared, got.Examples[0].Source)
	assert.Equal(t, "example/deployments/example.yaml", got.Examples[1].Path,
		"a type with no byType entry falls to the declared default")
}

// The cap is fixed rather than proportional: the stanza must stay bounded however many types
// a target watches, which is the same reason status.streams is counts and not a list.
func TestResolveLayout_ExamplesAreCappedAtThree(t *testing.T) {
	store := layoutStore(t, map[string]string{"web.yaml": layoutWebDoc})

	got := ResolveLayout(store, nil, "", []string{
		"v1/configmaps", "v1/secrets", "apps/v1/deployments", "apps/v1/statefulsets",
	})

	assert.Len(t, got.Examples, layoutExampleCap)
}

func TestParsePlacementTypeKey(t *testing.T) {
	cases := []struct {
		key            string
		ok             bool
		group, version string
		resource       string
	}{
		{key: "v1/configmaps", ok: true, version: "v1", resource: "configmaps"},
		{key: "apps/v1/deployments", ok: true, group: "apps", version: "v1", resource: "deployments"},
		{key: "configmaps", ok: false},
		{key: "a/b/c/d", ok: false},
		{key: "/v1/configmaps", ok: false},
		{key: "", ok: false},
	}
	for _, c := range cases {
		got, ok := ParsePlacementTypeKey(c.key)
		assert.Equal(t, c.ok, ok, "key %q", c.key)
		if !c.ok {
			continue
		}
		assert.Equal(t, c.group, got.Group, "key %q", c.key)
		assert.Equal(t, c.version, got.Version, "key %q", c.key)
		assert.Equal(t, c.resource, got.Resource, "key %q", c.key)
	}
}
