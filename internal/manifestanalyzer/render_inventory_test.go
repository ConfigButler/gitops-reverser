// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"testing"
	"testing/fstest"
)

// A candidate reports the types and namespaces it RENDERS, which is the part only this
// engine can answer. A consumer can scan apiVersion/kind headers themselves — and will,
// meanwhile — but the moment a layout transform is involved, their answer and ours diverge
// silently, in the direction of provisioning something the writer then refuses.
//
// These tests pin the divergence rather than the agreement, because the agreement is the
// easy case.

const inventoryDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: written-in-the-file
spec:
  replicas: 1
`

const inventoryClusterRole = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: viewer
rules: []
`

// TestRenderInventory_NamespaceIsWhatRendersNotWhatIsWritten is the case the ask exists
// for: kustomize's namespace transformer OVERRIDES metadata.namespace, so a folder whose
// files say one namespace lands in another. A consumer reading the file headers would
// provision the wrong namespace and only find out when the writer refused.
func TestRenderInventory_NamespaceIsWhatRendersNotWhatIsWritten(t *testing.T) {
	fsys := fstest.MapFS{
		"base/deployment.yaml": {Data: []byte(inventoryDeployment)},
		"base/kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - deployment.yaml\n")},
		"overlays/prod/kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: where-it-lands\nresources:\n  - ../../base\n")},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "overlays/prod")

	requireTypesByNamespace(t, cand.RenderedTypes, map[string][]string{
		"where-it-lands": {"apps/v1/Deployment"},
	})
	requireStrings(t, "namespaceUndeclared", cand.RenderedTypes.NamespaceUndeclared, nil)
}

// TestRenderInventory_KeepsEachTypePairedWithItsOwnNamespace is the reason this is a map
// and not two lists. A folder rendering a Deployment into one namespace and a Service into
// another has exactly two pairs; published as a type set beside a namespace set it would
// read as four, and a tool generating one watch rule per pair would authorize two that
// match nothing in the repository.
func TestRenderInventory_KeepsEachTypePairedWithItsOwnNamespace(t *testing.T) {
	const service = `apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: payments
spec:
  ports: []
`
	fsys := fstest.MapFS{
		"app/deployment.yaml": {Data: []byte(inventoryDeployment)},
		"app/service.yaml":    {Data: []byte(service)},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "app")

	requireTypesByNamespace(t, cand.RenderedTypes, map[string][]string{
		"written-in-the-file": {"apps/v1/Deployment"},
		"payments":            {"v1/Service"},
	})
}

// TestRenderInventory_CoversTheBaseOutsideTheSubtree pins the other half of "as rendered":
// an overlay renders documents it does not contain, and those types still have to be
// served wherever it is applied. Counting only the overlay's own files would miss them.
func TestRenderInventory_CoversTheBaseOutsideTheSubtree(t *testing.T) {
	fsys := fstest.MapFS{
		"base/deployment.yaml":  {Data: []byte(inventoryDeployment)},
		"base/clusterrole.yaml": {Data: []byte(inventoryClusterRole)},
		"base/kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - deployment.yaml\n  - clusterrole.yaml\n")},
		"overlays/prod/kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nnamespace: prod\nresources:\n  - ../../base\n")},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "overlays/prod")

	requireTypesByNamespace(t, cand.RenderedTypes, map[string][]string{
		"prod": {"apps/v1/Deployment"},
	})
	// The ClusterRole renders with no namespace, and this scan cannot say WHY: it is
	// cluster-scoped, but proving that needs API discovery the scan does not have. So it
	// goes to namespaceUndeclared, which is named for what was observed rather than for
	// what a reader might infer.
	requireStrings(t, "namespaceUndeclared", cand.RenderedTypes.NamespaceUndeclared,
		[]string{"rbac.authorization.k8s.io/v1/ClusterRole"})
}

// TestRenderInventory_ATypeCanBeBothNamespacedAndNot covers the shape that looks like a
// contradiction and is not: two ConfigMaps, one carrying a namespace and one relying on
// whatever the applier defaults to.
func TestRenderInventory_ATypeCanBeBothNamespacedAndNot(t *testing.T) {
	const placed = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: placed\n  namespace: storefront\n"
	const floating = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: floating\n"

	fsys := fstest.MapFS{
		"app/placed.yaml":   {Data: []byte(placed)},
		"app/floating.yaml": {Data: []byte(floating)},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "app")

	requireTypesByNamespace(t, cand.RenderedTypes, map[string][]string{
		"storefront": {"v1/ConfigMap"},
	})
	requireStrings(t, "namespaceUndeclared", cand.RenderedTypes.NamespaceUndeclared,
		[]string{"v1/ConfigMap"})
}

// TestRenderInventory_PlainFolderReportsItsOwnDocuments covers the folder with no
// kustomization, where the documents ARE the render and nothing transforms them on the way
// to the cluster.
func TestRenderInventory_PlainFolderReportsItsOwnDocuments(t *testing.T) {
	fsys := fstest.MapFS{
		"app/deployment.yaml":  {Data: []byte(inventoryDeployment)},
		"app/clusterrole.yaml": {Data: []byte(inventoryClusterRole)},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "app")

	requireTypesByNamespace(t, cand.RenderedTypes, map[string][]string{
		"written-in-the-file": {"apps/v1/Deployment"},
	})
	requireStrings(t, "namespaceUndeclared", cand.RenderedTypes.NamespaceUndeclared,
		[]string{"rbac.authorization.k8s.io/v1/ClusterRole"})
}

// TestRenderInventory_AbsentWhenTheRootDoesNotBuild says nothing rather than guessing: a
// root kustomize cannot build is one whose render nobody knows, and an empty inventory is
// the honest answer. The candidate is refused anyway.
func TestRenderInventory_AbsentWhenTheRootDoesNotBuild(t *testing.T) {
	fsys := fstest.MapFS{
		"app/kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - does-not-exist.yaml\n")},
	}

	cand := candidateAt(t, scanRepoFS(context.Background(), fsys), "app")

	if cand.AcceptedByOperator {
		t.Fatalf("a root that does not build must be refused, got accepted")
	}
	if len(cand.RenderedTypes.ByNamespace) != 0 || len(cand.RenderedTypes.NamespaceUndeclared) != 0 {
		t.Errorf("renderedTypes = %+v, want nothing: we cannot know what it renders", cand.RenderedTypes)
	}
}

// candidateAt returns the candidate at path, failing the test when the scan did not report
// one there.
func candidateAt(t *testing.T, rep RepoReport, path string) RepoCandidate {
	t.Helper()
	for _, cand := range rep.Candidates {
		if cand.Path == path {
			return cand
		}
	}
	t.Fatalf("no candidate at %q; got %+v", path, rep.Candidates)
	return RepoCandidate{}
}

// requireTypesByNamespace asserts the exact type-to-namespace map, which is the whole
// point of the shape: a missing pair and an invented one are both failures.
func requireTypesByNamespace(t *testing.T, got RenderedTypes, want map[string][]string) {
	t.Helper()
	if len(got.ByNamespace) != len(want) {
		t.Fatalf("byNamespace = %v, want %v", got.ByNamespace, want)
	}
	for ns, types := range want {
		requireStrings(t, "byNamespace["+ns+"]", got.ByNamespace[ns], types)
	}
}

// requireStrings asserts an exact, ordered match — the sets are sorted, so order is part
// of the contract a consumer can rely on.
func requireStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}
