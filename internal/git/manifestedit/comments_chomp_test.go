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

// Comments: a standalone comment line above a field (head comment), and a
// trailing comment after a value (line comment), both on UNCHANGED nodes, are
// preserved exactly. A newly added sibling key is appended without a comment.
func TestPatch_Comments_HeadStandaloneAndTrailingPreserved(t *testing.T) {
	before := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  # this is a standalone comment about labels
  labels:
    app: demo # trailing comment on app
data:
  # comment above color
  color: blue # trailing on color
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  labels:
    app: demo
    tier: web
data:
  color: blue
`)
	after := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  # this is a standalone comment about labels
  labels:
    app: demo # trailing comment on app
    tier: web
data:
  # comment above color
  color: blue # trailing on color
`
	res, _ := PatchDocument([]byte(before), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.Equal(t, after, string(res.Content))
}

// Comments: a trailing comment on a node whose VALUE changes is carried over to
// the new value (best-effort comment preservation, and it works here).
func TestPatch_Comments_TrailingCommentSurvivesValueChange(t *testing.T) {
	before := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  color: blue # the brand color
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  color: green
`)
	after := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  color: green # the brand color
`
	res, _ := PatchDocument([]byte(before), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.Equal(t, after, string(res.Content))
}

// Comments: a head comment is attached to the field below it, so deleting that
// field removes its head comment too. A sibling's own comment is unaffected.
func TestPatch_Comments_HeadCommentDeletedWithItsField(t *testing.T) {
	before := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  labels:
    app: demo # keep this
    # this comment belongs to drop-me
    drop-me: "yes"
data:
  color: blue
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  labels:
    app: demo
data:
  color: blue
`)
	after := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  labels:
    app: demo # keep this
data:
  color: blue
`
	res, _ := PatchDocument([]byte(before), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	assert.Equal(t, after, string(res.Content))
}

// Block scalars: a LITERAL block with strip (|-) or clip (|) keeps its chomping
// indicator AND its exact line layout when an unrelated field changes. This is
// the ConfigMap-script guarantee.
func TestPatch_LiteralBlockChompPreservedOnUnrelatedEdit(t *testing.T) {
	for _, chomp := range []string{"|-", "|"} {
		before := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: default\n" +
			"data:\n  script: " + chomp + "\n    line one\n    line two\n"
		desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"+
			"  namespace: default\n  labels:\n    a: b\ndata:\n  script: "+chomp+"\n    line one\n    line two\n")

		res, _ := PatchDocument([]byte(before), 0, desired)
		require.Equal(t, EditPatched, res.Mode)
		out := string(res.Content)

		assert.Contains(t, out, "a: b", "%s: label added", chomp)
		assert.Contains(t, out, "  script: "+chomp+"\n    line one\n    line two",
			"%s: literal block keeps chomp indicator and exact layout", chomp)
	}
}

// Block scalars: the keep indicator (|+) is preserved when it is meaningful, i.e.
// when there are trailing blank lines to keep. Without trailing blanks, |+ is
// equivalent to | and yaml.v3 canonicalizes it to | (the string value is the
// same either way).
func TestPatch_LiteralKeepChompPreservedWhenMeaningful(t *testing.T) {
	// A trailing blank line inside the block is what |+ exists to preserve.
	before := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n  namespace: default\n" +
		"data:\n  script: |+\n    line one\n    line two\n\n"
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"+
		"  namespace: default\n  labels:\n    a: b\ndata:\n  script: |+\n    line one\n    line two\n\n")

	res, _ := PatchDocument([]byte(before), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)
	assert.Contains(t, out, "script: |+", "the keep indicator survives when there is a trailing blank to keep")
	assert.Contains(t, out, "    line one\n    line two", "block layout preserved")
}

// Block scalars: a FOLDED block (>, >-, >+) keeps its style and chomping and the
// same string VALUE, but yaml.v3 re-flows the line wrapping on re-encode. This is
// a recorded limitation: folded source layout is not byte-stable through an edit,
// even when the folded value itself does not change.
func TestPatch_FoldedBlockReflowsButKeepsValue(t *testing.T) {
	before := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
data:
  note: >-
    line one
    line two
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
  labels:
    a: b
data:
  note: >-
    line one
    line two
`)
	res, _ := PatchDocument([]byte(before), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	assert.Contains(t, out, "note: >-", "folded style and chomp are kept")
	assert.Contains(t, out, "line one line two", "yaml.v3 re-flows folded text onto one line")
	assert.NotContains(t, out, "    line one\n    line two", "original folded line layout is not preserved")

	// The decoded value is unchanged: folding "line one\nline two" yields the
	// same string either way.
	var got map[string]interface{}
	require.NoError(t, yamlUnmarshal(out, &got))
	data, _ := got["data"].(map[string]interface{})
	assert.Equal(t, "line one line two", data["note"], "folded string value is preserved")
}
