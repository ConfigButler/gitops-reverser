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

func newSecretRequest(name string) PlacementRequest {
	return PlacementRequest{
		Identifier: types.NewResourceIdentifier("", "v1", "secrets", "app", name),
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

// With render-root scoping the scan is re-rooted at renderBase, so a canonical path resolves
// outside spec.path. WriteScope rebases it back under the write jail rather than letting it
// escape (and be skipped) — placement stays relative to spec.path as documented.
func TestLocateNew_WriteScope_RebasesCanonicalIntoJail(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	req := newConfigMapRequest("cache", "app")
	req.WriteScope = "overlays/production"

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "overlays/production/" + newConfigMapRequest("cache", "app").Identifier.ToGitPath()
	if res.Path != want {
		t.Fatalf("got %q, want the canonical path rebased under the jail %q", res.Path, want)
	}
	if !pathWithin(res.Path, "overlays/production") {
		t.Fatalf("resolved path %q escaped the write jail", res.Path)
	}
}

// A declared placement template is likewise rebased under the jail, so a GitTarget's declared
// layout lands inside spec.path for an overlay instead of at renderBase's root (where it would
// have been silently skipped).
func TestLocateNew_WriteScope_RebasesDeclared(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	req := newConfigMapRequest("cache", "app")
	req.WriteScope = "overlays/production"
	policy := &PlacementPolicy{Default: "{namespace}/configmaps.yaml"}

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Source != PlacementSourceDeclared {
		t.Fatalf("expected a declared placement, got %s", res.Source)
	}
	if res.Path != "overlays/production/app/configmaps.yaml" {
		t.Fatalf("got %q, want the declared path rebased under the jail", res.Path)
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
	req := newSecretRequest("api-token")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "app/secrets/api-token.sops.yaml"
	if res.Path != want || res.Append || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want the secure canonical SOPS fallback %q", res, want)
	}
}

func TestLocateNew_Sensitive_JoinsSensitiveSiblingDirectory(t *testing.T) {
	fsys := fstest.MapFS{
		"secrets/app/db.sops.yaml": {Data: []byte(secretYAML("db", "app"))},
	}
	store := placementStore(t, fsys)
	req := newSecretRequest("api-token")

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
		ByType: map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
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
		ByType: map[string]string{"v1/secrets": "secrets/{namespace}/{name}.sops.yaml"},
	}

	_, err := LocateNew(store, policy, newSecretRequest("api-token-2"))
	if err == nil {
		t.Fatalf("expected an error placing a second identity onto the same sensitive path")
	}
}

// Under Option B2 the single declared map is consulted for sensitive and normal
// resources alike, so a plaintext resource can be routed onto a path that already
// holds an encrypted document. finishPlacement must refuse that rather than
// append the cleartext beside SOPS data (or fall through to a whole-file
// overwrite that would destroy the encrypted document) — the write-time guard
// that replaces B1's structural sensitive/normal split.
func TestLocateNew_PlaintextOntoEncryptedFile_Refused(t *testing.T) {
	// The analyzer classifies a document as encrypted only for a ".sops.yaml"/
	// ".sops.yml" file carrying a sops: key, so the fixture must use that name.
	fsys := fstest.MapFS{
		"bundle.sops.yaml": {Data: []byte(secretYAML("db", "app"))},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{Default: "bundle.sops.yaml"}

	_, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err == nil {
		t.Fatalf("expected a refusal placing a plaintext resource onto an encrypted file")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("error should name the encrypted-file conflict, got: %v", err)
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
		Default: "overlays/test/debug-toolbox.yaml",
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

func TestPlacementVars_GroupedClusterScoped(t *testing.T) {
	req := PlacementRequest{
		Identifier: types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", "admin"),
		Kind:       "ClusterRole",
	}
	vars := placementVars(req)
	if vars["scope"] != "cluster" || vars["namespaceOrCluster"] != "_cluster" {
		t.Errorf("got scope=%q namespaceOrCluster=%q, want scope=\"cluster\" (descriptor) and "+
			"namespaceOrCluster=\"_cluster\" (illegal-namespace sentinel) for a cluster-scoped resource",
			vars["scope"], vars["namespaceOrCluster"])
	}
	if want := "rbac.authorization.k8s.io/v1"; vars["apiVersion"] != want {
		t.Errorf("apiVersion = %q, want %q for a grouped resource", vars["apiVersion"], want)
	}
}

// A sensitive and a normal document of the SAME type (e.g. one ConfigMap
// encrypted as .sops.yaml, one plain) must not be conflated: cohortMembers must
// skip the mismatched-sensitivity sibling rather than only relying on the type
// filter (which cannot tell them apart, since sensitivity is an encryption fact,
// not a type fact).
func TestLocateNew_MixedSensitivityConfigMapsInSameNamespace_NeverConflated(t *testing.T) {
	fsys := fstest.MapFS{
		"normal.yaml": {Data: []byte(configMapYAML("a", "app") + "---\n" + configMapYAML("b", "app"))},
		"secret.sops.yaml": {
			Data: []byte(
				"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sensitive-cm\n  namespace: app\nsops:\n  version: \"3\"\n",
			),
		},
	}
	store := placementStore(t, fsys)

	res, err := LocateNew(store, nil, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "normal.yaml"
	if res.Path != want || !res.Append {
		t.Fatalf("got %+v, want the new normal ConfigMap appended to its normal bundle %q, "+
			"never to the encrypted sibling", res, want)
	}
}

// A file tolerated despite a non-editable construct (e.g. a YAML anchor) must
// never be joined — classifyCohortLocations excludes it from both bundle and
// singleton candidacy, so a genuinely new sibling falls through past it instead
// of silently landing beside content the writer cannot vouch for.
func TestLocateNew_TaintedSiblingNeverJoined(t *testing.T) {
	tainted := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: anchored\n  namespace: app\n" +
		"data: &d\n  color: blue\nextra:\n  <<: *d\n"
	fsys := fstest.MapFS{
		"tainted.yaml": {Data: []byte(tainted)},
	}
	store := placementStore(t, fsys)

	res, err := LocateNew(store, nil, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path == "tainted.yaml" || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want the tainted file excluded and canonical fallback used", res)
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

func TestValidPlacementTemplateSyntax(t *testing.T) {
	if err := ValidPlacementTemplateSyntax("{namespace}/{name}.yaml"); err != nil {
		t.Errorf("a template built only from known variables must be valid: %v", err)
	}
	if err := ValidPlacementTemplateSyntax("{namespace}/{bogus}.yaml"); err == nil {
		t.Errorf("expected an error for the unknown variable {bogus}")
	}
}

// Two supported kustomizations under the scanned root is ambiguous: neither can
// safely be assumed to be "the one" the GitTarget is about, so a genuinely new
// type falls through to canonical rather than guessing.
func TestLocateNew_KustomizeRoot_AmbiguousWithTwoSupported(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/a/kustomization.yaml": {Data: []byte("namespace: a\nresources:\n  - deployment.yaml\n")},
		"overlays/a/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: a\n",
		)},
		"overlays/b/kustomization.yaml": {Data: []byte("namespace: b\nresources:\n  - deployment.yaml\n")},
		"overlays/b/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: b\n",
		)},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("cache", "a")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want canonical fallback: two supported kustomizations is ambiguous", res)
	}
}

// The kustomize-root fallback must also work for a sensitive resource: no
// existing sibling of that type, exactly one supported kustomization, no
// namespace: transformer set (so the sensitive path keeps its explicit namespace).
func TestLocateNew_KustomizeRootSensitive(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {Data: []byte("resources:\n  - deployment.yaml\n")},
		"overlays/test/deployment.yaml": {Data: []byte(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: app\n",
		)},
	}
	store := placementStore(t, fsys)
	req := newSecretRequest("api-token")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "overlays/test/api-token.sops.yaml"
	if res.Path != want || res.NamespaceInherited {
		t.Fatalf("got %+v, want %q with no namespace transformer set", res, want)
	}
}

// A declared template with an unknown variable is a misconfiguration LocateNew
// must not crash or write on; it falls through to sibling inference / canonical,
// exactly as if no declared template had matched.
func TestLocateNew_DeclaredTemplateUnknownVariable_FallsThrough(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{
		Default: "{bogus}/all.yaml",
	}
	req := newConfigMapRequest("cache", "app")

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want canonical fallback when the declared template is invalid", res)
	}
}

func TestFileIsAppendSafe(t *testing.T) {
	if fileIsAppendSafe(nil) {
		t.Error("a nil FileModel must never be append-safe")
	}
	clean := &FileModel{Documents: []*DocumentModel{{Cause: DocumentCause{Kind: CauseNone}}}}
	if !fileIsAppendSafe(clean) {
		t.Error("a file with only cleanly editable documents must be append-safe")
	}
	sensitive := &FileModel{Documents: []*DocumentModel{{Cause: DocumentCause{Kind: CauseEncrypted}}}}
	if !fileIsAppendSafe(sensitive) {
		t.Error("an ordinary encrypted document must not be treated as tainted")
	}
	tainted := &FileModel{Documents: []*DocumentModel{
		{Cause: DocumentCause{Kind: CauseNone}},
		{Cause: DocumentCause{Kind: CauseNonEditable}},
	}}
	if fileIsAppendSafe(tainted) {
		t.Error("a file holding a non-editable (e.g. anchor-using) document must never be append-safe")
	}
}

func TestSpansMultipleNamespaces(t *testing.T) {
	if spansMultipleNamespaces(nil) {
		t.Error("no members cannot span multiple namespaces")
	}
	unresolved := []*DocumentModel{{ResourceIdentity: nil}}
	if spansMultipleNamespaces(unresolved) {
		t.Error("a document with no resolved ResourceIdentity contributes no namespace")
	}
	oneNamespace := []*DocumentModel{
		{ResourceIdentity: &types.ResourceIdentifier{Namespace: "a"}},
		{ResourceIdentity: &types.ResourceIdentifier{Namespace: "a"}},
	}
	if spansMultipleNamespaces(oneNamespace) {
		t.Error("members sharing one namespace do not span multiple namespaces")
	}
	twoNamespaces := []*DocumentModel{
		{ResourceIdentity: &types.ResourceIdentifier{Namespace: "a"}},
		{ResourceIdentity: &types.ResourceIdentifier{Namespace: "b"}},
	}
	if !spansMultipleNamespaces(twoNamespaces) {
		t.Error("members in two distinct namespaces must span multiple namespaces")
	}
}

func TestValidateResolvedPlacementPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"clean relative yaml", "overlays/test/cache.yaml", true},
		{"clean relative yml", "overlays/test/cache.yml", true},
		{"sops path is a yaml path too", "secrets/app/db.sops.yaml", true},
		{"empty", "", false},
		{"parent traversal", "../outside.yaml", false},
		{"nested parent traversal", "overlays/../../outside.yaml", false},
		{"absolute", "/etc/passwd", false},
		{"backslash separator", "overlays\\test\\cache.yaml", false},
		{"not clean (double slash)", "overlays//cache.yaml", false},
		{"no file name", "overlays/test/", false},
		{"bad suffix", "overlays/test/cache.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResolvedPlacementPath(tc.path)
			if (err == nil) != tc.ok {
				t.Errorf("ValidateResolvedPlacementPath(%q) = %v, want ok=%v", tc.path, err, tc.ok)
			}
		})
	}
}

func TestValidPlacementTemplatePath(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		ok   bool
	}{
		{"clean relative", "{namespace}/{name}.yaml", true},
		{"sensitiveSuffix placeholder", "{namespace}/secret-{name}{sensitiveSuffix}", true},
		{"parent traversal", "../outside.yaml", false},
		{"nested parent traversal", "{namespace}/../../outside.yaml", false},
		{"absolute", "/etc/{name}.yaml", false},
		{"backslash", "{namespace}\\{name}.yaml", false},
		{"bad suffix", "{namespace}/{name}.txt", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidPlacementTemplatePath(tc.tmpl)
			if (err == nil) != tc.ok {
				t.Errorf("ValidPlacementTemplatePath(%q) = %v, want ok=%v", tc.tmpl, err, tc.ok)
			}
		})
	}
}

// Defense in depth: even if a path-escaping template somehow reached LocateNew
// (e.g. validation were bypassed, stale, or a future bug), the runtime gate in
// finishPlacement must still refuse to write outside the GitTarget's spec.path,
// exactly like the existing sensitive-collision refusal — skip the resource, not
// escape the folder.
func TestLocateNew_DeclaredTemplateEscapingPath_Refused(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{
		Default: "../../outside.yaml",
	}

	_, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err == nil {
		t.Fatal("expected an error for a declared template that escapes spec.path")
	}
}
