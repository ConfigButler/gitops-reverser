// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
)

// argoAppMultiSourceValues is the load-bearing fixture shape: a multi-source Application whose
// first source renders an external chart and whose second source (ref: values) exposes this
// repo, so helm.valueFiles names the co-located values file through a $values ref.
const argoAppMultiSourceValues = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cert-manager
  namespace: argocd
spec:
  sources:
    - repoURL: https://charts.jetstack.io
      chart: cert-manager
      targetRevision: v1.16.2
      helm:
        valueFiles:
          - $values/platform/cert-manager/values.yaml
    - repoURL: https://github.com/example-org/gitops.git
      targetRevision: main
      ref: values
`

// argoAppRelativeValues is the simpler single-source spelling: helm.valueFiles names a file by a
// path relative to the Application's own directory.
const argoAppRelativeValues = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app
  namespace: argocd
spec:
  source:
    repoURL: https://charts.example.com
    chart: foo
    helm:
      valueFiles:
        - values.yaml
`

// fluxHelmReleaseValues is a Flux HelmRelease naming a values file through
// spec.chart.spec.valuesFiles — the Flux counterpart to an Argo Application's helm.valueFiles.
const fluxHelmReleaseValues = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: ingress-nginx
  namespace: flux-system
spec:
  interval: 30m
  chart:
    spec:
      chart: ingress-nginx
      version: 4.11.3
      sourceRef:
        kind: HelmRepository
        name: ingress-nginx
      valuesFiles:
        - values.yaml
`

// helmValuesFile is a Helm values document: plain YAML, no apiVersion/kind, so it is non-KRM.
const helmValuesFile = "# helm values for the chart -- NOT a Kubernetes object\n" +
	"replicaCount: 2\ninstallCRDs: true\n"

// clusterIssuerYAML is the co-located plain KRM the folder-wide refusal used to take down.
const clusterIssuerYAML = `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt
spec:
  acme:
    email: platform@example.com
`

// valueRefsOf builds a structure-only store and returns the read-only-context set.
func valueRefsOf(t *testing.T, fsys fs.FS) map[string]struct{} {
	t.Helper()
	store := buildStoreFS(context.Background(), fsys, nil, WriterAllowlist())
	return store.ValueFileRefs
}

func hasRef(refs map[string]struct{}, p string) bool {
	_, ok := refs[p]
	return ok
}

func TestHelmValueFileRefs_WholeRepoAbsoluteRef(t *testing.T) {
	// The whole-repo scan sees the $values path at exactly its root-relative location.
	fsys := fstest.MapFS{
		"platform/cert-manager/application.yaml":   {Data: []byte(argoAppMultiSourceValues)},
		"platform/cert-manager/values.yaml":        {Data: []byte(helmValuesFile)},
		"platform/cert-manager/clusterissuer.yaml": {Data: []byte(clusterIssuerYAML)},
	}
	refs := valueRefsOf(t, fsys)
	if !hasRef(refs, "platform/cert-manager/values.yaml") {
		t.Fatalf("want platform/cert-manager/values.yaml as read-only context, got %v", refs)
	}
	if len(refs) != 1 {
		t.Fatalf("only the referenced values file is context, got %v", refs)
	}
}

func TestHelmValueFileRefs_SubtreeCoLocatedRef(t *testing.T) {
	// The live operator's GitTarget subtree has lost the repo-root prefix, so the same
	// $values/platform/cert-manager/values.yaml ref resolves by co-location beside the app.
	fsys := fstest.MapFS{
		"application.yaml":   {Data: []byte(argoAppMultiSourceValues)},
		"values.yaml":        {Data: []byte(helmValuesFile)},
		"clusterissuer.yaml": {Data: []byte(clusterIssuerYAML)},
	}
	refs := valueRefsOf(t, fsys)
	if !hasRef(refs, "values.yaml") {
		t.Fatalf("want values.yaml recognised by co-location in a subtree scan, got %v", refs)
	}
}

func TestHelmValueFileRefs_RelativeSingleSource(t *testing.T) {
	fsys := fstest.MapFS{
		"application.yaml": {Data: []byte(argoAppRelativeValues)},
		"values.yaml":      {Data: []byte(helmValuesFile)},
	}
	refs := valueRefsOf(t, fsys)
	if !hasRef(refs, "values.yaml") {
		t.Fatalf("want values.yaml resolved relative to the Application, got %v", refs)
	}
}

func TestHelmValueFileRefs_FluxHelmReleaseCoLocated(t *testing.T) {
	// The Flux counterpart: a HelmRelease names values.yaml through spec.chart.spec.valuesFiles,
	// resolved relative to the HelmRelease's own directory.
	fsys := fstest.MapFS{
		"helmrelease.yaml": {Data: []byte(fluxHelmReleaseValues)},
		"values.yaml":      {Data: []byte(helmValuesFile)},
	}
	refs := valueRefsOf(t, fsys)
	if !hasRef(refs, "values.yaml") {
		t.Fatalf("want values.yaml named by a HelmRelease as read-only context, got %v", refs)
	}
}

func TestHelmValueFileRefs_FluxHelmReleaseRepoRootPath(t *testing.T) {
	// Flux valuesFiles are relative to the SourceRef (the repo), so a whole-repo scan resolves
	// the path at its root-relative location, and a subtree scan resolves it by co-location.
	release := "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: r\n" +
		"spec:\n  chart:\n    spec:\n      chart: c\n      valuesFiles:\n        - apps/ingress/values.yaml\n"

	wholeRepo := fstest.MapFS{
		"apps/ingress/helmrelease.yaml": {Data: []byte(release)},
		"apps/ingress/values.yaml":      {Data: []byte(helmValuesFile)},
	}
	if refs := valueRefsOf(t, wholeRepo); !hasRef(refs, "apps/ingress/values.yaml") {
		t.Fatalf("whole-repo scan should resolve the root-relative valuesFiles path, got %v", refs)
	}

	subtree := fstest.MapFS{
		"helmrelease.yaml": {Data: []byte(release)},
		"values.yaml":      {Data: []byte(helmValuesFile)},
	}
	if refs := valueRefsOf(t, subtree); !hasRef(refs, "values.yaml") {
		t.Fatalf("subtree scan should resolve the co-located values file by basename, got %v", refs)
	}
}

func TestHelmValueFileRefs_FluxKRMTargetIsNotContext(t *testing.T) {
	// As with Argo, a HelmRelease that references a real manifest must not un-manage it.
	release := "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: r\n" +
		"spec:\n  chart:\n    spec:\n      chart: c\n      valuesFiles:\n        - deploy.yaml\n"
	fsys := fstest.MapFS{
		"helmrelease.yaml": {Data: []byte(release)},
		"deploy.yaml":      {Data: []byte(deployYAML)},
	}
	if refs := valueRefsOf(t, fsys); len(refs) != 0 {
		t.Fatalf("a KRM target is not read-only context, got %v", refs)
	}
}

func TestHelmValueFileRefs_KRMTargetIsNotContext(t *testing.T) {
	// A release that references a real manifest must not silently un-manage it: only a
	// non-KRM values file is rescued as context.
	app := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app\n" +
		"spec:\n  source:\n    helm:\n      valueFiles:\n        - deploy.yaml\n"
	fsys := fstest.MapFS{
		"application.yaml": {Data: []byte(app)},
		"deploy.yaml":      {Data: []byte(deployYAML)},
	}
	refs := valueRefsOf(t, fsys)
	if len(refs) != 0 {
		t.Fatalf("a KRM target is not read-only context, got %v", refs)
	}
}

func TestHelmValueFileRefs_MissingTargetIsNotContext(t *testing.T) {
	fsys := fstest.MapFS{"application.yaml": {Data: []byte(argoAppRelativeValues)}}
	if refs := valueRefsOf(t, fsys); len(refs) != 0 {
		t.Fatalf("a values file that is not in the scan is not context, got %v", refs)
	}
}

func TestHelmValueFileRefs_NonApplicationIsIgnored(t *testing.T) {
	// A helm.valueFiles-looking field on a document that is not an Argo Application is not a
	// claim we honour — the vocabulary is closed to argoproj.io/Application.
	notApp := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n" +
		"spec:\n  source:\n    helm:\n      valueFiles:\n        - values.yaml\n"
	fsys := fstest.MapFS{
		"cm.yaml":     {Data: []byte(notApp)},
		"values.yaml": {Data: []byte(helmValuesFile)},
	}
	if refs := valueRefsOf(t, fsys); len(refs) != 0 {
		t.Fatalf("only an Argo Application names a values file, got %v", refs)
	}
}
