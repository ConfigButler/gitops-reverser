// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/types"
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

// A delete keeps an input another entry also reads, and an input outside the write jail; a
// generator in a read-only base is refused, as an edit to it is.
func TestPlanGeneratedDelete(t *testing.T) {
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte("configMapGenerator:\n" +
			"  - {name: a, envs: [shared.env, own.env]}\n" +
			"  - {name: b, envs: [shared.env]}\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
		"shared.env": {Data: []byte("S=1\n")},
		"own.env":    {Data: []byte("O=1\n")},
	}
	read := func(p string) ([]byte, bool) {
		f, ok := fsys[p]
		if !ok {
			return nil, false
		}
		return f.Data, true
	}
	generated := generatedFor(t, fsys, liveConfigMap("", "a", nil))
	require.Len(t, generated, 1)

	writes, err := PlanGeneratedDelete(generated[0], "", read)
	require.NoError(t, err)
	require.Equal(t, []GeneratedWrite{
		{Path: "kustomization.yaml", Content: []byte("configMapGenerator:\n" +
			"  - {name: b, envs: [shared.env]}\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
		{Path: "own.env"},
	}, writes, "shared.env is read by entry b too, so it stays")

	_, err = PlanGeneratedDelete(generated[0], "overlays/prod", read)
	require.ErrorContains(t, err, "which this target reads and never writes")
}

// TestLiteralValue_AgreesWithKustomize writes literals the way the planner does and asks a real
// build what they come back as: a value that is itself quoted must survive kustomize removing one
// pair of surrounding quotes.
func TestLiteralValue_AgreesWithKustomize(t *testing.T) {
	values := []string{`plain`, `"double"`, `'single'`, `"unbalanced`, `a=b`, `'`, ``, "two\nlines"}
	edits := make([]manifestedit.LiteralEdit, 0, len(values))
	want := map[string]string{}
	for i, v := range values {
		key := fmt.Sprintf("K%d", i)
		edits = append(edits, manifestedit.LiteralEdit{Key: key, Value: v})
		want[key] = v
	}
	kustomization := []byte("configMapGenerator:\n  - name: probe\n    literals: [SEED=x]\n" +
		"generatorOptions:\n  disableNameSuffixHash: true\n")
	res, diags := manifestedit.EditConfigMapGeneratorLiterals("kustomization.yaml", kustomization,
		manifestedit.GeneratorEntryID{Name: "probe"}, edits)
	require.Equal(t, manifestedit.EditPatched, res.Mode, "%v", diags)

	rendered, err := renderRoot([]manifestedit.FileContent{{Path: "kustomization.yaml", Content: res.Content}}, ".")
	require.NoError(t, err)
	got, _, _ := unstructured.NestedStringMap(rendered[0].Object.Object, "data")
	want["SEED"] = "x"
	require.Equal(t, want, got)
}

// The sweep treats a generated object as it treats a document: dropped only when the registry
// names it, it is in the plan's scope, and the policy drops orphans.
func TestBuildPlan_SweepsAGeneratedObject(t *testing.T) {
	fsys := coffeeFS()
	drops := func(store *ManifestStore, inScope func(types.ResourceIdentifier) bool) []PlanAction {
		plan := BuildScopedPlan(store, nil, nil, Policy{Sweep: SweepDropOrphans}, inScope)
		var out []PlanAction
		for _, a := range plan.Actions {
			if a.Generated {
				out = append(out, a)
			}
		}
		return out
	}
	followable := buildStoreFS(context.Background(), fsys, snapMapper(), WriterAllowlist())

	got := drops(followable, allInScope)
	require.Len(t, got, 2, "both generated ConfigMaps are absent from the empty snapshot")
	require.Equal(t, PlanDropOrphan, got[0].Kind)
	require.Equal(t, "kustomization.yaml", got[0].Ref.FilePath)
	require.Equal(t, "coffee", got[0].Resource.Namespace)

	deploymentsOnly := func(id types.ResourceIdentifier) bool { return id.Resource == "deployments" }
	require.Empty(t, drops(followable, deploymentsOnly), "a per-type plan sweeps only its own type")

	structureOnly := buildStoreFS(context.Background(), fsys, nil, WriterAllowlist())
	require.Empty(t, drops(structureOnly, allInScope), "a scan with no registry names nothing to drop")
}

// A live object the generator already renders plans no write, so it needs no input it may write:
// a generator two render roots share, or one in a read-only base, is matched as it stands.
func TestPlanGeneratedWrite_AnUnchangedObjectNeedsNoWritableEntry(t *testing.T) {
	sharedBase := fstest.MapFS{
		"base/kustomization.yaml": {Data: []byte("configMapGenerator:\n  - name: menu\n    envs: [menu.env]\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
		"base/menu.env":                 {Data: []byte("A=1\n")},
		"overlays/a/kustomization.yaml": {Data: []byte("resources: [../../base]\n")},
		"overlays/b/kustomization.yaml": {Data: []byte("resources: [../../base]\n")},
	}
	cases := map[string]struct {
		fsys       fstest.MapFS
		live       *unstructured.Unstructured
		writeScope string
	}{
		"shared by two render roots": {
			fsys: sharedBase,
			live: liveConfigMap("", "menu", map[string]interface{}{"A": "1"}),
		},
		"in a read-only base": {
			fsys: coffeeFS(),
			live: liveConfigMap("coffee", "coffee-flags", map[string]interface{}{
				"ENABLE_VOUCHERS": "true", "SHOW_BANNER": "true",
			}),
			writeScope: "overlays/prod",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			generated := generatedFor(t, tc.fsys, tc.live)
			require.NotEmpty(t, generated)
			writes, err := PlanGeneratedWrite(generated[0], tc.live, tc.writeScope, func(string) ([]byte, bool) {
				t.Fatal("an unchanged object reads no input")
				return nil, false
			})
			require.NoError(t, err)
			require.Empty(t, writes)
		})
	}
}

// An input a HelmRelease also names as a values file is the release's too, and the render cannot
// show what an edit does to the release: a delete keeps it, and a write to it is refused.
func TestGeneratedInputs_AReleasesValuesFileIsKeptAndNeverWritten(t *testing.T) {
	fsys := fstest.MapFS{
		"kustomization.yaml": {Data: []byte("resources: [release.yaml]\n" +
			"configMapGenerator:\n  - {name: chart-values, files: [values.yaml], literals: [MODE=a]}\n" +
			"generatorOptions:\n  disableNameSuffixHash: true\n")},
		"release.yaml": {Data: []byte("apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\n" +
			"metadata: {name: shop}\nspec:\n  chart:\n    spec:\n      chart: shop\n      valuesFiles: [values.yaml]\n")},
		"values.yaml": {Data: []byte("replicas: 2\n")},
	}
	read := func(p string) ([]byte, bool) {
		f, ok := fsys[p]
		if !ok {
			return nil, false
		}
		return f.Data, true
	}
	live := liveConfigMap("", "chart-values", map[string]interface{}{"values.yaml": "replicas: 2\n", "MODE": "a"})
	generated := generatedFor(t, fsys, live)
	require.Len(t, generated, 1)

	writes, err := PlanGeneratedDelete(generated[0], "", read)
	require.NoError(t, err)
	require.Equal(t, []string{"kustomization.yaml"}, pathsOf(toFileContents(writes)), "values.yaml stays")

	_, err = PlanGeneratedWrite(
		generated[0],
		liveConfigMap(
			"",
			"chart-values",
			map[string]interface{}{"values.yaml": "replicas: 3\n", "MODE": "a"},
		),
		"",
		read,
	)
	require.ErrorContains(t, err, "values.yaml is also a release's values file")

	writes, err = PlanGeneratedWrite(
		generated[0],
		liveConfigMap(
			"",
			"chart-values",
			map[string]interface{}{"values.yaml": "replicas: 2\n", "MODE": "b"},
		),
		"",
		read,
	)
	require.NoError(t, err, "a key of another input is written as before")
	require.Equal(t, []string{"kustomization.yaml"}, pathsOf(toFileContents(writes)))
}

func toFileContents(writes []GeneratedWrite) []manifestedit.FileContent {
	out := make([]manifestedit.FileContent, 0, len(writes))
	for _, w := range writes {
		out = append(out, manifestedit.FileContent{Path: w.Path, Content: w.Content})
	}
	return out
}

// Two generated objects of one name, from one kustomization, in two namespaces are two objects to
// the oracle: removing one is not refused because the other still renders, and a write to one is
// checked against that one.
func TestVerifyBatchRenders_GeneratedObjectsInTwoNamespaces(t *testing.T) {
	kustomization := func(entries string) manifestedit.FileContent {
		return manifestedit.FileContent{Path: "kustomization.yaml", Content: []byte("configMapGenerator:\n" + entries +
			"generatorOptions:\n  disableNameSuffixHash: true\n")}
	}
	eu := "  - {name: menu, namespace: eu, literals: [A=1]}\n"
	us := "  - {name: menu, namespace: us, literals: [A=1]}\n"
	before := []manifestedit.FileContent{kustomization(eu + us)}

	removed := WriteIntent{SourcePath: "kustomization.yaml", Kind: "ConfigMap", Name: "menu", Namespace: "eu",
		Removed: true}
	require.NoError(t, VerifyBatchRenders(before, []manifestedit.FileContent{kustomization(us)},
		[]WriteIntent{removed}, VerifyOptions{}))

	written := liveConfigMap("us", "menu", map[string]interface{}{"A": "2"})
	intent := WriteIntent{SourcePath: "kustomization.yaml", Kind: "ConfigMap", Name: "menu", Namespace: "us",
		Desired: written, Governed: true}
	after := []manifestedit.FileContent{kustomization(eu + "  - {name: menu, namespace: us, literals: [A=2]}\n")}
	require.NoError(t, VerifyBatchRenders(before, after, []WriteIntent{intent}, VerifyOptions{}))

	intent.Namespace = "eu"
	written.SetNamespace("eu")
	require.Error(t, VerifyBatchRenders(before, after, []WriteIntent{intent}, VerifyOptions{}),
		"eu did not move, and us moved without an intent")
}
