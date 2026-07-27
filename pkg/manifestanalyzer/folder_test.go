// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer"
)

const configMapYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: demo
data:
  key: value
`

const kustomizationHelmYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
helmCharts:
  - name: podinfo
    repo: https://stefanprodan.github.io/podinfo
`

func TestScanFolder_AcceptsPlainKRM(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cm.yaml"), []byte(configMapYAML), 0o600))

	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)

	require.True(t, report.Status.Accepted)
	require.Empty(t, report.Status.Issues)
	require.Equal(t, manifestanalyzer.APIVersion, report.APIVersion)
	require.Equal(t, manifestanalyzer.KindFolderReport, report.Kind)
	require.Equal(t, manifestanalyzer.ModeScanFolder, report.Spec.Mode)
	require.Equal(t, root, report.Spec.Root)
	require.Equal(t, manifestanalyzer.GeneratorName, report.Status.Generator.Name)
	require.NotEmpty(t, report.Status.Generator.Version, "a report must always say what produced it")
}

// A folder the operator would refuse must come back as a successful scan with
// Accepted=false. Refusal is a verdict, never an error.
func TestScanFolder_RefusalIsNotAnError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cm.yaml"), []byte(configMapYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(kustomizationHelmYAML), 0o600))

	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)

	require.False(t, report.Status.Accepted)
	require.NotEmpty(t, report.Status.Issues)
	kinds := make([]manifestanalyzer.IssueKind, 0, len(report.Status.Issues))
	for _, issue := range report.Status.Issues {
		kinds = append(kinds, issue.Kind)
	}
	require.Contains(t, kinds, manifestanalyzer.IssueUnsupportedKustomize,
		"Helm inflation is the permanent support boundary and must be named as such")
}

// A folder holding a values file that an Argo CD Application names through helm.valueFiles
// is adopted, not refused: the values file is read-only context. This is the published
// contract for the live operator adopting a GitTarget subtree with a co-located release.
func TestScanFolder_ReferencedValuesFileAccepted(t *testing.T) {
	t.Parallel()

	const application = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cert-manager
  namespace: argocd
spec:
  sources:
    - repoURL: https://charts.jetstack.io
      chart: cert-manager
      helm:
        valueFiles:
          - $values/platform/cert-manager/values.yaml
    - repoURL: https://github.com/example-org/gitops.git
      ref: values
`
	const valuesFile = "# helm values -- not a Kubernetes object\nreplicaCount: 2\ninstallCRDs: true\n"
	const clusterIssuer = "apiVersion: cert-manager.io/v1\nkind: ClusterIssuer\nmetadata:\n  name: le\n"

	fsys := fstest.MapFS{
		"application.yaml":   {Data: []byte(application)},
		"values.yaml":        {Data: []byte(valuesFile)},
		"clusterissuer.yaml": {Data: []byte(clusterIssuer)},
	}
	report := manifestanalyzer.ScanFolderFS(context.Background(), fsys)

	require.True(t, report.Status.Accepted,
		"a referenced values file must not refuse its folder: %+v", report.Status.Issues)
	for _, issue := range report.Status.Issues {
		require.NotEqual(t, manifestanalyzer.IssueNonKRM, issue.Kind,
			"the values file the Application names is context, not a non-krm-yaml refusal")
	}
}

// The Flux counterpart to TestScanFolder_ReferencedValuesFileAccepted: a folder holding a
// values file a HelmRelease names through spec.chart.spec.valuesFiles is adopted, not refused.
func TestScanFolder_FluxHelmReleaseValuesFileAccepted(t *testing.T) {
	t.Parallel()

	const helmRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: ingress-nginx
  namespace: flux-system
spec:
  chart:
    spec:
      chart: ingress-nginx
      sourceRef:
        kind: HelmRepository
        name: ingress-nginx
      valuesFiles:
        - values.yaml
`
	const valuesFile = "# helm values -- not a Kubernetes object\ncontroller:\n  replicaCount: 2\n"

	fsys := fstest.MapFS{
		"helmrelease.yaml": {Data: []byte(helmRelease)},
		"values.yaml":      {Data: []byte(valuesFile)},
	}
	report := manifestanalyzer.ScanFolderFS(context.Background(), fsys)

	require.True(t, report.Status.Accepted, "a HelmRelease-referenced values file must not refuse its folder: %+v",
		report.Status.Issues)
	for _, issue := range report.Status.Issues {
		require.NotEqual(t, manifestanalyzer.IssueNonKRM, issue.Kind,
			"the values file the HelmRelease names is context, not a non-krm-yaml refusal")
	}
}

func TestScanFolder_MissingDirIsAnError(t *testing.T) {
	t.Parallel()
	_, err := manifestanalyzer.ScanFolder(context.Background(), filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
}

func TestScanFolderFS_ReportsDuplicateIdentity(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"a.yaml": {Data: []byte(configMapYAML)},
		"b.yaml": {Data: []byte(configMapYAML)},
	}
	report := manifestanalyzer.ScanFolderFS(context.Background(), fsys)

	require.False(t, report.Status.Accepted)
	require.Equal(t, manifestanalyzer.APIVersion, report.APIVersion)
	require.Empty(t, report.Spec.Root, "an fs.FS has no path to report")

	var found bool
	for _, issue := range report.Status.Issues {
		if issue.Kind == manifestanalyzer.IssueDuplicate {
			found = true
			require.NotEmpty(t, issue.Path)
			require.NotEmpty(t, issue.Message)
		}
	}
	require.True(t, found, "two documents with the same identity must be refused as duplicates")
}

// The retained set names the build directives the operator reads but never writes. A
// consumer relies on it to explain "we understand this kustomization; we will not edit it".
func TestScanFolder_ReportsRetainedKustomization(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cm.yaml"), []byte(configMapYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cm.yaml\n"), 0o600))

	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)
	require.True(t, report.Status.Accepted)
	require.Len(t, report.Status.Retained, 1)
	require.Equal(t, "kustomization.yaml", report.Status.Retained[0].Path)
	require.False(t, report.Status.Retained[0].Unsupported)
	require.Nil(t, report.Status.Retained[0].Identity,
		"a whole-file retention names no resource; only the refused mixed-file case does")
}

// A kustomization whose feature set the writer cannot map back to source is retained and
// flagged, so a consumer can point at the exact file that made the folder unadoptable.
func TestScanFolder_FlagsUnsupportedRetainedKustomization(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(kustomizationHelmYAML), 0o600))

	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)
	require.False(t, report.Status.Accepted)
	require.Len(t, report.Status.Retained, 1)
	require.True(t, report.Status.Retained[0].Unsupported)
}

// The JSON document is the published contract: it is a KRM document (apiVersion/kind,
// request in spec, findings in status), it always says what produced it, and issues is an
// empty array rather than null so a consumer can iterate it unconditionally.
func TestFolderReport_WriteJSON_Contract(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cm.yaml"), []byte(configMapYAML), 0o600))
	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf))

	require.Contains(t, buf.String(), `"issues": []`, "issues must marshal as [] and never null")

	var raw map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &raw))
	require.Equal(t, "manifestanalyzer.configbutler.ai/v1alpha1", raw["apiVersion"])
	require.Equal(t, "FolderReport", raw["kind"])
	require.NotContains(t, raw, "schemaVersion", "apiVersion replaces the schemaVersion marker outright")
	require.NotContains(t, raw, "metadata", "a report observes a path at an instant; it has no identity")

	spec, ok := raw["spec"].(map[string]any)
	require.True(t, ok, "the scan request is the spec")
	require.Equal(t, "scan-folder", spec["mode"])

	status, ok := raw["status"].(map[string]any)
	require.True(t, ok, "what was found is the status")
	require.Equal(t, true, status["accepted"])
	require.Equal(t, map[string]any{"name": "manifest-analyzer", "version": manifestanalyzer.Version()},
		status["generator"], "a report that cannot say what produced it is the failure this field prevents")
	require.NotContains(t, status, "retained", "an empty retained set is omitted")
}

// The public report must survive a marshal/unmarshal round trip unchanged, or a consumer
// storing it and reading it back would see a different verdict than the one produced.
func TestFolderReport_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cm.yaml"), []byte(configMapYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(kustomizationHelmYAML), 0o600))
	report, err := manifestanalyzer.ScanFolder(context.Background(), root)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf))

	var decoded manifestanalyzer.FolderReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Equal(t, report, decoded)
}
