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

	requireStrings(t, "namespaces", cand.Namespaces, []string{"where-it-lands"})
	if len(cand.Kinds) != 1 || cand.Kinds[0].Kind != "Deployment" || cand.Kinds[0].Group != "apps" {
		t.Errorf("kinds = %+v, want the single apps/v1 Deployment the overlay renders", cand.Kinds)
	}
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

	if len(cand.Kinds) != 2 {
		t.Fatalf("kinds = %+v, want both types the overlay renders from the base", cand.Kinds)
	}
	// Sorted by group, then version, then kind — so "apps" precedes
	// "rbac.authorization.k8s.io" and the order is part of what a consumer can rely on.
	requireStrings(t, "kinds", kindNames(cand.Kinds), []string{"Deployment", "ClusterRole"})
	// The ClusterRole is cluster-scoped: it lands in no namespace and must contribute none,
	// because an empty string in the set would read as a namespace called "".
	requireStrings(t, "namespaces", cand.Namespaces, []string{"prod"})
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

	requireStrings(t, "kinds", kindNames(cand.Kinds), []string{"Deployment", "ClusterRole"})
	requireStrings(t, "namespaces", cand.Namespaces, []string{"written-in-the-file"})
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
	if len(cand.Kinds) != 0 || len(cand.Namespaces) != 0 {
		t.Errorf("kinds = %+v, namespaces = %+v, want nothing: we cannot know what it renders",
			cand.Kinds, cand.Namespaces)
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

// kindNames projects a GVK slice onto its kinds, for the assertions that care about which
// types are present rather than their full identity.
func kindNames(kinds []GVK) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.Kind)
	}
	return out
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
