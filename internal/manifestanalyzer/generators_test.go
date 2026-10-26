// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// coffeeKustomization is the issue's own example: a settings file and an env file, each wrapped
// by a configMapGenerator with the name hash disabled.
const coffeeKustomization = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: coffee
resources:
  - deployment.yaml
configMapGenerator:
  - name: coffee-menu
    files:
      - menu.yaml
  - name: coffee-flags
    envs:
      - flags.env
generatorOptions:
  disableNameSuffixHash: true
`

const coffeeDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: coffee
spec:
  template:
    spec:
      containers:
        - name: coffee
          image: ghcr.io/example/coffee:1.0.0
`

const coffeeMenu = "# Prices in cents.\nproducts:\n  - sku: espresso\n    priceCents: 275\n"

func coffeeFS() fstest.MapFS {
	return fstest.MapFS{
		"kustomization.yaml": {Data: []byte(coffeeKustomization)},
		"deployment.yaml":    {Data: []byte(coffeeDeployment)},
		"menu.yaml":          {Data: []byte(coffeeMenu)},
		"flags.env":          {Data: []byte("# Turn off during the rush.\nENABLE_VOUCHERS=true\nSHOW_BANNER=true\n")},
	}
}

// The inputs of a supported generator are named context: the folder is accepted, the .env file is
// not foreign content, and the settings file is not refused as non-KRM YAML.
func TestAccept_ConfigMapGeneratorWithTheHashDisabled(t *testing.T) {
	store := buildStoreFS(context.Background(), coffeeFS(), nil, WriterAllowlist())
	acc := AcceptStructureOnly(store)
	require.True(t, acc.Accepted, "issues: %+v", acc.Issues)
	require.Empty(t, store.Foreign)
	require.Equal(t, map[string]struct{}{"flags.env": {}, "menu.yaml": {}}, store.GeneratorInputs)
	require.NotContains(t, store.FilesByPath, "menu.yaml", "a generator input is never a managed file")
}

func TestConfigMapGenerators_Features(t *testing.T) {
	cases := map[string]struct {
		kustomization string
		want          []string
	}{
		"hash disabled globally": {
			kustomization: "configMapGenerator:\n  - name: a\n    literals: [k=v]\n" +
				"generatorOptions:\n  disableNameSuffixHash: true\n",
		},
		"hash disabled on the entry": {
			kustomization: "configMapGenerator:\n  - name: a\n    literals: [k=v]\n" +
				"    options:\n      disableNameSuffixHash: true\n",
		},
		"hash left on": {
			kustomization: "configMapGenerator:\n  - name: a\n    literals: [k=v]\n",
			want:          []string{featureGeneratorNameHash},
		},
		"behavior merge is refused, and the hash is not named beside it": {
			kustomization: "configMapGenerator:\n  - name: a\n    behavior: merge\n    literals: [k=v]\n",
			want:          []string{featureGeneratorBehavior},
		},
		"behavior create is the default": {
			kustomization: "configMapGenerator:\n  - name: a\n    behavior: create\n    literals: [k=v]\n" +
				"generatorOptions:\n  disableNameSuffixHash: true\n",
		},
		"secretGenerator stays refused": {
			kustomization: "secretGenerator:\n  - name: a\n    literals: [k=v]\n" +
				"generatorOptions:\n  disableNameSuffixHash: true\n",
			want: []string{"secretGenerator"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, features := parseKustomization([]byte(tc.kustomization), "kustomization.yaml", nil)
			if len(tc.want) == 0 {
				require.Empty(t, features)
				return
			}
			require.Equal(t, tc.want, features)
		})
	}
}

// The hash refusal is the one generator refusal its author can clear, so the message says how.
func TestAccept_ConfigMapGeneratorWithTheHashNamesTheAction(t *testing.T) {
	fsys := coffeeFS()
	fsys["kustomization.yaml"] = &fstest.MapFile{Data: []byte(
		strings.Replace(coffeeKustomization, "generatorOptions:\n  disableNameSuffixHash: true\n", "", 1))}
	acc := AcceptStructureOnly(buildStoreFS(context.Background(), fsys, nil, WriterAllowlist()))
	require.False(t, acc.Accepted)
	require.Len(t, acc.Issues, 1)
	issue := acc.Issues[0]
	require.Equal(t, IssueUnsupportedKustomize, issue.Kind)
	require.True(t, issue.Solvable)
	require.Equal(t, ActorRepositoryAuthor, issue.Actor)
	require.Contains(t, issue.Message, "set disableNameSuffixHash: true")
}

// A file both listed under resources: and read by a generator stays a resource: kustomize renders
// it as an object, so it must be mirrored, never hidden as an input.
func TestAttachGeneratorInputs_AResourceStaysAResource(t *testing.T) {
	scan := FolderScan{YAMLFiles: genFiles{
		{
			"kustomization.yaml",
			"resources: [cm.yaml]\nconfigMapGenerator:\n  - name: copy\n    files: [cm.yaml, notes.yaml]\n",
		},
		{"cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cm}\n"},
		{"notes.yaml", "just: notes\n"},
	}.files(), NonYAML: []string{"flags.env"}, Foreign: []ForeignEntry{{Path: "flags.env", Kind: ForeignFile}}}
	AttachGeneratorInputs(&scan, func(string) ([]byte, bool) { return nil, false })

	require.ElementsMatch(t, []string{"cm.yaml", "kustomization.yaml"}, pathsOf(scan.YAMLFiles))
	require.Equal(t, []string{"notes.yaml"}, pathsOf(scan.InputFiles))
	require.Len(t, scan.Foreign, 1, "a foreign file no generator names stays foreign")
}

// TestReadGeneratorSources_AgreesWithKustomize asks kustomize rather than our reading of it: the
// env file below holds every line shape kv.keyValuesFromLine treats specially, and the data we
// derive must be the data the real build generates.
func TestReadGeneratorSources_AgreesWithKustomize(t *testing.T) {
	env := "\xEF\xBB\xBFFIRST=after a byte-order mark\r\n" +
		"# a comment\n" +
		"   INDENTED=kept  \n" +
		"\n" +
		"EQUALS=a=b=c\n" +
		"QUOTED=\"not unquoted\"\n" +
		"BARE\n" +
		"LAST=no newline"
	files := genFiles{
		{"kustomization.yaml", "configMapGenerator:\n  - name: probe\n    envs: [probe.env]\n" +
			"    literals: [\"LIT='quoted'\"]\n    files: [key=probe.txt]\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n"},
		{"probe.env", env},
		{"probe.txt", "whole\nfile\n"},
	}.files()

	rendered, err := renderRoot(files, ".")
	require.NoError(t, err)
	require.Len(t, rendered, 1)
	want, _, _ := unstructured.NestedStringMap(rendered[0].Object.Object, "data")

	doc, features := parseKustomization(files[0].Content, "kustomization.yaml", nil)
	require.Empty(t, features)
	got, err := readGeneratorSources(&doc.configMapGenerators[0], readerOf(files))
	require.NoError(t, err)
	require.Equal(t, want, got.data)
}

func TestSetEnvValue(t *testing.T) {
	cases := map[string]struct {
		in, key, value, want string
	}{
		"edits the line in place":      {"# c\nA=1\nB=2\n", "A", "9", "# c\nA=9\nB=2\n"},
		"keeps the indent":             {"  A=1\n", "A", "9", "  A=9\n"},
		"keeps a CRLF terminator":      {"A=1\r\nB=2\r\n", "A", "9", "A=9\r\nB=2\r\n"},
		"keeps the byte-order mark":    {"\xEF\xBB\xBFA=1\n", "A", "9", "\xEF\xBB\xBFA=9\n"},
		"gives a bare key a value":     {"A\n", "A", "9", "A=9\n"},
		"appends a new key":            {"A=1\n", "B", "2", "A=1\nB=2\n"},
		"appends after a last line":    {"A=1", "B", "2", "A=1\nB=2\n"},
		"appends to an empty file":     {"", "B", "2", "B=2\n"},
		"a commented key is not a key": {"# A=1\n", "A", "2", "# A=1\nA=2\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, string(setEnvValue([]byte(tc.in), tc.key, tc.value)))
		})
	}
}

func TestRemoveEnvKey(t *testing.T) {
	require.Equal(t, "# c\nB=2\n", string(removeEnvKey([]byte("# c\nA=1\nB=2\n"), "A")))
	require.Equal(t, "A=1\n", string(removeEnvKey([]byte("A=1\nB=2"), "B")))
	require.Equal(t, "A=1\n", string(removeEnvKey([]byte("A=1\n"), "Z")), "an absent key is a no-op")
}

// generatedStore builds the writer's store over fsys and returns the generated object the live
// object resolves to.
func generatedFor(t *testing.T, fsys fstest.MapFS, live *unstructured.Unstructured) []*GeneratedObject {
	t.Helper()
	store := buildStoreFS(context.Background(), fsys, nil, WriterAllowlist())
	return store.GeneratedObjectsFor(live)
}

func liveConfigMap(namespace, name string, data map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
	if data != nil {
		obj.Object["data"] = data
	}
	return obj
}

func TestPlanGeneratedWrite_RoutesEachKey(t *testing.T) {
	fsys := coffeeFS()
	read := func(p string) ([]byte, bool) {
		f, ok := fsys[p]
		if !ok {
			return nil, false
		}
		return f.Data, true
	}
	live := liveConfigMap("coffee", "coffee-flags", map[string]interface{}{
		"ENABLE_VOUCHERS": "false",
		"SHOW_TIPS":       "true",
	})
	generated := generatedFor(t, fsys, live)
	require.Len(t, generated, 1)

	writes, err := PlanGeneratedWrite(generated[0], live, "", read)
	require.NoError(t, err)
	require.Equal(t, []GeneratedWrite{{
		Path:    "flags.env",
		Content: []byte("# Turn off during the rush.\nENABLE_VOUCHERS=false\nSHOW_TIPS=true\n"),
	}}, writes)

	// In sync: nothing to write.
	inSync := liveConfigMap("coffee", "coffee-menu", map[string]interface{}{"menu.yaml": coffeeMenu})
	generated = generatedFor(t, fsys, inSync)
	require.Len(t, generated, 1)
	writes, err = PlanGeneratedWrite(generated[0], inSync, "", read)
	require.NoError(t, err)
	require.Empty(t, writes)
}

func TestPlanGeneratedWrite_Refusals(t *testing.T) {
	sharedBase := fstest.MapFS{
		"base/kustomization.yaml": {Data: []byte("configMapGenerator:\n  - name: menu\n    envs: [menu.env]\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
		"base/menu.env":                    {Data: []byte("A=1\n")},
		"overlays/a/kustomization.yaml":    {Data: []byte("resources: [../../base]\n")},
		"overlays/b/kustomization.yaml":    {Data: []byte("resources: [../../base]\n")},
		"overlays/b/unrelated-config.yaml": {Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")},
	}
	readFrom := func(fsys fstest.MapFS) func(string) ([]byte, bool) {
		return func(p string) ([]byte, bool) {
			f, ok := fsys[p]
			if !ok {
				return nil, false
			}
			return f.Data, true
		}
	}

	cases := map[string]struct {
		fsys       fstest.MapFS
		live       *unstructured.Unstructured
		writeScope string
		want       string
	}{
		"a base two overlays render shares its inputs": {
			fsys: sharedBase,
			live: liveConfigMap("", "menu", map[string]interface{}{"A": "2"}),
			want: "rendered by 2 render roots (overlays/a, overlays/b)",
		},
		"a value with a newline has no env line to hold it": {
			fsys: coffeeFS(),
			live: liveConfigMap("coffee", "coffee-flags", map[string]interface{}{
				"ENABLE_VOUCHERS": "true\nfalse", "SHOW_BANNER": "true",
			}),
			want: `the value of key "ENABLE_VOUCHERS" spans lines`,
		},
		"a generator outside the write jail is base-owned": {
			fsys:       coffeeFS(),
			live:       liveConfigMap("coffee", "coffee-flags", map[string]interface{}{"ENABLE_VOUCHERS": "false"}),
			writeScope: "overlays/prod",
			want:       "the generator lives in kustomization.yaml, which this target reads and never writes",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			generated := generatedFor(t, tc.fsys, tc.live)
			require.NotEmpty(t, generated)
			_, err := PlanGeneratedWrite(generated[0], tc.live, tc.writeScope, readFrom(tc.fsys))
			require.ErrorIs(t, err, errGeneratedWrite)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// Two entries of one name, told apart only by their own namespace: the live object's namespace
// picks the entry, and an object neither entry's namespace names has no single source.
func TestGeneratedObjectsFor_EntriesSharingAName(t *testing.T) {
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte("configMapGenerator:\n" +
			"  - {name: menu, namespace: eu, literals: [A=1]}\n" +
			"  - {name: menu, namespace: us, literals: [A=2]}\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
	}
	generated := generatedFor(t, fsys, liveConfigMap("us", "menu", nil))
	require.Len(t, generated, 1)
	require.NotNil(t, generated[0].Entry)
	require.Equal(t, "us", generated[0].Entry.Namespace)

	require.Empty(t, generatedFor(t, fsys, liveConfigMap("ap", "menu", nil)))
}

type genFile struct{ path, content string }

type genFiles []genFile

func (g genFiles) files() []manifestedit.FileContent {
	out := make([]manifestedit.FileContent, 0, len(g))
	for _, f := range g {
		out = append(out, manifestedit.FileContent{Path: f.path, Content: []byte(f.content)})
	}
	return out
}

func pathsOf(files []manifestedit.FileContent) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func readerOf(files []manifestedit.FileContent) func(string) ([]byte, bool) {
	return func(p string) ([]byte, bool) {
		for _, f := range files {
			if f.Path == p {
				return f.Content, true
			}
		}
		return nil, false
	}
}
