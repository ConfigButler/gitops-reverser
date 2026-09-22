// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
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
