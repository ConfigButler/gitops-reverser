// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// specSchema builds the slice of a CRD this tool reads, from the YAML a CRD
// actually carries, so the tests exercise the same parse the real files take.
func specSchema(t *testing.T) *schema {
	t.Helper()
	var s schema
	require.NoError(t, yaml.Unmarshal([]byte(targetLike), &s))
	return &s
}

const targetLike = `
type: object
required: [branch]
properties:
  branch:
    type: string
  suspend:
    type: boolean
    default: false
  commit:
    type: object
    properties:
      window:
        type: string
      message:
        type: object
        properties:
          liveTemplate: {type: string}
  rules:
    type: array
    items:
      type: object
      required: [resources]
      properties:
        resources: {type: array}
        operations: {type: array}
`

// The gate: a field nobody described must fail the run, because the failure mode
// it replaces is a table that quietly stops listing everything you can set.
func TestCheckCoverage_FailsOnAFieldNobodyDescribed(t *testing.T) {
	spec := specSchema(t)
	kd := kindDoc{Kind: "GitTarget", Fields: []fieldDoc{
		{Path: "branch", Doc: "Branch to write"},
		{Path: "commit.window", Doc: "Coalescing window"},
		{Path: "commit.message", Doc: "How commits are phrased"},
		{Path: "rules.resources", Doc: "What to watch"},
		{Path: "rules.operations", Doc: "Which operations"},
	}}

	err := checkCoverage(kd, spec)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "suspend", "the undescribed field must be named")
}

// A described parent is a leaf: its children are the next document's business.
// Without this, describing spec.commit.message would drag every template field in.
func TestCheckCoverage_ADescribedParentStopsTheWalk(t *testing.T) {
	spec := specSchema(t)
	kd := kindDoc{Kind: "GitTarget", Fields: []fieldDoc{
		{Path: "branch", Doc: "Branch to write"},
		{Path: "suspend", Doc: "Stop writing"},
		{Path: "commit", Doc: "How writes become commits"},
		{Path: "rules", Doc: "What to watch"},
	}}

	assert.NoError(t, checkCoverage(kd, spec))
}

// The half-described parent is the real drift: somebody added a sibling to a
// group the table already expands, and listing four of five silently loses one.
func TestCheckCoverage_FailsOnASiblingAddedToAnExpandedParent(t *testing.T) {
	spec := specSchema(t)
	kd := kindDoc{Kind: "GitTarget", Fields: []fieldDoc{
		{Path: "branch", Doc: "Branch to write"},
		{Path: "suspend", Doc: "Stop writing"},
		{Path: "commit.window", Doc: "Coalescing window"},
		{Path: "rules.resources", Doc: "What to watch"},
		{Path: "rules.operations", Doc: "Which operations"},
	}}

	err := checkCoverage(kd, spec)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "commit.message")
}

func TestCheckCoverage_SkipCoversAFieldDeliberatelyLeftOut(t *testing.T) {
	spec := specSchema(t)
	kd := kindDoc{Kind: "GitTarget", Fields: []fieldDoc{
		{Path: "branch", Doc: "Branch to write"},
		{Path: "suspend", Skip: "internal"},
		{Path: "commit", Doc: "How writes become commits"},
		{Path: "rules", Doc: "What to watch"},
	}}

	assert.NoError(t, checkCoverage(kd, spec))
}

// Required and default are read off the schema, never off the prose, which is
// the whole reason this tool exists.
func TestRenderRow_TakesRequiredAndDefaultFromTheSchema(t *testing.T) {
	spec := specSchema(t)

	required, err := renderRow("GitTarget", fieldDoc{Path: "branch", Doc: "Branch to write"}, spec)
	require.NoError(t, err)
	assert.Contains(t, required, "| **required** |")

	defaulted, err := renderRow("GitTarget", fieldDoc{Path: "suspend", Doc: "Stop writing"}, spec)
	require.NoError(t, err)
	assert.Contains(t, defaulted, "| `false` |")
}

// A prose default is for the fields the schema leaves open, and only those. If
// the schema also has one the two can disagree, and the prose is the half that
// nothing checks.
func TestRenderRow_RejectsAProseDefaultTheSchemaAlreadyGives(t *testing.T) {
	spec := specSchema(t)

	_, err := renderRow("GitTarget", fieldDoc{Path: "suspend", Default: "off", Doc: "Stop"}, spec)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove the `default:`")
}

func TestRenderRow_ReportsAFieldThatNoLongerExists(t *testing.T) {
	spec := specSchema(t)

	_, err := renderRow("GitTarget", fieldDoc{Path: "commit.retired", Doc: "Gone"}, spec)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GitTarget.commit.retired")
}

// An array field is spelled the way the manifest spells it, so a reader can match
// the row to the YAML in front of them.
func TestDisplayPath_SpellsAnArrayItemWithBrackets(t *testing.T) {
	spec := specSchema(t)

	assert.Equal(t, "rules[].resources", displayPath("rules.resources", spec))
	assert.Equal(t, "commit.window", displayPath("commit.window", spec))
}

func TestFormatDefault(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"string": {`"5s"`, "`5s`"},
		"bool":   {`false`, "`false`"},
		"number": {`2`, "`2`"},
		"object": {`{"name": "default"}`, "`{\"name\":\"default\"}`"},
		"null":   {`null`, ""},
		"empty":  {`""`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatDefault(json.RawMessage(tc.in)))
		})
	}
}

// `default: off` is read by YAML as a boolean and reaches the table as "false".
// It is silent, and it wrote a wrong cell the first time fields.yaml was filled in.
func TestValidateIndexDoc_CatchesABareYAMLBoolean(t *testing.T) {
	var doc indexDoc
	require.NoError(t, yaml.Unmarshal([]byte(`
kinds:
  - kind: GitProvider
    fields:
      - path: commit.signing
        default: off
        doc: SSH commit signing
`), &doc))

	err := validateIndexDoc(&doc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "quote it")
}

func TestValidateIndexDoc_RequiresADocOrASkip(t *testing.T) {
	doc := indexDoc{Kinds: []kindDoc{{Kind: "GitTarget", Fields: []fieldDoc{{Path: "branch"}}}}}

	err := validateIndexDoc(&doc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a `doc:`")
}

// The committed fields.yaml and the committed CRDs must agree, which is what the
// lint task asserts. Running it here too means a `task manifests` that adds a
// field fails the unit suite, not only the docs gate.
func TestCommittedFieldsFileCoversTheCommittedCRDs(t *testing.T) {
	index, err := loadIndexDoc("fields.yaml")
	require.NoError(t, err)
	schemas, err := loadSchemas("../../" + crdDir)
	require.NoError(t, err)

	out, err := render(index, schemas)

	require.NoError(t, err)
	for _, kd := range index.Kinds {
		assert.Contains(t, out, "### "+kd.Title)
	}
	assert.NotContains(t, out, "| none |",
		"a field with no default and no prose for it reads as though omitting it does nothing")
}

func TestWriteBlock_ReportsAMissingMarker(t *testing.T) {
	path := t.TempDir() + "/doc.md"
	require.NoError(t, writeFile(path, "# Doc\n\nno markers here\n"))

	err := writeBlock(path, "body", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot find the generated block")
}

func TestWriteBlock_CheckFailsWhenTheBlockIsStale(t *testing.T) {
	path := t.TempDir() + "/doc.md"
	require.NoError(t, writeFile(path,
		"# Doc\n\n"+beginMarker+"\n\nold body\n"+endMarker+"\n"))

	err := writeBlock(path, "new body\n", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "task settings-index")
}

func TestWriteBlock_RewritesOnlyTheMarkedRegion(t *testing.T) {
	path := t.TempDir() + "/doc.md"
	require.NoError(t, writeFile(path,
		"# Doc\n\nbefore\n\n"+beginMarker+"\n\nold body\n"+endMarker+"\n\nafter\n"))

	require.NoError(t, writeBlock(path, "new body\n", false))

	got := readFile(t, path)
	assert.Contains(t, got, "before\n")
	assert.Contains(t, got, "after\n")
	assert.Contains(t, got, "new body")
	assert.NotContains(t, got, "old body")
	assert.NoError(t, writeBlock(path, "new body\n", true), "a fresh block must satisfy -check")
}

func TestLoadSchemas_ReportsAnEmptyDirectory(t *testing.T) {
	_, err := loadSchemas(t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CRDs found")
}

func TestLoadCRD_ReportsAnObjectWithNoStoredVersion(t *testing.T) {
	path := t.TempDir() + "/crd.yaml"
	require.NoError(t, writeFile(path, strings.TrimSpace(`
spec:
  names: {kind: Ghost}
  versions:
    - storage: false
`)+"\n"))

	_, _, err := loadCRD(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stored version")
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- a path this test just created
	require.NoError(t, err)
	return string(raw)
}

// TestRun_CheckPassesAgainstTheCommittedTable wires the whole tool together the way the lint gate
// does: read fields.yaml, read every CRD, render, and compare against what is committed in
// docs/configuration.md. It fails when the table has drifted from the API — which is the tool's
// entire purpose — and it fails here, in `task test`, rather than only in `task lint`.
func TestRun_CheckPassesAgainstTheCommittedTable(t *testing.T) {
	t.Chdir(repoRoot(t))

	require.NoError(t, run(true),
		"the generated settings index is out of date; run `go run ./hack/crdfields`")
}

// TestRun_RewriteIsIdempotent is the other mode. Rewriting an up-to-date document must leave it
// byte-for-byte alone: a generator that churns its own output turns every unrelated PR into a
// diff on docs/configuration.md.
func TestRun_RewriteIsIdempotent(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)

	before, err := os.ReadFile(targetFile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.WriteFile(targetFile, before, 0o600)) })

	require.NoError(t, run(false))

	after, err := os.ReadFile(targetFile)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a rewrite of an up-to-date table changes nothing")
}

// TestRun_ReportsAMissingInputRatherThanPanicking. The tool runs from the repository root; run
// from anywhere else it has to name the file it could not read, because "no such file" with no
// path is the least useful thing a build gate can say.
func TestRun_ReportsAMissingInputRatherThanPanicking(t *testing.T) {
	t.Chdir(t.TempDir())

	err := run(true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), fieldsFile)
}

// repoRoot walks up from the test's own directory to the module root, so the tool's fixed
// relative paths resolve the same way they do for `go run ./hack/crdfields`.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found above the test's working directory")
	return ""
}

// TestRender_RefusesAKindNoCRDDefines catches an entry left behind after a kind was renamed or
// removed: the table would describe settings that no longer exist anywhere.
func TestRender_RefusesAKindNoCRDDefines(t *testing.T) {
	index := &indexDoc{Kinds: []kindDoc{{Kind: "Ghost", Title: "Ghost"}}}

	_, err := render(index, map[string]*schema{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ghost: no CRD defines this kind")
}

// TestRender_RefusesACRDKindNobodyDescribes is the other direction, and the one a walk driven by
// fields.yaml would never reach on its own: a whole new kind shipping with no index entry at all.
func TestRender_RefusesACRDKindNobodyDescribes(t *testing.T) {
	index := &indexDoc{Kinds: []kindDoc{{
		Kind:   "GitTarget",
		Title:  "GitTarget",
		Fields: gitTargetFixtureFields(),
	}}}
	schemas := map[string]*schema{"GitTarget": specSchema(t), "Newcomer": specSchema(t)}

	_, err := render(index, schemas)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Newcomer: a CRD defines this kind and "+fieldsFile+" does not describe it")
}

// TestRender_RefusesAFieldTheSchemaDoesNotHave is the renderKind half of the gate: a described
// field that has been removed from the API produces a row with nothing behind it, so the run
// fails instead of printing one.
func TestRender_RefusesAFieldTheSchemaDoesNotHave(t *testing.T) {
	index := &indexDoc{Kinds: []kindDoc{{
		Kind:  "GitTarget",
		Title: "GitTarget",
		Fields: append(gitTargetFixtureFields(),
			fieldDoc{Path: "departed", Doc: "a field that was removed"}),
	}}}

	_, err := render(index, map[string]*schema{"GitTarget": specSchema(t)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GitTarget.departed")
}

// TestRender_WritesThePreambleAndOneSectionPerKind is the happy path of the assembly itself: what
// the two halves produce together, in the order the document expects them.
func TestRender_WritesThePreambleAndOneSectionPerKind(t *testing.T) {
	index := &indexDoc{
		Preamble: "Every setting, from the CRDs.",
		Kinds: []kindDoc{{
			Kind:   "GitTarget",
			Title:  "GitTarget",
			Intro:  "Where writes go.",
			Outro:  "See the guide for the rest.",
			Fields: gitTargetFixtureFields(),
		}},
	}

	out, err := render(index, map[string]*schema{"GitTarget": specSchema(t)})

	require.NoError(t, err)
	assert.Contains(t, out, "Every setting, from the CRDs.")
	assert.Contains(t, out, "### GitTarget")
	assert.Contains(t, out, "Where writes go.")
	assert.Contains(t, out, "| `branch` | **required** | the branch |")
	assert.NotContains(t, out, "`suspend`", "a skipped field stays out of the table")
	assert.Contains(t, out, "See the guide for the rest.")
}

// gitTargetFixtureFields describes every top-level property of targetLike, which is what the
// coverage half of the gate requires before it will render anything at all.
func gitTargetFixtureFields() []fieldDoc {
	return []fieldDoc{
		{Path: "branch", Doc: "the branch"},
		{Path: "suspend", Skip: "covered above"},
		{Path: "commit", Doc: "commit settings"},
		{Path: "rules", Doc: "what to mirror"},
	}
}
