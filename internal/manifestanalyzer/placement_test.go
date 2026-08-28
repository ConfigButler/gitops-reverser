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

// Every layout sibling inference used to read, in one table, all resolving to the
// canonical path. This is the deletion's contract, stated as the behaviour rather than
// as an absence: the destination of a new document depends on the GitTarget's
// declaration and on whether the folder has one kustomize root — never on where the
// repository happens to keep the OTHER documents of the same type.
//
// The last two rows are the ones the ladder used to accept, and they are the cost the
// deletion is paying deliberately (docs/design/open-asks-priority.md, "The cost, stated
// plainly"): a bundle or a directory that had already proven itself namespace-agnostic
// was extended. It is not extended now. A `placement.byType` line is how a repository
// asks for either, and it is now the only way.
func TestLocateNew_LayoutsThatUsedToBeInferred_AllResolveCanonical(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		namespace string
	}{
		{
			name:      "a bundle of the same type in the same namespace",
			files:     map[string]string{"all.yaml": configMapYAML("a", "app") + "---\n" + configMapYAML("b", "app")},
			namespace: "app",
		},
		{
			name:      "one document per file, same type and namespace",
			files:     map[string]string{"overlays/test/configmap-a.yaml": configMapYAML("a", "app")},
			namespace: "app",
		},
		{
			name: "a per-namespace bundle, for an unseen namespace",
			files: map[string]string{
				"ns1/configmaps.yaml": configMapYAML("a", "ns1") + "---\n" + configMapYAML("b", "ns1"),
			},
			namespace: "ns2",
		},
		{
			name: "a directory per namespace, for an unseen namespace",
			files: map[string]string{
				"ns1/configmap-a.yaml": configMapYAML("a", "ns1"),
				"ns2/configmap-b.yaml": configMapYAML("b", "ns2"),
			},
			namespace: "ns3",
		},
		{
			name:      "one directory holding one namespace, for an unseen namespace",
			files:     map[string]string{"ns1/configmap-a.yaml": configMapYAML("a", "ns1")},
			namespace: "ns2",
		},
		{
			name:      "a bundle that already spans two namespaces",
			files:     map[string]string{"all.yaml": configMapYAML("a", "ns1") + "---\n" + configMapYAML("b", "ns2")},
			namespace: "ns3",
		},
		{
			name: "a shared directory that already spans two namespaces",
			files: map[string]string{
				"shared/configmap-a.yaml": configMapYAML("a", "ns1"),
				"shared/configmap-b.yaml": configMapYAML("b", "ns2"),
			},
			namespace: "ns3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for path, body := range tc.files {
				fsys[path] = &fstest.MapFile{Data: []byte(body)}
			}
			store := placementStore(t, fsys)
			req := newConfigMapRequest("cache", tc.namespace)

			res, err := LocateNew(store, nil, req)
			if err != nil {
				t.Fatalf("LocateNew: %v", err)
			}
			if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
				t.Fatalf("got %+v, want the canonical path %q", res, req.Identifier.ToGitPath())
			}
			if res.Append {
				t.Fatalf("got %+v, want a file of its own: no existing document's file is ever joined", res)
			}
		})
	}
}

// The production shape of the cascade the deletion retires, kept as its own named test
// because it is the failure the argument rests on. Objects that exist under the SAME NAME
// in every namespace (kube-root-ca.crt is in all of them) made the inferred path collide
// exactly with the first namespace's file, so the second namespace's object was appended
// as an extra document. That file then genuinely spanned two namespaces, so every later
// object of the type legitimately preferred the bundle and the whole type collapsed into
// one file — one wrong inference cascading into total collapse. With no ladder there is
// no first wrong step to cascade from.
func TestLocateNew_SameNameInANewNamespace_NeverLandsOnTheFirstNamespacesFile(t *testing.T) {
	fsys := fstest.MapFS{
		"ns1/configmaps/kube-root-ca.crt.yaml": {Data: []byte(configMapYAML("kube-root-ca.crt", "ns1"))},
	}
	store := placementStore(t, fsys)
	req := newConfigMapRequest("kube-root-ca.crt", "ns2")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Append || res.Path == "ns1/configmaps/kube-root-ca.crt.yaml" {
		t.Fatalf("ns2's object was filed onto ns1's own file: %+v", res)
	}
	if res.Path != req.Identifier.ToGitPath() || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want canonical fallback carrying ns2's own namespace segment", res)
	}
}

// A folder whose one kustomization sets a namespace: transformer for this resource's own
// namespace means the new document must omit metadata.namespace — the build context
// supplies it, and repeating it would break the convention every document in that
// context follows. The destination here comes from there being ONE kustomize root, not
// from the sibling: what the sibling's own bytes look like no longer enters into it.
func TestLocateNew_KustomizeContextNamespace_NewFileOmitsNamespace(t *testing.T) {
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

// No kustomize context means no inherited namespace: the document carries its own
// metadata.namespace, because nothing else will supply it.
func TestLocateNew_NoKustomizeContext_NewFileKeepsNamespace(t *testing.T) {
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
		t.Fatalf("got %+v, want NamespaceInherited false: no build context supplies a namespace", res)
	}
}

// The safety half of the inheritance rule, and the case the old kustomize-root path got
// wrong: a kustomization whose namespace: transformer names a DIFFERENT namespace than the
// resource's own must not make the write omit metadata.namespace. Omitting it would hand
// the namespace to kustomize, which renders the document as an object in the
// transformer's namespace — a different object than the one being mirrored. The explicit
// line stays, and the render oracle reports the folder as unable to express this object.
func TestLocateNew_KustomizeContextNamespaceMismatch_NewFileKeepsItsOwnNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {
			Data: []byte("namespace: other\nresources:\n  - configmap-a.yaml\n"),
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
	if res.NamespaceInherited {
		t.Fatalf("got %+v, want the namespace written explicitly: the transformer names another namespace", res)
	}
}

// A DECLARED template pointing into a governed directory carries the same obligation as
// the kustomize-root fallback: the context supplies the namespace, so the file must not
// repeat it. Before the Option C deletion this only ever came out of a sibling's own
// bytes, so a declared placement silently wrote a namespace: line the folder omits.
func TestLocateNew_DeclaredIntoKustomizeContext_OmitsNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {
			Data: []byte("namespace: app\nresources:\n  - configmap-a.yaml\n"),
		},
		"overlays/test/configmap-a.yaml": {
			Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"),
		},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{Default: "overlays/test/{name}.yaml"}

	res, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Source != PlacementSourceDeclared {
		t.Fatalf("expected a declared placement, got %s", res.Source)
	}
	if !res.NamespaceInherited {
		t.Fatalf("got %+v, want NamespaceInherited: the declared path lands in a namespaced context", res)
	}
}

// resolveKustomizeRoot's fallback must flag NamespaceInherited when the one
// kustomization declares a namespace: transformer for this resource's namespace.
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

// A sensitive resource gets no sibling reuse either: with the ladder gone, the existing
// .sops.yaml directory does not attract the new Secret, and the canonical SOPS path — which
// is identity-complete by construction — is what it gets. A repository that wants its
// secrets kept together says so with one placement.byType line.
func TestLocateNew_Sensitive_ExistingSopsDirectoryIsNotReused(t *testing.T) {
	fsys := fstest.MapFS{
		"secrets/app/db.sops.yaml": {Data: []byte(secretYAML("db", "app"))},
	}
	store := placementStore(t, fsys)
	req := newSecretRequest("api-token")

	res, err := LocateNew(store, nil, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	want := "app/secrets/api-token.sops.yaml"
	if res.Path != want || res.Append || res.Source != PlacementSourceCanonical {
		t.Fatalf("got %+v, want the canonical SOPS path %q", res, want)
	}
}

func TestLocateNew_DeclaredOutranksTheKustomizeRoot(t *testing.T) {
	fsys := fstest.MapFS{
		"overlays/test/kustomization.yaml": {Data: []byte("resources:\n  - configmap-a.yaml\n")},
		"overlays/test/configmap-a.yaml":   {Data: []byte(configMapYAML("a", "app"))},
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
		t.Fatalf("got %+v, want the declared template %q to win over the kustomize root", res, want)
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

// A declared template resolving onto a file the writer cannot vouch for — one holding a
// document tolerated despite a non-editable construct (a YAML anchor) — must not be
// appended to. Append stays false and the caller falls through to writeWholeFile, whose
// multi-document guard refuses rather than overwriting. This used to be enforced twice,
// once for cohort candidacy and once here; the append gate is now the only place, so it is
// worth pinning directly.
func TestLocateNew_DeclaredOntoTaintedFile_NeverAppends(t *testing.T) {
	tainted := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: anchored\n  namespace: app\n" +
		"data: &d\n  color: blue\nextra:\n  <<: *d\n"
	fsys := fstest.MapFS{
		"tainted.yaml": {Data: []byte(tainted)},
	}
	store := placementStore(t, fsys)
	policy := &PlacementPolicy{Default: "tainted.yaml"}

	res, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if res.Path != "tainted.yaml" {
		t.Fatalf("got %+v, want the declared path honoured", res)
	}
	if res.Append {
		t.Fatalf("got %+v, want Append false: the file holds a document the writer cannot account for", res)
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
		{
			"versionless canonical shape",
			"{namespaceOrCluster}/{groupPath}/{resource}/{name}.yaml",
			false,
			true,
		},
		{"missing resource for default", "{groupPath}/{version}/{namespaceOrCluster}/{name}.yaml", false, false},
		{"missing group for default", "{version}/{resource}/{namespaceOrCluster}/{name}.yaml", false, false},
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
