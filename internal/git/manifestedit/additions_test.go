/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifestedit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(s string, out interface{}) error {
	return yaml.Unmarshal([]byte(s), out)
}

// --- Gating corpus round-trip: every document must re-encode byte-for-byte. ---

func TestCorpusRoundTrip_ByteIdentical(t *testing.T) {
	files, err := filepath.Glob("testdata/corpus/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		for i, d := range splitDocuments(string(content)) {
			root, empty, err := decodeDoc(d.body)
			if empty || err != nil {
				continue
			}
			out, err := encodeNode(root)
			require.NoError(t, err)
			if string(out) != d.body {
				t.Errorf("round-trip drift in %s doc %d\n--- original ---\n%s\n--- re-encoded ---\n%s",
					filepath.Base(f), i, d.body, out)
			}
		}
	}
}

// TestRoundTrip_KnownDrift_FlushLeftSequence records a real, structured
// limitation: yaml.v3 normalizes flush-left sequence indentation to its own
// indented style. The edited document therefore is best-effort, not byte-exact,
// for that input style. This test fails loudly if the behavior ever changes.
func TestRoundTrip_KnownDrift_FlushLeftSequence(t *testing.T) {
	flushLeft := `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
spec:
  items:
  - one
  - two
`
	root, _, err := decodeDoc(flushLeft)
	require.NoError(t, err)
	out, err := encodeNode(root)
	require.NoError(t, err)

	if string(out) == flushLeft {
		t.Fatal("expected yaml.v3 to normalize flush-left sequence indentation; it did not — update the docs")
	}
	t.Logf("known drift (flush-left sequence) re-encoded as:\n%s", out)

	// It must still mean the same thing.
	var a, b map[string]interface{}
	require.NoError(t, yamlUnmarshal(flushLeft, &a))
	require.NoError(t, yamlUnmarshal(string(out), &b))
	assert.Equal(t, normalizeJSON(a), normalizeJSON(b))
}

// --- Edited-document framing fidelity (CRLF, BOM, trailing newline, ..., ---). ---

func TestPatch_EditedCRLFBOMDocumentKeepsFraming(t *testing.T) {
	doc := "\ufeffapiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n" +
		"  name: a\r\n  namespace: default\r\ndata:\r\n  color: blue\r\n"
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
  labels:
    app: demo
data:
  color: blue
`)
	res, _ := PatchDocument([]byte(doc), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.True(t, strings.HasPrefix(out, "\ufeff"), "BOM must survive editing the same document")
	assert.Contains(t, out, "\r\n", "CRLF must survive")
	assert.NotContains(t, strings.ReplaceAll(out, "\r\n", ""), "\n", "no bare LF should remain")
	assert.Contains(t, out, "app: demo", "the actual edit still applied")
}

func TestPatch_EditedDocumentNoTrailingNewlinePreserved(t *testing.T) {
	doc := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  color: blue"
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: red
`)
	res, _ := PatchDocument([]byte(doc), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.False(t, strings.HasSuffix(string(res.Content), "\n"), "missing trailing newline must stay missing")
	assert.Contains(t, string(res.Content), "color: red")
}

func TestPatch_EditedDocumentLeadingSeparatorPreserved(t *testing.T) {
	doc := "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  color: blue\n"
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: red
`)
	res, _ := PatchDocument([]byte(doc), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.True(t, strings.HasPrefix(string(res.Content), "---\n"), "leading --- must survive editing the document")
}

func TestPatch_EditedDocumentTrailingEndMarkerPreserved(t *testing.T) {
	doc := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  color: blue\n...\n"
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: red
`)
	res, _ := PatchDocument([]byte(doc), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.True(t, strings.HasSuffix(strings.TrimRight(string(res.Content), "\n"), "..."),
		"trailing ... marker must survive editing the document")
}

// --- Duplicate keys and unusual tags are disallowed. ---

func TestIndex_DuplicateKeyNonEditable(t *testing.T) {
	content := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  k: 1\n  k: 2\n"
	inv, diags := IndexFile("dup.yaml", []byte(content))

	for _, r := range inv.Records {
		assert.False(t, r.Editable, "a duplicate-key document must not be editable")
	}
	_, ok := inv.Location(Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "a"})
	assert.False(t, ok, "a duplicate-key document must not be an authoritative location")
	assert.NotEmpty(t, diags)
}

func TestPatch_DuplicateKeySkipped(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: 1\n  k: 2\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	res, diags := PatchDocument(content, 0, desired)
	assert.Equal(t, EditSkipped, res.Mode)
	assert.NotEmpty(t, diags)
}

func TestIndex_UnusualTagNonEditable(t *testing.T) {
	for _, tagged := range []string{
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: a\ndata:\n  p: !vault secret\n",
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: a\ndata:\n  p: !!binary aGk=\n",
	} {
		inv, _ := IndexFile("tag.yaml", []byte(tagged))
		require.Len(t, inv.Records, 1)
		assert.False(t, inv.Records[0].Editable, "unusual tags must be non-editable: %q", tagged)
	}
}

func TestIndex_PlainTimestampStillEditable(t *testing.T) {
	// An unquoted date resolves to !!timestamp but is perfectly normal; it must
	// not be treated as an unusual tag.
	content := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  when: not-a-date\nspec:\n  at: 2020-01-01T00:00:00Z\n"
	inv, _ := IndexFile("ts.yaml", []byte(content))
	require.Len(t, inv.Records, 1)
	assert.True(t, inv.Records[0].Editable, "a plain timestamp value must remain editable")
}

// --- Deletion of the matching document only. ---

func TestDelete_MiddleDocumentKeepsOthersByteIdentical(t *testing.T) {
	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
data:
  color: blue
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
data:
  color: green
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: c
data:
  color: red
`
	doc0 := docBody(content, 0)
	res, _ := DeleteDocument([]byte(content), 1)
	require.Equal(t, EditDeleted, res.Mode)
	assert.False(t, res.FileEmpty)

	assert.Equal(t, doc0, docBody(string(res.Content), 0), "untouched doc 0 must be byte-identical")
	assert.NotContains(t, string(res.Content), "name: b", "deleted document is gone")
	assert.Contains(t, string(res.Content), "name: c", "later document survives")

	// Result must still be valid, indexable YAML with the right documents.
	inv, _ := IndexFile("after.yaml", res.Content)
	assert.Len(t, inv.Records, 2)
}

func TestDelete_OnlyDocumentReportsFileEmpty(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	res, _ := DeleteDocument(content, 0)
	assert.Equal(t, EditDeleted, res.Mode)
	assert.True(t, res.FileEmpty, "removing the only document should signal file deletion")
}

func TestDelete_FirstDocumentDropsLeadingSeparator(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n")
	res, _ := DeleteDocument(content, 0)
	require.Equal(t, EditDeleted, res.Mode)
	assert.False(t, strings.HasPrefix(string(res.Content), "---"), "file should not start with a stray separator")
	assert.Contains(t, string(res.Content), "name: b")
	assert.NotContains(t, string(res.Content), "name: a")
}

func TestDelete_DuplicateLoserLocation(t *testing.T) {
	doc := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\n  namespace: default\n"
	winner := "apps/app.yaml"
	loserFile := "overlays/dev/app.yaml"
	inv, _ := IndexFiles([]FileContent{
		{Path: winner, Content: []byte(doc)},
		{Path: loserFile, Content: []byte(doc)},
	})
	require.Len(t, inv.Duplicates(), 1)
	loser := inv.Duplicates()[0].Location
	assert.Equal(t, loserFile, loser.Path)

	// Deleting the loser's only document empties that file.
	res, _ := DeleteDocument([]byte(doc), loser.DocumentIndex)
	assert.True(t, res.FileEmpty)
}

func TestDelete_IndexOutOfRange(t *testing.T) {
	res, diags := DeleteDocument([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"), 9)
	assert.Equal(t, EditSkipped, res.Mode)
	assert.NotEmpty(t, diags)
}

// --- Recursive scanning with symlink skipping. ---

func TestIndexDir_ScansRecursivelyAndSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: default\n"

	writeManifest := func(rel, name string) {
		require.NoError(t, os.WriteFile(
			filepath.Join(root, rel), []byte(strings.Replace(cm, "%s", name, 1)), 0o600))
	}
	writeManifest("a.yaml", "a")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o750))
	writeManifest(filepath.Join("sub", "b.yml"), "b")
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignore me"), 0o600))

	// A symlink to a real manifest must be skipped, not indexed twice.
	if err := os.Symlink(filepath.Join(root, "a.yaml"), filepath.Join(root, "link.yaml")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	inv, diags := IndexDir(root)

	_, okA := inv.Location(Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "a"})
	_, okB := inv.Location(Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "b"})
	assert.True(t, okA, "a.yaml indexed")
	assert.True(t, okB, "sub/b.yml indexed recursively")
	assert.Len(t, inv.Records, 2, "the symlink must not produce a duplicate record")

	var skipped bool
	for _, d := range diags {
		if strings.Contains(d.Message, "symlink skipped") {
			skipped = true
		}
	}
	assert.True(t, skipped, "a symlink-skipped diagnostic should be emitted")
}
