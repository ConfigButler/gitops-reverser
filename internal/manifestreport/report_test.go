// SPDX-License-Identifier: Apache-2.0

package manifestreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A nil object in the desired set is skipped rather than panicking the reconcile.
func TestBuildReport_NilDesiredObjectSkipped(t *testing.T) {
	rep, _ := BuildReport(nil, []*unstructured.Unstructured{nil})
	assert.Empty(t, rep.Entries, "a nil desired object must be skipped, not classified")
}

// configMap builds a desired API object for a ConfigMap with one data key.
func configMap(name, color string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"data":       map[string]interface{}{"color": color},
	}}
}

// houseFile renders an object to the exact bytes the Git writer would commit, so
// a "no-change" verdict is genuinely byte-faithful (Git was written this way).
func houseFile(t *testing.T, path string, obj *unstructured.Unstructured) manifestedit.FileContent {
	t.Helper()
	content, err := Render(Project(obj))
	require.NoError(t, err)
	return manifestedit.FileContent{Path: path, Content: content}
}

// entryFor finds the report entry for an identity.
func entryFor(t *testing.T, r Report, name string) Entry {
	t.Helper()
	for _, e := range r.Entries {
		if e.Identity.Name == name {
			return e
		}
	}
	t.Fatalf("no report entry for %q", name)
	return Entry{}
}

// The read-only reconcile classifies every cell of the comparison: a resource
// unchanged in Git is a no-op, a changed one is an update, a cluster-only one is
// a create, and a Git-only one is a prune candidate.
func TestBuildReport_ClassifiesEveryCell(t *testing.T) {
	files := []manifestedit.FileContent{
		houseFile(t, "same.yaml", configMap("same", "blue")),       // present in both, identical
		houseFile(t, "changed.yaml", configMap("changed", "blue")), // present in both, different
		houseFile(t, "gone.yaml", configMap("gone", "blue")),       // only in Git -> delete
	}
	desired := []*unstructured.Unstructured{
		configMap("same", "blue"),     // matches Git -> no-change
		configMap("changed", "green"), // differs -> update
		configMap("new", "red"),       // not in Git -> create
	}

	report, diags := BuildReport(files, desired)
	assert.Empty(t, diagErrors(diags), "a clean corpus produces no error diagnostics")

	assert.Equal(t, ActionNoChange, entryFor(t, report, "same").Action)
	assert.Equal(t, ActionUpdate, entryFor(t, report, "changed").Action)
	assert.Equal(t, ActionCreate, entryFor(t, report, "new").Action)
	assert.Equal(t, ActionDelete, entryFor(t, report, "gone").Action)

	counts := report.Counts()
	assert.Equal(t, 1, counts[ActionNoChange])
	assert.Equal(t, 1, counts[ActionUpdate])
	assert.Equal(t, 1, counts[ActionCreate])
	assert.Equal(t, 1, counts[ActionDelete])
}

// The report is read-only: building it must not mutate the input file bytes.
func TestBuildReport_DoesNotMutateInput(t *testing.T) {
	original := houseFile(t, "cm.yaml", configMap("cm", "blue"))
	snapshot := append([]byte(nil), original.Content...)

	_, _ = BuildReport([]manifestedit.FileContent{original}, []*unstructured.Unstructured{configMap("cm", "green")})

	assert.Equal(t, string(snapshot), string(original.Content), "BuildReport must not change the input bytes")
}

// An encrypted document in the cluster's desired set is reported as a skip — it
// must go through the re-encrypt writer, never an in-place patch.
func TestBuildReport_EncryptedDocumentSkipped(t *testing.T) {
	enc := manifestedit.FileContent{Path: "secret.sops.yaml", Content: []byte(
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: default\n" +
			"data:\n  password: ENC[AES256_GCM,data:abc]\nsops:\n  age: []\n")}

	desired := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "db", "namespace": "default"},
		"data":     map[string]interface{}{"password": "hunter2"},
	}}

	report, _ := BuildReport([]manifestedit.FileContent{enc}, []*unstructured.Unstructured{desired})
	entry := entryFor(t, report, "db")
	assert.Equal(t, ActionSkip, entry.Action)
	assert.Contains(t, entry.Reason, "encrypted")
}

// A duplicate-identity document (same resource in two files) is reported as a
// prune candidate for the loser, even when the resource still exists.
func TestBuildReport_DuplicateIsPruneCandidate(t *testing.T) {
	cm := configMap("dup", "blue")
	files := []manifestedit.FileContent{
		houseFile(t, "apps/dup.yaml", cm),     // winner (lexicographically first)
		houseFile(t, "overlays/dup.yaml", cm), // duplicate loser
	}

	report, _ := BuildReport(files, []*unstructured.Unstructured{cm})

	var deletes []Entry
	for _, e := range report.Entries {
		if e.Action == ActionDelete {
			deletes = append(deletes, e)
		}
	}
	require.Len(t, deletes, 1, "exactly the duplicate loser is a prune candidate")
	assert.Equal(t, "overlays/dup.yaml", deletes[0].Location.Path)
}

// diagErrors filters diagnostics down to errors for assertions.
func diagErrors(diags []manifestedit.Diagnostic) []manifestedit.Diagnostic {
	var out []manifestedit.Diagnostic
	for _, d := range diags {
		if d.Level == manifestedit.DiagError {
			out = append(out, d)
		}
	}
	return out
}

// A non-editable Git document (here an anchor) is reported as skip, with the
// inventory's reason, and never as a delete or edit.
func TestBuildReport_NonEditableDocumentSkipped(t *testing.T) {
	anchor := manifestedit.FileContent{Path: "anchor.yaml", Content: []byte(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: anc\n  namespace: default\n" +
			"data:\n  a: &x 1\n  b: *x\n")}

	report, _ := BuildReport([]manifestedit.FileContent{anchor}, nil)
	entry := entryFor(t, report, "anc")
	assert.Equal(t, ActionSkip, entry.Action)
	assert.Contains(t, entry.Reason, "not editable")
}

// Multiple cluster-only resources all become creates; the report stays
// deterministically ordered even though creates share the zero Location.
func TestBuildReport_MultipleCreatesAreOrdered(t *testing.T) {
	report, _ := BuildReport(nil, []*unstructured.Unstructured{
		configMap("zebra", "x"),
		configMap("alpha", "y"),
	})
	require.Len(t, report.Entries, 2)
	assert.Equal(t, ActionCreate, report.Entries[0].Action)
	assert.Equal(t, "alpha", report.Entries[0].Identity.Name, "creates are ordered by identity")
	assert.Equal(t, "zebra", report.Entries[1].Identity.Name)
}

func TestActionFromDecision_Mapping(t *testing.T) {
	assert.Equal(t, ActionNoChange, actionFromDecision(manifestedit.ActionNoChange))
	assert.Equal(t, ActionUpdate, actionFromDecision(manifestedit.ActionPatch))
	assert.Equal(t, ActionUpdate, actionFromDecision(manifestedit.ActionReplace))
	assert.Equal(t, ActionDelete, actionFromDecision(manifestedit.ActionDelete))
	assert.Equal(t, ActionSkip, actionFromDecision(manifestedit.ActionSkip))
	assert.Equal(t, ActionSkip, actionFromDecision(manifestedit.DecisionAction("bogus")),
		"unknown is conservatively a skip")
}

func TestNonEditableReason_Fallback(t *testing.T) {
	assert.Equal(t, "not editable", nonEditableReason(manifestedit.DocumentRecord{}))
	assert.Equal(t, "not editable: anchor", nonEditableReason(manifestedit.DocumentRecord{Reason: "anchor"}))
}

func TestEditOptions_ProductionDefaults(t *testing.T) {
	opts := EditOptions()
	assert.NotNil(t, opts.Render, "the house renderer is injected")
	assert.Nil(t, opts.Owns, "whole-object truth: Owns must be nil in production")
	assert.Empty(t, opts.ListMatch.KeyField, "list matching stays index-based")
}
