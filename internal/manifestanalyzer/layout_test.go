// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"

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

// A self-contained folder with one supported kustomization resolves to that root, and to
// KustomizeRoot rather than KustomizeOverlay: it renders nothing it may not write to.
func TestResolveLayout_SingleKustomizationRoot(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"kustomization.yaml": layoutRootWithNamespace,
		"web.yaml":           layoutWebDoc,
	})

	got := ResolveLayout(store, "")

	assert.Equal(t, LayoutSingleKustomization, got.Reason)
	assert.Equal(t, LayoutModeKustomizeRoot, got.Mode)
	assert.Equal(t, ".", got.RenderRoot)
	assert.Empty(t, got.ReadOnlyBases, "nothing outside the write scope was scanned")
}

// A folder with no kustomization at all resolves to None and to Plain: the ladder falls
// through to a declared template or the canonical path, and no file is registered anywhere.
func TestResolveLayout_NoKustomization(t *testing.T) {
	store := layoutStore(t, map[string]string{"web.yaml": layoutWebDoc})

	got := ResolveLayout(store, "")

	assert.Equal(t, LayoutNone, got.Reason)
	assert.Equal(t, LayoutModePlain, got.Mode)
	assert.Empty(t, got.RenderRoot)
	assert.Empty(t, got.ReadOnlyBases)
}

// The rule this PR ships. A target covering two overlays covers two render roots, so there is
// no single answer: renderRoot is empty rather than an arbitrary pick, and mode is empty
// rather than one of the two roots' answers.
func TestResolveLayout_TwoRenderRootsIsAmbiguous(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"overlays/prod/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-prod\nresources:\n  - cm.yaml\n",
		"overlays/prod/cm.yaml": layoutWebDoc,
		"overlays/test/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-test\nresources:\n  - cm.yaml\n",
		"overlays/test/cm.yaml": layoutWebDoc,
	})

	got := ResolveLayout(store, "")

	assert.Equal(t, LayoutAmbiguous, got.Reason)
	assert.Empty(t, got.RenderRoot, "an ambiguous folder must not report one of its roots as THE root")
	assert.Empty(t, got.Mode, "with two roots there is no single way the folder is written")
	assert.Equal(t, []string{"overlays/prod", "overlays/test"}, got.RenderRoots,
		"the roots are named so the message can say what the folder actually covers")
}

// Render-root scoping: a leaf overlay that reads a base outside spec.path is scanned from the
// common ancestor, so the store holds both kustomizations. Only the one inside the write jail
// is a candidate — read scope is wider than write scope — so the leaf resolves to its own
// single root, and the base it renders is reported as read-only rather than as a second root.
//
// The base is spelled the way the overlay's own resources: spells it, which is also how the
// write-boundary refusal names it.
func TestResolveLayout_BaseOutsideTheWriteJailIsReadOnly(t *testing.T) {
	store := layoutStore(t, map[string]string{
		"base/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - deployment.yaml\n",
		"base/deployment.yaml": layoutWebDoc,
		"overlays/prod/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: shop-prod\nresources:\n  - ../../base\n",
	})

	got := ResolveLayout(store, "overlays/prod")

	assert.Equal(t, LayoutSingleKustomization, got.Reason)
	assert.Equal(t, LayoutModeKustomizeOverlay, got.Mode)
	assert.Equal(t, ".", got.RenderRoot, "the leaf's own root, expressed relative to spec.path")
	assert.Equal(t, []string{"../../base"}, got.ReadOnlyBases)
}

// Mode separates the two kustomize shapes on exactly the condition every write-boundary
// refusal turns on, so the two cannot disagree about which folder is which: an overlay is a
// root that renders something outside its write scope, and nothing else.
func TestResolveLayout_ModeSeparatesOverlayFromSelfContainedRoot(t *testing.T) {
	files := map[string]string{
		"apps/checkout/kustomization.yaml": layoutRootWithNamespace,
		"apps/checkout/web.yaml":           layoutWebDoc,
	}

	scoped := ResolveLayout(layoutStore(t, files), "apps/checkout")

	assert.Equal(t, LayoutModeKustomizeRoot, scoped.Mode,
		"a scan anchored at the folder itself holds nothing above it, so nothing is read-only")
	assert.Empty(t, scoped.ReadOnlyBases)
}
