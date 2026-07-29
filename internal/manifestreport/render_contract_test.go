// SPDX-License-Identifier: Apache-2.0

package manifestreport

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// dirtyConfigMap is an API object carrying operational noise the projection
// strips: a status, a server-set resourceVersion, and an operational annotation.
func dirtyConfigMap() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            "app",
			"namespace":       "default",
			"resourceVersion": "12345",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"team": "payments",
			},
		},
		"data":   map[string]interface{}{"color": "blue"},
		"status": map[string]interface{}{"observedGeneration": int64(2)},
	}}
}

// The integration renderer must be byte-identical to the renderer the Git writer
// uses (internal/git/content_writer.go buildContentForWrite calls
// sanitize.MarshalToOrderedYAML on an already-sanitized object). If these ever
// diverge, whole-replace/new-file output would no longer match committed content.
func TestRender_MatchesWriterHouseFormat(t *testing.T) {
	raw := dirtyConfigMap()

	// What the writer would commit: MarshalToOrderedYAML on the sanitized object.
	want, err := sanitize.MarshalToOrderedYAML(sanitize.Sanitize(raw))
	require.NoError(t, err)

	got, err := Render(Project(raw))
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got), "the integration renderer must match the Git writer")
}

// Whole-document replacement through manifestedit, using the injected production
// options, must produce exactly the house format — proving new-file and
// fallback output stay in lockstep with the writer.
func TestRender_WholeReplaceMatchesHouseFormat(t *testing.T) {
	raw := dirtyConfigMap()
	want, err := sanitize.MarshalToOrderedYAML(sanitize.Sanitize(raw))
	require.NoError(t, err)

	// A top-level sequence is not a KRM object, forcing manifestedit to fall back
	// to a canonical whole-document render via the injected Render.
	res, diags := manifestedit.PatchDocument([]byte("- a\n- b\n"), 0, Project(raw), EditOptions())
	require.Equal(t, manifestedit.EditWholeReplace, res.Mode)
	require.NotEmpty(t, diags)

	// The file had a single document, so its whole content is that rendered body.
	assert.Equal(t, string(want), string(res.Content),
		"whole-replace output must be the house canonical format")
}

// listyObjects are the shapes the create-versus-update divergence actually showed up in:
// sequences of mappings, sequences of scalars, a nested sequence, and the scalar forms that
// each encoder renders its own way.
func listyObjects() map[string]*unstructured.Unstructured {
	return map[string]*unstructured.Unstructured{
		"a sequence of mappings": {Object: map[string]interface{}{
			"apiVersion": "example.com/v1",
			"kind":       "Thing",
			"metadata":   map[string]interface{}{"name": "app", "namespace": "demo"},
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"repositories": []interface{}{
					map[string]interface{}{"name": "a", "url": "https://example.com/a"},
					map[string]interface{}{"name": "b", "url": "https://example.com/b"},
				},
			},
		}},
		"a sequence of scalars": {Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "Role",
			"metadata":   map[string]interface{}{"name": "reader", "namespace": "demo"},
			"rules": []interface{}{
				map[string]interface{}{
					"apiGroups": []interface{}{""},
					"resources": []interface{}{"configmaps", "secrets"},
					"verbs":     []interface{}{"get", "list", "watch"},
				},
			},
		}},
		"a nested sequence": {Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":        "web",
				"namespace":   "demo",
				"labels":      map[string]interface{}{"app": "web"},
				"annotations": map[string]interface{}{"team": "payments"},
			},
			"spec": map[string]interface{}{
				"replicas": int64(2),
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "web",
								"image": "nginx:1.27",
								"ports": []interface{}{
									map[string]interface{}{"containerPort": int64(80), "name": "http"},
								},
								"args": []interface{}{"--verbose", "--port=80"},
							},
						},
					},
				},
			},
		}},
		"scalars an encoder can disagree about": {Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "scalars", "namespace": "demo"},
			"data": map[string]interface{}{
				"block":     "repositories:\n  - name: a\n    url: https://example.com/a\n",
				"empty":     "",
				"numberish": "0755",
				"yes":       "yes",
				"long": "a value long enough that an encoder may decide to fold it across " +
					"more than one line, which is exactly the kind of difference that churns a diff",
			},
		}},
	}
}

// TestCreateAndUpdateAgreeByteForByte is the gate consumer ask #11 asked for: the bytes a
// CREATE commits and the bytes an UPDATE commits for the same object must be identical, so a
// commit that changes one field diffs as one field.
//
// It pins the invariant rather than the style: take the canonical create output, patch a field
// in place the way the live writer does, and require the result to equal the canonical render
// of the patched object. Before the shared encoder, "a sequence of mappings" failed here with
// every list line rewritten — the create rendered dashes at the parent key's column and the
// patch two columns deeper — which is why an install churned all 19 `repositories` lines to
// carry one changed field.
//
// Every case changes an EXISTING field. Adding a key is deliberately excluded, and it is not
// a gap: a patch appends a new key where the document's own order puts it while a canonical
// render sorts keys, so the two legitimately differ there. Preserving the file's key order is
// what manifestedit is for, and normalizing it would rewrite documents a human wrote.
func TestCreateAndUpdateAgreeByteForByte(t *testing.T) {
	mutations := map[string]func(*unstructured.Unstructured){
		"a sequence of mappings": func(o *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(o.Object, int64(4), "spec", "replicas"))
		},
		"a sequence of scalars": func(o *unstructured.Unstructured) {
			rules, _, err := unstructured.NestedSlice(o.Object, "rules")
			require.NoError(t, err)
			rules[0].(map[string]interface{})["verbs"] = []interface{}{"get", "list"}
			require.NoError(t, unstructured.SetNestedSlice(o.Object, rules, "rules"))
		},
		"a nested sequence": func(o *unstructured.Unstructured) {
			containers, _, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
			require.NoError(t, err)
			containers[0].(map[string]interface{})["image"] = "nginx:1.28"
			require.NoError(t, unstructured.SetNestedSlice(o.Object, containers,
				"spec", "template", "spec", "containers"))
		},
		"scalars an encoder can disagree about": func(o *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(o.Object, "0644", "data", "numberish"))
		},
	}

	for name, obj := range listyObjects() {
		t.Run(name, func(t *testing.T) {
			mutate, ok := mutations[name]
			require.True(t, ok, "every fixture needs a mutation")

			created, err := Render(Project(obj))
			require.NoError(t, err)

			// The same file, read back and patched with an identical object: a faithful
			// edit must return the bytes it was given.
			unchanged, ok := EditInPlace("app.yaml", created, obj)
			require.True(t, ok, "an in-place edit of the writer's own output must be editable")
			assert.Equal(t, string(created), string(unchanged),
				"patching a document with no change rewrote it")

			changed := obj.DeepCopy()
			mutate(changed)

			wholeFile, err := Render(Project(changed))
			require.NoError(t, err)
			patched, ok := EditInPlace("app.yaml", created, changed)
			require.True(t, ok)
			assert.Equal(t, string(wholeFile), string(patched),
				"a create and an update of the same object serialize differently")
		})
	}
}

// TestUpdateOfCanonicalContentTouchesOnlyTheChangedLines is the user-visible half of the
// same ask, stated as a diff rather than as equality: an update to one field must not
// rewrite lines that did not change.
func TestUpdateOfCanonicalContentTouchesOnlyTheChangedLines(t *testing.T) {
	obj := listyObjects()["a sequence of mappings"]
	created, err := Render(Project(obj))
	require.NoError(t, err)

	changed := obj.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(changed.Object, int64(4), "spec", "replicas"))
	patched, ok := EditInPlace("app.yaml", created, changed)
	require.True(t, ok)

	before := strings.Split(string(created), "\n")
	after := strings.Split(string(patched), "\n")
	require.Len(t, after, len(before), "an update changed the line count")

	var changedLines []string
	for i := range before {
		if before[i] != after[i] {
			changedLines = append(changedLines, before[i]+" -> "+after[i])
		}
	}
	assert.Len(t, changedLines, 1, "one field changed but these lines moved: %v", changedLines)
}
