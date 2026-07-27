// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer"
)

// A whole published document, checked in. Everything else in this package asserts one
// field at a time, which cannot answer the question a consumer actually asks — "what does
// the thing I am about to parse look like?" — and cannot show a reviewer that a change to
// the envelope changed the envelope.
//
// The goldens are YAML because the report is a KRM document: the same bytes `--format
// yaml` prints, in the serialization a human reads and a diff explains. Regenerate with
// UPDATE_GOLDEN=1 and read the diff; that diff IS the contract change.
//
// generator.version is "dev" here and always will be: a test binary carries no ldflags and
// the toolchain records "(devel)" for a module built from its own checkout, which [Version]
// deliberately rejects. So the goldens are stable without any normalization.

// TestRepoReport_Golden pins the document `manifest-analyzer --mode scan-repo --format
// yaml` prints, over a corpus fixture whose folder is refused for a reason its author can
// solve.
func TestRepoReport_Golden(t *testing.T) {
	t.Parallel()

	root := fixture(t, "unsupported", "plain-nonkrm")
	report, err := manifestanalyzer.ScanRepo(context.Background(), root)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, report.WriteYAML(&buf))
	assertGolden(t, "testdata/repo-report.golden.yaml", buf.Bytes())
}

// TestFolderReport_Golden pins the document `manifest-analyzer --mode scan-folder --format
// yaml` prints. The tree is deliberately refused two ways at once, so the golden shows both
// answers the contract can carry: a stray file its author can remove, and a kustomization
// nobody can make adoptable.
func TestFolderReport_Golden(t *testing.T) {
	t.Parallel()

	report := manifestanalyzer.ScanFolderFS(context.Background(), fstest.MapFS{
		"kustomization.yaml": {Data: []byte(kustomizationHelmYAML)},
		"configmap.yaml":     {Data: []byte(configMapYAML)},
		"notes.txt":          {Data: []byte("scratch\n")},
	})

	var buf bytes.Buffer
	require.NoError(t, report.WriteYAML(&buf))
	assertGolden(t, "testdata/folder-report.golden.yaml", buf.Bytes())
}

// TestWriteYAML_IsTheSameDocumentAsWriteJSON keeps the two serializations from drifting: a
// consumer reading YAML and one reading JSON must be handed the same document, or the
// golden below documents a shape nobody receives.
func TestWriteYAML_IsTheSameDocumentAsWriteJSON(t *testing.T) {
	t.Parallel()

	report, err := manifestanalyzer.ScanRepo(context.Background(), fixture(t, "unsupported", "plain-nonkrm"))
	require.NoError(t, err)

	var asJSON, asYAML bytes.Buffer
	require.NoError(t, report.WriteJSON(&asJSON))
	require.NoError(t, report.WriteYAML(&asYAML))

	var fromJSON, fromYAML manifestanalyzer.RepoReport
	require.NoError(t, yamlUnmarshal(asJSON.Bytes(), &fromJSON))
	require.NoError(t, yamlUnmarshal(asYAML.Bytes(), &fromYAML))
	require.Equal(t, fromJSON, fromYAML)
	require.Equal(t, report, fromYAML, "the YAML form must round-trip back to the report that produced it")
}

// assertGolden compares got against the golden file, rewriting it when UPDATE_GOLDEN=1.
func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(path, got, 0o600))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "read %s (run UPDATE_GOLDEN=1 to create)", path)
	require.Equal(t, string(want), string(got),
		"the published document changed. If that is intended, run UPDATE_GOLDEN=1 and review the diff — "+
			"it is a change to what every consumer parses.")
}

// yamlUnmarshal decodes either serialization: sigs.k8s.io/yaml routes YAML through the
// JSON tags, so one decoder reads both forms.
func yamlUnmarshal(data []byte, into any) error {
	return yaml.Unmarshal(data, into)
}
