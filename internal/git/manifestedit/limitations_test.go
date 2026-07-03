// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Known limitation: sequence matching is index-based, so reordering a list (same
// items, different order) rewrites it slot-by-slot. The result is semantically
// correct and converges, but a comment attached to a list item stays on its
// slot — it ends up on the wrong item. Kubernetes-aware keyed matching (e.g. by
// container `name`) would fix this; it is deliberately deferred. This test pins
// the current behavior so we notice when keyed matching changes it.
func TestPatch_KnownLimitation_ListReorderMisattributesComments(t *testing.T) {
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
	res, _ := patch([]byte(git), 0, desired)
	require.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)

	// Semantically correct: each container has the right image.
	assert.Contains(t, out, "name: web")
	assert.Contains(t, out, "name: sidecar")

	// Limitation: the sidecar comment migrated onto the (now slot-0) web image.
	assert.Contains(t, out, "image: nginx:1.0 # the mesh sidecar",
		"index-based matching mis-attributes the comment on reorder (keyed matching would fix this)")

	// It still converges: a second reconcile is a no-op.
	res2, _ := patch(res.Content, 0, desired)
	assert.Equal(t, EditNoChange, res2.Mode)
}

// Bounded-stats summary: inventory exposes high-level counts (and diagnostics can
// be grouped by level) so a status surface need not enumerate every manifest.
func TestInventory_SummaryAndDiagnosticCounts(t *testing.T) {
	good := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: default\n"
	anchor := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: anc\n  namespace: default\ndata:\n  a: &a 1\n  b: *a\n"
	encrypted := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: default\n" +
		"data:\n  p: ENC[AES256_GCM,data:x]\nsops:\n  age: []\n"

	inv, diags := IndexFiles([]FileContent{
		{Path: "a.yaml", Content: []byte(strings.Replace(good, "%s", "a", 1))},
		{Path: "b.yaml", Content: []byte(strings.Replace(good, "%s", "a", 1))}, // duplicate of a
		{Path: "anchor.yaml", Content: []byte(anchor)},                         // non-editable
		{Path: "secret.sops.yaml", Content: []byte(encrypted)},                 // encrypted
	})

	s := inv.Summary()
	assert.Equal(t, 4, s.Documents)
	assert.Equal(t, 1, s.NonEditable, "the anchor document")
	assert.Equal(t, 1, s.Encrypted, "the sops secret")
	assert.Equal(t, 1, s.Duplicates, "b.yaml lost to a.yaml")
	assert.Equal(t, 3, s.Editable, "two configmaps + the encrypted secret are editable records")

	counts := CountByLevel(diags)
	assert.GreaterOrEqual(t, counts[DiagWarning], 1, "duplicate and anchor warnings are counted")
}
