// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// mustObj parses YAML into an unstructured object for use as a desired state.
func mustObj(t *testing.T, y string) *unstructured.Unstructured {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(y), &m))
	return &unstructured.Unstructured{Object: m}
}

// testRender is the small canonical renderer the tests inject in place of the
// production house renderer (sanitize.MarshalToOrderedYAML). The package is
// mechanism, not policy, so the renderer is injected, never owned here.
func testRender(obj *unstructured.Unstructured) ([]byte, error) {
	return yaml.Marshal(obj.Object)
}

// patch wraps PatchDocument with the injected test renderer, so the many edit
// tests read like the original three-argument call while the package stays
// renderer-agnostic. The desired object is the already-projected Git state.
func patch(content []byte, documentIndex int, desired *unstructured.Unstructured) (EditResult, []Diagnostic) {
	return PatchDocument(content, documentIndex, desired, EditOptions{Render: testRender})
}

// docBody returns the body bytes of one document in content.
func docBody(content string, idx int) string {
	docs := splitDocuments(content)
	if idx < 0 || idx >= len(docs) {
		return ""
	}
	return docs[idx].body
}

// --- Test 1: No-op round-trip drift (hard, gating) ---

func TestRoundTripDrift_Baseline(t *testing.T) {
	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config # stable name
  namespace: default
data:
  build: "00123"
  enabled: "false"
  start.sh: |-
    #!/bin/sh
    set -eu

    echo "starting app"
    exec /app/server
`
	root, empty, err := decodeDoc(manifest)
	require.NoError(t, err)
	require.False(t, empty)

	out, err := encodeNode(root)
	require.NoError(t, err)

	if string(out) == manifest {
		t.Log("yaml.v3 round-trip is byte-for-byte identical for this manifest")
		return
	}
	// Not byte-identical is the interesting POC finding; record exactly where.
	t.Logf("yaml.v3 round-trip drifted from source.\n--- original ---\n%s\n--- re-encoded ---\n%s", manifest, out)

	// It must at least stay semantically equivalent.
	var a, b map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &a))
	require.NoError(t, yaml.Unmarshal(out, &b))
	assert.Equal(t, normalizeJSON(a), normalizeJSON(b), "round-trip must preserve meaning")
}

// --- Test 2: Multi-document inventory ---

func TestIndex_MultiDocument(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
---
# intentionally empty document
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
`
	inv, _ := IndexFile("app.yaml", []byte(content))

	cm, ok := inv.Location(Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "app-config"})
	require.True(t, ok)
	assert.Equal(t, 0, cm.DocumentIndex)

	dep, ok := inv.Location(Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "default", Name: "app"})
	require.True(t, ok)
	assert.Equal(t, 2, dep.DocumentIndex, "deployment must keep document index 2 despite the empty doc 1")
}

// --- Test 3: Non-KRM YAML ---

func TestIndex_NonKRMIgnored(t *testing.T) {
	content := `# just some config
database:
  host: localhost
  port: 5432
`
	inv, diags := IndexFile("values.yaml", []byte(content))
	assert.Empty(t, inv.Records, "non-KRM YAML must not be indexed as a resource")
	assert.NotEmpty(t, diags, "non-KRM YAML should emit a diagnostic")
}

// --- Test 4: Duplicate identity ---

func TestIndex_DuplicateFirstWins(t *testing.T) {
	doc := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
`
	inv, diags := IndexFiles([]FileContent{
		{Path: "overlays/dev/app.yaml", Content: []byte(doc)},
		{Path: "apps/app.yaml", Content: []byte(doc)},
	})

	id := Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "default", Name: "app"}
	loc, ok := inv.Location(id)
	require.True(t, ok)
	assert.Equal(t, "apps/app.yaml", loc.Path, "first by lexicographic path wins")

	require.Len(t, inv.Duplicates(), 1)
	assert.Equal(t, "overlays/dev/app.yaml", inv.Duplicates()[0].Location.Path)

	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "removing duplicate") {
			found = true
		}
	}
	assert.True(t, found, "a duplicate diagnostic should be emitted")
}

// --- Test 5: Semantic no-op vs cleaning (hard) ---

func TestPatch_TrueNoOpPreservesBytes(t *testing.T) {
	gitContent := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  color: blue
`
	// The caller already projected the API object to the clean Git state, so the
	// desired object matches Git exactly: the package never sanitizes internally.
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  color: blue
`)
	res, _ := patch([]byte(gitContent), 0, desired)
	assert.Equal(t, EditNoChange, res.Mode)
	assert.Equal(t, gitContent, string(res.Content), "a true no-op must preserve bytes")
}

func TestPatch_DirtyGitFieldIsCleaned(t *testing.T) {
	// resourceVersion lives in Git: it must be deleted, not preserved.
	gitContent := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  resourceVersion: "12345"
data:
  color: blue
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  color: blue
`)
	res, _ := patch([]byte(gitContent), 0, desired)
	assert.Equal(t, EditPatched, res.Mode)
	assert.NotContains(t, string(res.Content), "resourceVersion", "dirty resourceVersion must be cleaned from Git")
	assert.Contains(t, string(res.Content), "color: blue")
}

// --- Test 6: Document-scoped update (hard) ---

func TestPatch_DocumentScopedUpdate(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  color: blue
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: other
  namespace: default
data:
  color: green
`
	doc0Before := docBody(content, 0)
	doc1Before := docBody(content, 1)

	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: other
  namespace: default
data:
  color: red
`)
	res, _ := patch([]byte(content), 1, desired)
	require.Equal(t, EditPatched, res.Mode)

	assert.Equal(t, doc0Before, docBody(string(res.Content), 0), "untouched document 0 must be byte-for-byte identical")
	assert.NotEqual(t, doc1Before, docBody(string(res.Content), 1))
	assert.Contains(t, docBody(string(res.Content), 1), "color: red")
}

// --- Test 7: Comment preservation ---

func TestPatch_CommentPreservation(t *testing.T) {
	content := `# app config
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config # stable name
  namespace: default
  labels:
    app: demo # selector label
data:
  color: blue
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  labels:
    app: demo
    tier: frontend
data:
  color: blue
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, "tier: frontend", "new label added")
	assert.Contains(t, out, "# app config", "head comment preserved")
	assert.Contains(t, out, "stable name", "comment on unchanged name field preserved")
	assert.Contains(t, out, "selector label", "comment on unchanged label preserved")
}

// --- Test 8: ConfigMap script block survives unrelated edits (hard) ---

func TestPatch_ScriptBlockSurvivesUnrelatedEdit(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-scripts
  namespace: default
data:
  start.sh: |-
    #!/bin/sh
    set -eu

    echo "starting app"
    exec /app/server
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-scripts
  namespace: default
  labels:
    app: demo
data:
  start.sh: |-
    #!/bin/sh
    set -eu

    echo "starting app"
    exec /app/server
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, "app: demo", "label added")
	const block = `  start.sh: |-
    #!/bin/sh
    set -eu

    echo "starting app"
    exec /app/server`
	assert.Contains(t, out, block, "the script block must survive byte-for-byte")
}

// --- Test 9: ConfigMap script block changes intentionally ---

func TestPatch_ScriptBlockChangesStayBlock(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-scripts
  namespace: default
data:
  start.sh: |-
    #!/bin/sh
    echo "old"
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-scripts
  namespace: default
data:
  start.sh: |-
    #!/bin/sh
    echo "new"
    echo "line two"
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, "start.sh: |-", "changed script should stay a literal block, not an escaped string")
	assert.Contains(t, out, `echo "new"`)
	assert.NotContains(t, out, `\n`, "must not become an escaped one-line string")
}

// --- Test 10: Quoted and string-like values ---

func TestPatch_QuotedValuesPreserved(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  build: "00123"
  enabled: "false"
`
	// Unrelated edit (add a label); the quoted string-like values must not change.
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  labels:
    app: demo
data:
  build: "00123"
  enabled: "false"
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, `build: "00123"`, "quoting preserved, not turned into a number")
	assert.Contains(t, out, `enabled: "false"`, "quoting preserved, not turned into a bool")
}

// --- Test 11: List item update ---

func TestPatch_ListItemImageUpdate(t *testing.T) {
	content := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.0
      - name: sidecar
        image: envoy:1.0
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  template:
    spec:
      containers:
      - name: web
        image: nginx:2.0
      - name: sidecar
        image: envoy:1.0
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, "image: nginx:2.0", "changed image updated")
	assert.Contains(t, out, "image: envoy:1.0", "unchanged image preserved")
}

// --- Test 12: Field deletion ---

func TestPatch_FieldDeletion(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  labels:
    app: demo # keep
    drop-me: yes # remove this label
data:
  color: blue
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  labels:
    app: demo
data:
  color: blue
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.NotContains(t, out, "drop-me", "removed label must be gone")
	assert.Contains(t, out, "app: demo", "sibling label preserved")
	assert.Contains(t, out, "color: blue", "unrelated field preserved")
}

// --- Test 13: Disallowed constructs are ignored, not materialized (hard) ---

func TestIndex_AnchorsAndAliasesIgnored(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  base: &base value
  copy: *base
`
	inv, diags := IndexFile("anchor.yaml", []byte(content))
	require.Len(t, inv.Records, 1)
	assert.False(t, inv.Records[0].Editable, "documents with anchors/aliases must be non-editable")

	_, ok := inv.Location(inv.Records[0].Identity)
	assert.False(t, ok, "a non-editable record must not become an authoritative location")

	var warned bool
	for _, d := range diags {
		if strings.Contains(d.Message, "not editable") {
			warned = true
		}
	}
	assert.True(t, warned)
}

func TestIndex_AliasBombDoesNotBlowUp(t *testing.T) {
	// A billion-laughs style alias bomb must be detected at the node level
	// without ever being materialized.
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: bomb
  namespace: default
data:
  a: &a ["x","x","x","x","x","x","x","x","x"]
  b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
  c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
  d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
  e: [*d,*d,*d,*d,*d,*d,*d,*d,*d]
`
	inv, _ := IndexFile("bomb.yaml", []byte(content))
	// It is indexed (identity readable) but never editable, and we never expanded it.
	// Using a plain ".yaml" path (not ".sops.yaml") keeps the record present: a
	// ".sops.yaml" file without a sops key is invalid and indexes to zero records,
	// which would make the assertion below pass vacuously.
	require.Len(t, inv.Records, 1, "alias-bomb input should still index when identity is readable")
	assert.False(t, inv.Records[0].Editable, "alias-bomb input must be marked non-editable without expansion")
}

func TestIndex_MergeKeyIgnored(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data: &d
  color: blue
extra:
  <<: *d
  shade: dark
`
	inv, _ := IndexFile("merge.yaml", []byte(content))
	require.Len(t, inv.Records, 1)
	assert.False(t, inv.Records[0].Editable, "merge keys must be non-editable")
}

// --- Test 14: Line-ending and boundary fidelity (hard) ---

func TestPatch_UnrelatedCRLFDocumentPreserved(t *testing.T) {
	// Document 0 uses CRLF and a BOM; an edit to document 1 must not touch it.
	doc0 := "\ufeffapiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n  name: crlf\r\n  namespace: default\r\ndata:\r\n  color: blue\r\n"
	doc1 := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: lf\n  namespace: default\ndata:\n  color: green\n"
	content := doc0 + "---\n" + doc1

	doc0Before := docBody(content, 0)

	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: lf
  namespace: default
data:
  color: red
`)
	res, _ := patch([]byte(content), 1, desired)
	require.Equal(t, EditPatched, res.Mode)

	assert.Equal(t, doc0Before, docBody(string(res.Content), 0),
		"CRLF+BOM document must survive an unrelated edit byte-for-byte")
}

func TestPatch_TrailingDocumentEndMarkerPreserved(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: blue
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
  namespace: default
data:
  color: green
...
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: red
`)
	res, _ := patch([]byte(content), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "...", "trailing document-end marker on an unrelated doc must survive")
}

// --- Test 15: Partially-encrypted manifest indexing ---

func TestIndex_SOPSPartialEncrypted(t *testing.T) {
	content := `apiVersion: v1
kind: Secret
metadata:
  name: db
  namespace: default
data:
  password: ENC[AES256_GCM,data:abc,iv:def,tag:ghi,type:str]
sops:
  age: []
  lastmodified: "2026-01-01T00:00:00Z"
`
	inv, _ := IndexFile("secret.sops.yaml", []byte(content))
	require.Len(t, inv.Records, 1)
	assert.True(t, inv.Records[0].Encrypted)
	assert.True(t, inv.Records[0].Editable)
}

func TestIndex_SOPSMissingSopsKeyInvalid(t *testing.T) {
	content := `apiVersion: v1
kind: Secret
metadata:
  name: db
  namespace: default
data:
  password: ENC[AES256_GCM,data:abc]
`
	inv, diags := IndexFile("secret.sops.yaml", []byte(content))
	assert.Empty(t, inv.Records, "a SOPS file without a sops key is invalid and not indexed")

	var invalid bool
	for _, d := range diags {
		if strings.Contains(d.Message, "without a sops key") {
			invalid = true
		}
	}
	assert.True(t, invalid)
}

func TestIndex_SOPSFullyEncryptedIdentityHiddenSkipped(t *testing.T) {
	// Fully encrypted: identity fields are not readable, so it cannot be sync material.
	content := `data: ENC[AES256_GCM,data:abcdef,iv:xyz,tag:t,type:str]
sops:
  age: []
`
	inv, diags := IndexFile("opaque.sops.yaml", []byte(content))
	assert.Empty(t, inv.Records, "a file without readable identity must be skipped")
	assert.NotEmpty(t, diags)
}
