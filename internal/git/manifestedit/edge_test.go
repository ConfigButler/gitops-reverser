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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatch_IndexOutOfRange(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchDocument(content, 5, desired)
	assert.Equal(t, EditSkipped, res.Mode)
	assert.Equal(t, content, res.Content)
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagError, diags[0].Level)
}

func TestPatch_EmptyDocumentSkipped(t *testing.T) {
	content := []byte("# only a comment\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchDocument(content, 0, desired)
	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
}

func TestPatch_InvalidYAMLSkipped(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: [unterminated\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchDocument(content, 0, desired)
	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
}

func TestPatch_DisallowedDocumentSkipped(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  x: &x 1\n  y: *x\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchDocument(content, 0, desired)
	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
}

func TestPatch_NonMappingRootFallsBackToWholeReplace(t *testing.T) {
	// A top-level sequence is not a Kubernetes object; the editor cannot patch it
	// field-by-field and falls back to a whole-document render with a diagnostic.
	content := []byte("- a\n- b\n")
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  color: blue
`)

	res, diags := PatchDocument(content, 0, desired)
	assert.Equal(t, EditWholeReplace, res.Mode)
	assert.Contains(t, string(res.Content), "kind: ConfigMap")
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagWarning, diags[0].Level)
}

func TestPatch_SequenceItemRemovedAndAdded(t *testing.T) {
	content := []byte(`apiVersion: apps/v1
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
      - name: old
        image: old:1.0
`)
	// Remove the "old" container, add a "new" one: exercises truncate + append.
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
        image: nginx:1.0
      - name: new
        image: new:1.0
`)
	res, _ := PatchDocument(content, 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)
	assert.Contains(t, out, "name: new")
	assert.Contains(t, out, "image: new:1.0")
	assert.NotContains(t, out, "name: old")
}

func TestPatch_SequenceGrows(t *testing.T) {
	content := []byte(`apiVersion: apps/v1
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
`)
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
        image: nginx:1.0
      - name: sidecar
        image: envoy:1.0
`)
	res, _ := PatchDocument(content, 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "name: sidecar")
}

func TestIndex_ClusterScopedDuplicate(t *testing.T) {
	doc := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: team-a\n"
	inv, diags := IndexFiles([]FileContent{
		{Path: "b.yaml", Content: []byte(doc)},
		{Path: "a.yaml", Content: []byte(doc)},
	})

	loc, ok := inv.Location(Identity{APIVersion: "v1", Kind: "Namespace", Name: "team-a"})
	require.True(t, ok)
	assert.Equal(t, "a.yaml", loc.Path)

	var clusterMsg bool
	for _, d := range diags {
		if assert.ObjectsAreEqual(DiagWarning, d.Level) && containsAll(d.Message, "v1/Namespace/_cluster/team-a") {
			clusterMsg = true
		}
	}
	assert.True(t, clusterMsg, "cluster-scoped identity should render with _cluster")
}

func TestIndex_InvalidYAMLDiagnostic(t *testing.T) {
	inv, diags := IndexFile("broken.yaml", []byte("metadata: [unterminated\n"))
	assert.Empty(t, inv.Records)
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagError, diags[0].Level)
}

func TestPatch_TypeChangeScalarToMapAndSequence(t *testing.T) {
	content := []byte(`apiVersion: example.com/v1
kind: Widget
metadata:
  name: w
  namespace: default
spec:
  value: hello
  items: single
`)
	desired := mustObj(t, `apiVersion: example.com/v1
kind: Widget
metadata:
  name: w
  namespace: default
spec:
  value:
    nested: x
  items:
  - a
  - b
`)
	res, _ := PatchDocument(content, 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)
	assert.Contains(t, out, "nested: x", "scalar replaced by a map")
	assert.Contains(t, out, "- a", "scalar replaced by a sequence")
}

func TestIndex_LeadingSeparator(t *testing.T) {
	content := []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\n")
	inv, _ := IndexFile("lead.yaml", content)

	loc, ok := inv.Location(Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "a"})
	require.True(t, ok)
	assert.Equal(t, 0, loc.DocumentIndex, "a leading --- must not create a spurious empty document 0")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
