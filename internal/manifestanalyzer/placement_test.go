// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// placementSnapshot is like sampleClusterSnapshot, but additionally allows core
// Secrets, so a sensitive resource's ByResourceIdentity actually resolves and can
// be exercised by the placement tests below.
func placementSnapshot() typeset.Snapshot {
	return typeset.Snapshot{
		Generation: 1,
		Entries: []typeset.Entry{
			{
				GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				GVR:        schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        schema.GroupVersionKind{Version: "v1", Kind: "Secret"},
				GVR:        schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
				Namespaced: true,
				Allowed:    true,
			},
		},
	}
}

func placementStore(t *testing.T, fsys fstest.MapFS) *ManifestStore {
	t.Helper()
	mapper := typeset.NewSnapshotRegistry(placementSnapshot())
	return BuildStore(context.Background(), fsys, mapper)
}

func configMapYAML(name, namespace string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n", name, namespace)
}

func secretYAML(name, namespace string) string {
	return fmt.Sprintf(
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\nsops:\n  version: \"3\"\n",
		name, namespace,
	)
}

func newConfigMapRequest(name, namespace string) PlacementRequest {
	return PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", namespace, name),
		Kind:       "ConfigMap",
	}
}

func newSecretRequest(name, namespace string) PlacementRequest {
	return PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "secrets", namespace, name),
		Kind:       "Secret",
		Sensitive:  true,
	}
}

func TestLocateNew_EmptyRepo_Canonical(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := req.Identifier.ToGitPath()
	if res.Path != want || res.Source != PlacementSourceCanonical || res.Append {
		t.Fatalf("got %+v, want canonical path %q, no append", res, want)
	}
}

func TestLocateNew_BundleCohort_Appends(t *testing.T) {
	fsys := fstest.MapFS{
		"all.yaml": {Data: []byte(configMapYAML("a", "app") + "---\n" + configMapYAML("b", "app"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != "all.yaml" || !res.Append || res.Source != PlacementSourceInferred {
		t.Fatalf("got %+v, want append to all.yaml via inference", res)
	}
}

func TestLocateNew_SingletonCohort_NewFileBesideSiblings(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/configmap-a.yaml": {Data: []byte(configMapYAML("a", "app"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "overlays/test/cache.yaml"
	if res.Path != want || res.Append || res.Source != PlacementSourceInferred {
		t.Fatalf("got %+v, want a new file %q beside the sibling", res, want)
	}
}

// A sibling whose namespace is inherited from a kustomization's namespace:
// transformer (no metadata.namespace in its own bytes) means a new document
// placed beside it must also omit metadata.namespace — otherwise the write
// would silently break the convention every document in that context follows
// (this is what let an incidental resource sharing the namespace, e.g. a
// cluster-injected ConfigMap, write a namespace: line into a hand-curated
// bundle file in production; see the design doc's Option C test plan, "the new
// file inherits its sibling's NamespaceSource").
func TestLocateNew_SiblingNamespaceInheritedFromKustomize_NewFileOmitsNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {
			Data: []byte("namespace: app\nresources:\n  - configmap-a.yaml\n"),
		},
		"overlays/test/configmap-a.yaml": {
			Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"),
		},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if !res.NamespaceInherited {
		t.Fatalf("got %+v, want NamespaceInherited since the sibling omits metadata.namespace", res)
	}
}

// A sibling with an explicit metadata.namespace (no kustomize context) means a
// new document beside it keeps writing its namespace explicitly too.
func TestLocateNew_SiblingNamespaceExplicit_NewFileKeepsNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/configmap-a.yaml": {Data: []byte(configMapYAML("a", "app"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.NamespaceInherited {
		t.Fatalf("got %+v, want NamespaceInherited false: the sibling writes its namespace explicitly", res)
	}
}

// resolveKustomizeRoot's fallback (no sibling of this type yet) must also flag
// NamespaceInherited when the one kustomization declares a namespace:
// transformer, for the same reason.
func TestLocateNew_KustomizeRootWithNamespaceTransformer_NewFileOmitsNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {
			Data: []byte("namespace: podinfo-test\nresources:\n  - deployment.yaml\n"),
		},
		"overlays/test/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: podinfo-test\n",
		)},
	}
	store := placementStore(t, fsys)
	req := PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "podinfo-test", "debug-toolbox"),
		Kind:       "ConfigMap",
	}

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if !res.NamespaceInherited {
		t.Fatalf("got %+v, want NamespaceInherited since the kustomization sets namespace:", res)
	}
}

func TestLocateNew_Sensitive_NeverJoinsPlaintextBundle(t *testing.T) {
	fsys := fstest.MapFS{
		"all.yaml": {Data: []byte(configMapYAML("a", "app") + "---\n" + configMapYAML("b", "app"))},
	}
	store := placementStore(t, fsys)
	req := newSecretRequest("api-token", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "v1/secrets/app/api-token.sops.yaml"
	if res.Path != want || res.Append || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want the secure canonical SOPS fallback %q", res, want)
	}
}

func TestLocateNew_Sensitive_JoinsSensitiveSiblingDirectory(t *testing.T) {
	fsys := fstest.MapFS{
		"secrets/app/db.sops.yaml": {Data: []byte(secretYAML("db", "app"))},
	}
	store := placementStore(t, fsys)
	req := newSecretRequest("api-token", "app")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "secrets/app/api-token.sops.yaml"
	if res.Path != want || res.Append || res.Source != PlacementSourceInferred {
		t.Fatalf("got %+v, want a new single-doc SOPS file beside the sensitive sibling %q", res, want)
	}
}

func TestLocateNew_TieBreak_SingletonWinsWhenAheadOrTied(t *testing.T) {
	cases := []struct {
		name       string
		singletons int
		bundleSize int
		wantBundle bool
	}{
		{"singleton strictly ahead", 3, 2, false},
		{"tie favours singleton", 2, 2, false},
		{"bundle strictly ahead", 2, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for i := range tc.singletons {
				fsys[fmt.Sprintf("solo-%d.yaml", i)] = &fstest.MapFile{
					Data: []byte(configMapYAML(fmt.Sprintf("solo-%d", i), "app")),
				}
			}
			var bundle strings.Builder
			for i := range tc.bundleSize {
				if i > 0 {
					bundle.WriteString("---\n")
				}
				bundle.WriteString(configMapYAML(fmt.Sprintf("bundled-%d", i), "app"))
			}
			if tc.bundleSize > 0 {
				fsys["bundle.yaml"] = &fstest.MapFile{Data: []byte(bundle.String())}
			}

			store := placementStore(t, fsys)
			res, err := LocateNew(store, nil, newConfigMapRequest("new", "app"))
			if err != nil {
				t.Fatalf("LocateNew: %v", err)
			}
			if got := res.Path == "bundle.yaml"; got != tc.wantBundle {
				t.Fatalf("path = %q (append=%v), wantBundle=%v", res.Path, res.Append, tc.wantBundle)
			}
		})
	}
}

func TestLocateNew_DeclaredOutranksInferred(t *testing.T) {
	fsys := fstest.MapFS{
		"all.yaml": {Data: []byte(configMapYAML("a", "app"))},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{
		Normal: PlacementPolicyClass{
			ByType: map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
		},
	}

	res, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "app/configmaps.yaml"
	if res.Path != want || res.Source != PlacementSourceDeclared {
		t.Fatalf("got %+v, want the declared template %q to win over inference", res, want)
	}
}

func TestLocateNew_Step2_NewNamespaceUnderPerNamespaceBundle_FallsToCanonical(t *testing.T) {
	fsys := fstest.MapFS{
		"ns1/configmaps.yaml": {
			Data: []byte(
				configMapYAML("a", "ns1") + "---\n" + configMapYAML("b", "ns1") + "---\n" + configMapYAML("c", "ns1"),
			),
		},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "ns2")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	// P4: a per-namespace-segmented bundle must not be guessed for an unseen
	// namespace; the new namespace's ConfigMap must fall through to canonical,
	// never land in ns1/configmaps.yaml.
	if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want canonical fallback (never ns1's bundle)", res)
	}
}

func TestLocateNew_Step2_NamespaceAgnosticBundle_IsReused(t *testing.T) {
	fsys := fstest.MapFS{
		"all.yaml": {Data: []byte(configMapYAML("a", "ns1") + "---\n" + configMapYAML("b", "ns2"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "ns3")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != "all.yaml" || !res.Append || res.Source != PlacementSourceInferred {
		t.Fatalf("got %+v, want the namespace-agnostic bundle reused for the new namespace", res)
	}
}

func TestLocateNew_Step2_NewNamespaceUnderPerNamespaceDirectories_FallsToCanonical(t *testing.T) {
	// Two distinct singleton directories, one per namespace, is what proves a
	// per-namespace-segmented convention (P4) — a single existing directory would
	// be indistinguishable from coincidence, so this needs at least two.
	fsys := fstest.MapFS{
		"ns1/configmap-a.yaml": {Data: []byte(configMapYAML("a", "ns1"))},
		"ns2/configmap-b.yaml": {Data: []byte(configMapYAML("b", "ns2"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "ns3")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want canonical fallback, never ns1/ or ns2/ for a resource in ns3", res)
	}
}

func TestLocateNew_SensitiveCollision_Errors(t *testing.T) {
	// The existing file already occupies exactly the path the declared template
	// will render for the new resource (a misconfigured template lacking {name}
	// would produce this in practice); LocateNew must refuse to append a sensitive
	// document onto it rather than silently colliding two identities.
	fsys := fstest.MapFS{
		"secrets/app/api-token-2.sops.yaml": {Data: []byte(secretYAML("other", "app"))},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{
		Sensitive: PlacementPolicyClass{
			ByType: map[string]string{"v1/secrets": "secrets/{namespace}/{name}.sops.yaml"},
		},
	}

	_, err := LocateNew(store, policy, newSecretRequest("api-token-2", "app"))
	if err == nil {
		t.Fatalf("expected an error placing a second identity onto the same sensitive path")
	}
}

func TestLocateNew_KustomizationEntryDetected(t *testing.T) {
	kustYAML := "namespace: podinfo-test\nresources:\n  - deployment.yaml\n"
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {Data: []byte(kustYAML)},
		"overlays/test/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: podinfo-test\n",
		)},
	}
	store := placementStore(t, fsys)
	req := PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "podinfo-test", "debug-toolbox"),
		Kind:       "ConfigMap",
	}

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Kustomization == nil {
		t.Fatalf("got %+v, want a Kustomization entry to add since the overlay carries one", res)
	}
	if res.Kustomization.Path != "overlays/test/kustomization.yaml" {
		t.Errorf("Kustomization.Path = %q, want overlays/test/kustomization.yaml", res.Kustomization.Path)
	}
}

func TestLocateNew_KustomizationAlreadyListed_NoEntryNeeded(t *testing.T) {
	kustYAML := "namespace: podinfo-test\nresources:\n  - deployment.yaml\n  - debug-toolbox.yaml\n"
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {Data: []byte(kustYAML)},
		"overlays/test/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: podinfo-test\n",
		)},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{
		Normal: PlacementPolicyClass{Default: "overlays/test/debug-toolbox.yaml"},
	}
	req := PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "podinfo-test", "debug-toolbox"),
		Kind:       "ConfigMap",
	}

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Kustomization != nil {
		t.Fatalf("got %+v, want no Kustomization entry since debug-toolbox.yaml is already listed", res)
	}
}

func TestLocateNew_KustomizationUnsupported_NeverEdited(t *testing.T) {
	kustYAML := "namespace: podinfo-test\nresources:\n  - deployment.yaml\nhelmCharts:\n  - name: x\n"
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {Data: []byte(kustYAML)},
	}
	store := placementStore(t, fsys)
	req := PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "podinfo-test", "debug-toolbox"),
		Kind:       "ConfigMap",
	}

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Kustomization != nil {
		t.Fatalf("got %+v, want an unsupported kustomization never surfaced for editing", res)
	}
}

func TestLocateNew_BatchOrderIndependence(t *testing.T) {
	fsys := fstest.MapFS{
		"solo.yaml": {Data: []byte(configMapYAML("solo", "app"))},
	}
	// The store snapshot is built once (the batch's pre-plan snapshot) and never
	// mutated; resolving two hypothetical new siblings against it must not depend on
	// which is resolved first (P2) — neither becomes the other's sibling.
	store := placementStore(t, fsys)

	first, err := LocateNew(store, nil, newConfigMapRequest("alpha", "app"))
	if err != nil {
		t.Fatalf("LocateNew(alpha): %v", err)
	}
	second, err := LocateNew(store, nil, newConfigMapRequest("beta", "app"))
	if err != nil {
		t.Fatalf("LocateNew(beta): %v", err)
	}

	storeAgain := placementStore(t, fsys)
	secondFirst, err := LocateNew(storeAgain, nil, newConfigMapRequest("beta", "app"))
	if err != nil {
		t.Fatalf("LocateNew(beta) reordered: %v", err)
	}
	firstSecond, err := LocateNew(storeAgain, nil, newConfigMapRequest("alpha", "app"))
	if err != nil {
		t.Fatalf("LocateNew(alpha) reordered: %v", err)
	}

	if first.Path != firstSecond.Path || second.Path != secondFirst.Path {
		t.Fatalf("resolution order changed the result: %q/%q vs %q/%q",
			first.Path, second.Path, firstSecond.Path, secondFirst.Path)
	}
	if first.Path == second.Path {
		t.Fatalf("alpha and beta must not collide onto the same new file: %q", first.Path)
	}
}

func TestRenderPlacementTemplate(t *testing.T) {
	vars := map[string]string{
		"group": "", "groupPath": "", "version": "v1", "apiVersion": "v1",
		"resource": "configmaps", "kind": "ConfigMap", "scope": "namespaced",
		"namespace": "default", "namespaceOrCluster": "default", "name": "app",
		"sensitiveSuffix": ".yaml",
	}
	got, err := RenderPlacementTemplate("{groupPath}/{version}/{resource}/{namespace}/{name}.yaml", vars)
	if err != nil {
		t.Fatalf("RenderPlacementTemplate: %v", err)
	}
	if want := "v1/configmaps/default/app.yaml"; got != want {
		t.Errorf("got %q, want %q (empty groupPath segment collapsed)", got, want)
	}
}

func TestRenderPlacementTemplate_UnknownVariable(t *testing.T) {
	_, err := RenderPlacementTemplate("{namespace}/{bogus}.yaml", map[string]string{"namespace": "default"})
	if err == nil {
		t.Fatalf("expected an error for an unknown template variable")
	}
}

func TestRenderPlacementTemplate_SanitizesSlashInValue(t *testing.T) {
	got, err := RenderPlacementTemplate("{namespace}/{name}.yaml", map[string]string{
		"namespace": "default", "name": "weird/name",
	})
	if err != nil {
		t.Fatalf("RenderPlacementTemplate: %v", err)
	}
	if want := "default/weird%2Fname.yaml"; got != want {
		t.Errorf("got %q, want %q (slash percent-encoded, not a path separator)", got, want)
	}
}

func TestIdentityCompletePlacementTemplate(t *testing.T) {
	cases := []struct {
		name              string
		tmpl              string
		narrowedToOneType bool
		want              bool
	}{
		{"full identity", "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml", false, true},
		{"missing resource for default", "{groupPath}/{version}/{namespaceOrCluster}/{name}.yaml", false, false},
		{"narrowed type needs only scope+name", "{namespace}/secret-{name}.sops.yaml", true, true},
		{"narrowed type missing name", "{namespace}/secret.sops.yaml", true, false},
		{"narrowed type missing scope", "secret-{name}.sops.yaml", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityCompletePlacementTemplate(tc.tmpl, tc.narrowedToOneType); got != tc.want {
				t.Errorf("IdentityCompletePlacementTemplate(%q, %v) = %v, want %v",
					tc.tmpl, tc.narrowedToOneType, got, tc.want)
			}
		})
	}
}

func TestPlacementTypeKey(t *testing.T) {
	if got := PlacementTypeKey("", "v1", "secrets"); got != "v1/secrets" {
		t.Errorf("core key = %q, want v1/secrets", got)
	}
	if got := PlacementTypeKey("apps", "v1", "deployments"); got != "apps/v1/deployments" {
		t.Errorf("grouped key = %q, want apps/v1/deployments", got)
	}
}
