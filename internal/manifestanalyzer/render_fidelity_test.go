// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

type renderFidelityWant struct {
	Condition   string `yaml:"condition"`
	Reason      string `yaml:"reason"`
	Divergences []struct {
		Field string `yaml:"field"`
		Token string `yaml:"token"`
	} `yaml:"divergences"`
}

// TestRenderTokenDivergences_Fixtures keeps the render-vs-live contract in an isolated corpus.
// The fixtures deliberately do not use the broader layout corpus: each one owns both the Git
// tree and its sanitized live object, which is the only honest input to this predicate.
func TestRenderTokenDivergences_Fixtures(t *testing.T) {
	root := filepath.Join("testdata", "render-fidelity")
	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			fixtureRoot := filepath.Join(root, name)
			want := readRenderFidelityWant(t, filepath.Join(fixtureRoot, "want.yaml"))
			rendered := renderFidelityFixture(t, filepath.Join(fixtureRoot, "git"))
			live := readRenderFidelityObject(t, filepath.Join(fixtureRoot, "live.yaml"))

			got := RenderTokenDivergences(rendered, live)
			if want.Condition == "True" {
				require.Equal(t, "RenderMatchesLive", want.Reason)
				require.Empty(t, got)
				return
			}
			require.Equal(t, "False", want.Condition)
			require.Equal(t, "RenderDoesNotMatchLive", want.Reason)
			require.Len(t, got, len(want.Divergences))
			for index, expected := range want.Divergences {
				require.Equal(t, expected.Field, got[index].Field)
				require.Equal(t, expected.Token, got[index].Token)
			}
		})
	}
}

func readRenderFidelityWant(t *testing.T, path string) renderFidelityWant {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var want renderFidelityWant
	require.NoError(t, yaml.Unmarshal(content, &want))
	return want
}

func renderFidelityFixture(t *testing.T, root string) map[string]interface{} {
	t.Helper()
	files := readYAMLTree(t, root)
	for _, file := range files {
		if filepath.Base(file.Path) != "kustomization.yaml" && filepath.Base(file.Path) != "kustomization.yml" {
			continue
		}
		rendered, err := renderRoot(files, ".")
		require.NoError(t, err)
		require.Len(t, rendered, 1, "each render-fidelity fixture owns exactly one object")
		return rendered[0].Object.Object
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	require.Len(t, files, 1, "a plain render-fidelity fixture has one manifest")
	return readRenderFidelityObject(t, filepath.Join(root, files[0].Path))
}

func readRenderFidelityObject(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var object map[string]interface{}
	require.NoError(t, yaml.Unmarshal(content, &object))
	return (&unstructured.Unstructured{Object: object}).Object
}
