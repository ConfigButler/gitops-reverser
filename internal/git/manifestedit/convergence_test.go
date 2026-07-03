// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assertConverges is the convergence property every edit strategy must satisfy:
// after the first Apply with a desired object, a second Decide returns NoChange
// and a second Apply is byte-identical, and it stays that way. The first write
// may normalize known drift (folded scalars reflow, flush-left sequences
// re-indent) and clean server fields; every reconcile after it must be a
// byte-stable no-op.
//
// The rule the vision leans on is exactly this: Decide after an Apply with the
// same desired must return NoChange. Wiring a new strategy (keyed lists, field
// ownership) through this helper is how it inherits the guarantee — it is the
// one property a strategy cannot quietly break. It returns the first EditResult
// so a caller can make additional first-write assertions.
func assertConverges(
	t *testing.T,
	git []byte,
	idx int,
	desired *unstructured.Unstructured,
	opts EditOptions,
) EditResult {
	t.Helper()

	first := applyOnce(git, idx, desired, opts)
	require.NotEqual(t, EditSkipped, first.Mode, "the document under test must be editable")

	// Decide after the first Apply, with the same desired, must be NoChange.
	c := Comparison{Git: gitDoc(first.Content, idx), Desired: desired, Options: opts}
	d := Decide(c)
	assert.Equal(t, ActionNoChange, d.Action, "second Decide must settle to NoChange")

	second, _ := Apply(c, d)
	assert.Equal(t, EditNoChange, second.Mode, "the next reconcile must be a no-op")
	assert.Equal(t, string(first.Content), string(second.Content), "and byte-stable")

	// And it stays converged.
	third := applyOnce(second.Content, idx, desired, opts)
	assert.Equal(t, EditNoChange, third.Mode, "convergence must hold across reconciles")
	assert.Equal(t, string(second.Content), string(third.Content))

	return first
}

// applyOnce runs one Decide + Apply over a fresh Comparison.
func applyOnce(content []byte, idx int, desired *unstructured.Unstructured, opts EditOptions) EditResult {
	c := Comparison{Git: gitDoc(content, idx), Desired: desired, Options: opts}
	res, _ := Apply(c, Decide(c))
	return res
}

// gitDoc builds a Document for the target index; the range flag is ignored
// because Decide reports an out-of-range index itself.
func gitDoc(content []byte, idx int) *Document {
	doc, _ := NewDocument(content, idx)
	return doc
}

// Convergence is the property the vision leans on: repeated reconciles must
// settle to "no change" without keeping a separate reconciliation-state layer.
// The first patch may normalize known drift (folded scalars reflow, flush-left
// sequences re-indent) and clean server fields (status), but every patch after
// that must be a byte-stable no-op.
func TestPatch_ConvergesAfterFirstWrite(t *testing.T) {
	git := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
spec:
  items:
  - one
  - two
  note: >-
    line one
    line two
status:
  replicas: 3
`
	desired := mustObj(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
  labels:
    app: demo
spec:
  items:
  - one
  - two
  note: >-
    line one
    line two
`)

	first := assertConverges(t, []byte(git), 0, desired, EditOptions{Render: testRender})
	require.Equal(t, EditPatched, first.Mode)
	assert.Contains(t, string(first.Content), "app: demo", "the change is applied")
	assert.NotContains(t, string(first.Content), "status:", "server-side status is cleaned from Git")
}

// A pure no-op never re-encodes, so even a folded scalar that would reflow on a
// real edit is preserved byte-for-byte when nothing actually changes.
func TestPatch_NoOpDoesNotReflowFoldedScalar(t *testing.T) {
	git := `apiVersion: v1
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
data:
  note: >-
    line one
    line two
`)
	res, _ := patch([]byte(git), 0, desired)
	assert.Equal(t, EditNoChange, res.Mode)
	assert.Equal(t, git, string(res.Content), "a no-op preserves the folded layout exactly")
}

// TestConvergence_Corpus gates the property across every editable document in
// the corpus: editing each one to a perturbed projection of itself must settle
// to a byte-stable no-op on the next reconcile. This is the guardrail any future
// strategy inherits — a regression announces itself by failing here.
func TestConvergence_Corpus(t *testing.T) {
	files, err := filepath.Glob("testdata/corpus/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	opts := EditOptions{Render: testRender}
	for _, f := range files {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		base := filepath.Base(f)

		for i, d := range splitDocuments(string(content)) {
			root, empty, decErr := decodeDoc(d.body)
			if empty || decErr != nil || root.Kind != yaml.MappingNode {
				continue
			}
			if _, ok := identityFromNode(root); !ok {
				continue
			}
			if _, bad := hasDisallowed(root); bad {
				continue
			}
			t.Run(fmt.Sprintf("%s/doc%d", base, i), func(t *testing.T) {
				// Perturb the projection with a new label so the first reconcile is a
				// real patch, then prove the next reconcile converges.
				desired := perturbWithLabel(t, d.body)
				assertConverges(t, content, i, desired, opts)
			})
		}
	}
}

// perturbWithLabel parses a document body into a desired object and adds a label,
// guaranteeing a real first-write patch for the convergence property to exercise.
func perturbWithLabel(t *testing.T, body string) *unstructured.Unstructured {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(body), &m))
	obj := &unstructured.Unstructured{Object: m}

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["convergence.test"] = "1"
	obj.SetLabels(labels)
	return obj
}
