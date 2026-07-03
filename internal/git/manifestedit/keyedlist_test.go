// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyedByName injects the keyed list-match strategy used for KRM lists whose
// items are identified by their name field (containers, env, volumes, ...). The
// GVK->key choice lives with the caller; the document model only sees "match by
// this field".
func keyedByName() EditOptions {
	return EditOptions{Render: testRender, ListMatch: ListMatchStrategy{KeyField: "name"}}
}

// Keyed matching fixes the recorded index-based limitation: reordering a list
// matches each item to its counterpart by key, so a comment travels with its
// item instead of staying on the slot. Compare with
// TestPatch_KnownLimitation_ListReorderMisattributesComments, which pins the
// default index-based behavior.
func TestPatch_KeyedListReorderKeepsCommentWithItem(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: sidecar
    image: envoy:1.0 # the mesh sidecar
  - name: web
    image: nginx:1.0
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: web
    image: nginx:1.0
  - name: sidecar
    image: envoy:1.0
`)

	first := assertConverges(t, []byte(git), 0, desired, keyedByName())
	require.Equal(t, EditPatched, first.Mode)
	out := string(first.Content)

	// The comment stays attached to the sidecar, where it belongs.
	assert.Contains(t, out, "image: envoy:1.0 # the mesh sidecar",
		"keyed matching keeps the comment with its item across a reorder")
	assert.NotContains(t, out, "image: nginx:1.0 # the mesh sidecar",
		"the comment must not migrate onto the web container")

	// Git now reflects the desired order: web before sidecar.
	assert.Less(t, strings.Index(out, "name: web"), strings.Index(out, "name: sidecar"),
		"the list is rebuilt in desired order")
}

// Keyed matching applies field-level merges to the matched item, so editing one
// container's image leaves the other (and its comment) byte-stable.
func TestPatch_KeyedListEditsMatchedItemInPlace(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: web
    image: nginx:1.0
  - name: sidecar
    image: envoy:1.0 # the mesh sidecar
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: web
    image: nginx:2.0
  - name: sidecar
    image: envoy:1.0
`)

	first := assertConverges(t, []byte(git), 0, desired, keyedByName())
	require.Equal(t, EditPatched, first.Mode)
	out := string(first.Content)

	assert.Contains(t, out, "image: nginx:2.0", "the matched container is updated in place")
	assert.Contains(t, out, "image: envoy:1.0 # the mesh sidecar",
		"the untouched container keeps its comment")
}

// A desired item with no Git counterpart is added; a Git item absent from desired
// is dropped — matched by key, not slot.
func TestPatch_KeyedListAddsAndRemovesByKey(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
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
  containers:
  - name: web
    image: nginx:1.0
  - name: logger
    image: fluentd:1.0
`)

	first := assertConverges(t, []byte(git), 0, desired, keyedByName())
	require.Equal(t, EditPatched, first.Mode)
	out := string(first.Content)

	assert.Contains(t, out, "name: logger", "a desired-only item is added")
	assert.Contains(t, out, "name: web", "a matched item is kept")
	assert.NotContains(t, out, "name: sidecar", "a Git item absent from desired is dropped")
}

// When the list is not uniformly keyed (here, a list of scalars), keyed matching
// does not apply and the merge falls back to index-based matching, which still
// converges.
func TestPatch_KeyedListFallsBackToIndexForScalars(t *testing.T) {
	git := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
spec:
  items:
  - one
  - two
`
	desired := mustObj(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  namespace: default
spec:
  items:
  - one
  - two
  - three
`)

	first := assertConverges(t, []byte(git), 0, desired, keyedByName())
	require.Equal(t, EditPatched, first.Mode)
	assert.Contains(t, string(first.Content), "- three", "index fallback still appends the new scalar")
}

// A keyed no-op must stay a byte-stable no-op: same items in the same order with
// no field change means nothing is rewritten.
func TestPatch_KeyedListNoOpPreservesBytes(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: web
    image: nginx:1.0 # primary
  - name: sidecar
    image: envoy:1.0
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  containers:
  - name: web
    image: nginx:1.0
  - name: sidecar
    image: envoy:1.0
`)

	res, _ := PatchDocument([]byte(git), 0, desired, keyedByName())
	assert.Equal(t, EditNoChange, res.Mode)
	assert.Equal(t, git, string(res.Content), "an unchanged keyed list preserves bytes and comments")
}
