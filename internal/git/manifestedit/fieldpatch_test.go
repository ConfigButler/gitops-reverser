// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// handFormattedDeployment is a manifest written the way a human would: a document
// header comment, an inline comment on replicas, a label comment, deliberate key
// order, and a block-sequence container. The whole point of a field patch is that
// scaling it changes ONLY spec.replicas and preserves all of this.
const handFormattedDeployment = `# Production web tier — GitOps owns spec.replicas; do not hand-edit.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels:
    app: web            # owned by team storefront
spec:
  replicas: 1           # current desired scale
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: nginx:1.25
`

// TestPatchFields_ScaleRoundTrip is the headline spike: the captured
// deployments/scale audit event reduces to one assignment, spec.replicas: 3, and
// applying it as a field patch updates exactly that value while leaving every
// comment, sibling field, and the block-sequence container byte-stable. This is
// the proof the subresource design rests on — no hydration, no whole-document
// rewrite, just the audited field landing on the committed manifest.
func TestPatchFields_ScaleRoundTrip(t *testing.T) {
	id := Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"}
	assignments := []FieldAssignment{{Path: []string{"spec", "replicas"}, Value: int64(3)}}

	res, diags := PatchFields([]byte(handFormattedDeployment), 0, id, assignments, EditOptions{Render: testRender})

	assert.Empty(t, diags, "a clean field patch emits no diagnostics")
	assert.Equal(t, EditPatched, res.Mode)

	out := string(res.Content)
	assert.Contains(t, out, "replicas: 3", "the audited field is updated")
	assert.NotContains(t, out, "replicas: 1", "the old value is gone")
	// Everything the patch did not name survives, including comments and style.
	assert.Contains(t, out, "# Production web tier", "document header comment preserved")
	assert.Contains(t, out, "# current desired scale", "inline comment travels with the changed scalar")
	assert.Contains(t, out, "# owned by team storefront", "unrelated label comment preserved")
	assert.Contains(t, out, "matchLabels", "selector preserved")
	assert.Contains(t, out, "image: nginx:1.25", "container spec preserved")
	assert.Contains(t, out, "- name: web", "block-sequence container preserved")
}

// TestPatchFields_NoOpWhenValueMatches proves a redundant patch is a true no-op:
// scaling to the value Git already holds rewrites nothing.
func TestPatchFields_NoOpWhenValueMatches(t *testing.T) {
	content := []byte("apiVersion: apps/v1\nkind: Deployment\n" +
		"metadata:\n  name: web\n  namespace: shop\nspec:\n  replicas: 3\n")
	id := Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"}

	res, _ := PatchFields(content, 0, id,
		[]FieldAssignment{{Path: []string{"spec", "replicas"}, Value: int64(3)}}, EditOptions{Render: testRender})

	assert.Equal(t, EditNoChange, res.Mode)
	assert.Equal(t, string(content), string(res.Content), "no node changed, so bytes are identical")
}

// TestPatchFields_LeavesUnassignedFields is the defining contrast with the
// whole-object merge: a field the patch never mentions is NOT deleted, where
// whole-object truth (Owns nil) would drop it.
func TestPatchFields_LeavesUnassignedFields(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n  shape: round\n")
	id := Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}

	res, diags := PatchFields(content, 0, id,
		[]FieldAssignment{{Path: []string{"data", "color"}, Value: "red"}}, EditOptions{Render: testRender})

	assert.Empty(t, diags)
	assert.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "color: red", "the assigned field is updated")
	assert.Contains(t, string(res.Content), "shape: round", "an unassigned sibling survives a field patch")
}

// TestPatchFields_MapValueReplacesSubtree shows the descendant-ownership rule: a
// map-valued assignment owns its subtree, so sub-keys absent from the new value
// are pruned — "set data to exactly this map" rather than "merge into data".
func TestPatchFields_MapValueReplacesSubtree(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n  shape: round\n")
	id := Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}

	res, _ := PatchFields(content, 0, id,
		[]FieldAssignment{{Path: []string{"data"}, Value: map[string]interface{}{"color": "red"}}},
		EditOptions{Render: testRender})

	assert.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "color: red")
	assert.NotContains(t, string(res.Content), "shape", "the owned subtree is replaced, pruning its absent sub-key")
}

// TestPatchFields_MultipleFields proves a single patch can carry several
// assignments — a subresource that mutates more than one parent field lands all
// of them in one edit, while every unassigned sibling stays put. This is the
// natural shape of the generic rule, which emits one assignment per spec leaf.
func TestPatchFields_MultipleFields(t *testing.T) {
	content := []byte("apiVersion: apps/v1\nkind: Deployment\n" +
		"metadata:\n  name: web\n  namespace: shop\n  labels:\n    app: web\n" +
		"spec:\n  replicas: 1\n  minReadySeconds: 0\n  paused: false\n")
	id := Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"}

	assignments := []FieldAssignment{
		{Path: []string{"spec", "replicas"}, Value: int64(3)},
		{Path: []string{"spec", "paused"}, Value: true},
	}
	res, diags := PatchFields(content, 0, id, assignments, EditOptions{Render: testRender})

	assert.Empty(t, diags)
	assert.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)
	assert.Contains(t, out, "replicas: 3", "first assignment landed")
	assert.Contains(t, out, "paused: true", "second assignment landed")
	assert.Contains(t, out, "minReadySeconds: 0", "an unassigned spec sibling is untouched")
	assert.Contains(t, out, "app: web", "an unassigned subtree is untouched")
}

// TestPatchFields_MultipleFieldsAcrossSubtrees proves assignments in different
// subtrees (data and metadata.annotations) coexist in one patch, each adding to
// its map without disturbing the existing keys around it.
func TestPatchFields_MultipleFieldsAcrossSubtrees(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: a\n  annotations:\n    keep: \"yes\"\n" +
		"data:\n  color: blue\n  shape: round\n")
	id := Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}

	assignments := []FieldAssignment{
		{Path: []string{"data", "color"}, Value: "red"},
		{Path: []string{"metadata", "annotations", "team"}, Value: "storefront"},
	}
	res, _ := PatchFields(content, 0, id, assignments, EditOptions{Render: testRender})

	assert.Equal(t, EditPatched, res.Mode)
	out := string(res.Content)
	assert.Contains(t, out, "color: red", "data assignment landed")
	assert.Contains(t, out, "team: storefront", "annotation assignment landed")
	assert.Contains(t, out, "keep: \"yes\"", "the existing annotation is preserved beside the new one")
	assert.Contains(t, out, "shape: round", "the unassigned data key is preserved")
}

// TestPatchFields_MultiDocOnlyTargetChanges proves a field patch touches only its
// target document and leaves siblings in the same file byte-for-byte identical.
func TestPatchFields_MultiDocOnlyTargetChanges(t *testing.T) {
	content := []byte(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keep\ndata:\n  v: original\n" +
			"---\n" +
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: target\ndata:\n  v: original\n")
	before0 := docBody(string(content), 0)

	res, _ := PatchFields(content, 1, Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "target"},
		[]FieldAssignment{{Path: []string{"data", "v"}, Value: "patched"}}, EditOptions{Render: testRender})

	assert.Equal(t, EditPatched, res.Mode)
	assert.Equal(t, before0, docBody(string(res.Content), 0), "the sibling document is untouched")
	assert.Contains(t, docBody(string(res.Content), 1), "v: patched")
}

// TestPatchFields_MissingDocumentSkips proves an out-of-range target is a soft
// skip with a diagnostic, not a panic — the "no parent in Git" caller path.
func TestPatchFields_MissingDocumentSkips(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchFields(content, 5, Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
		[]FieldAssignment{{Path: []string{"data", "x"}, Value: "y"}}, EditOptions{Render: testRender})

	assert.Equal(t, EditSkipped, res.Mode)
	assert.NotEmpty(t, diags)
}

// TestPatchFields_EncryptedSkips proves a field patch inherits the encrypted-
// document refusal: a SOPS document is skipped, never cleartext-patched in place.
func TestPatchFields_EncryptedSkips(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: a\ndata:\n  k: ZW5j\nsops:\n  mac: x\n")

	res, _ := PatchFields(content, 0, Identity{APIVersion: "v1", Kind: "Secret", Name: "a"},
		[]FieldAssignment{{Path: []string{"data", "k"}, Value: "dgo="}}, EditOptions{Render: testRender})

	assert.Equal(t, EditSkipped, res.Mode)
	assert.Contains(t, string(res.Content), "sops:", "the encrypted document is left intact")
}

// TestPatchFields_InvalidAssignmentSkips proves a malformed assignment surfaces a
// loud error diagnostic instead of editing anything.
func TestPatchFields_InvalidAssignmentSkips(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")

	res, diags := PatchFields(content, 0, Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
		[]FieldAssignment{{Path: nil, Value: 1}}, EditOptions{Render: testRender})

	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagError, diags[0].Level)
}

func TestPartialDesired(t *testing.T) {
	id := Identity{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"}

	obj, err := PartialDesired(id, []FieldAssignment{{Path: []string{"spec", "replicas"}, Value: int64(3)}})
	require.NoError(t, err)

	assert.Equal(t, "apps/v1", obj.GetAPIVersion())
	assert.Equal(t, "Deployment", obj.GetKind())
	assert.Equal(t, "web", obj.GetName())
	assert.Equal(t, "shop", obj.GetNamespace())
	replicas, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(3), replicas)
}

func TestPartialDesired_ClusterScopedOmitsNamespace(t *testing.T) {
	obj, err := PartialDesired(Identity{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "view"},
		[]FieldAssignment{{Path: []string{"spec", "x"}, Value: "y"}})
	require.NoError(t, err)

	_, found, _ := unstructured.NestedString(obj.Object, "metadata", "namespace")
	assert.False(t, found, "a cluster-scoped identity gains no spurious namespace key")
}

func TestPartialDesired_Errors(t *testing.T) {
	_, err := PartialDesired(Identity{Kind: "Deployment"}, nil)
	require.Error(t, err, "missing APIVersion is rejected")

	_, err = PartialDesired(Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
		[]FieldAssignment{{Path: nil, Value: 1}})
	require.Error(t, err, "an empty assignment path is rejected")

	_, err = PartialDesired(Identity{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}, []FieldAssignment{
		{Path: []string{"spec", "replicas"}, Value: int64(3)},
		{Path: []string{"spec", "replicas", "deep"}, Value: 1},
	})
	require.Error(t, err, "overlapping assignments are rejected")
}

func TestOwnsAssignedPaths(t *testing.T) {
	owns := OwnsAssignedPaths([]FieldAssignment{{Path: []string{"spec", "replicas"}}})

	assert.True(t, owns(FieldPath{"spec", "replicas"}), "the assigned path is owned")
	assert.True(t, owns(FieldPath{"spec", "replicas", "deep"}), "a descendant of an assigned path is owned")
	assert.False(t, owns(FieldPath{"spec"}), "an ancestor of an assigned path is not owned")
	assert.False(t, owns(FieldPath{"spec", "selector"}), "a sibling of an assigned path is not owned")
	assert.False(t, owns(FieldPath{"status"}), "an unrelated path is not owned")
}
