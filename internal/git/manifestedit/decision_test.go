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

// --- The two-version comparison as a first-class value ---

func TestDecide_NoChangeWhenGitMatchesDesired(t *testing.T) {
	doc := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: default\ndata:\n  color: blue\n"
	git, ok := NewDocument([]byte(doc), 0)
	require.True(t, ok)
	assert.Equal(t, Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "a"}, git.Identity)

	desired := mustObj(t, doc)
	d := Decide(Comparison{Git: git, Desired: desired})
	assert.Equal(t, ActionNoChange, d.Action)
	assert.NotEmpty(t, d.Snapshot.BodyHash)
}

func TestDecide_PatchWhenGitDiffers(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n")
	git, _ := NewDocument(content, 0)
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")
	assert.Equal(t, ActionPatch, Decide(Comparison{Git: git, Desired: desired}).Action)
}

func TestDecide_DeleteWhenDesiredAbsent(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	git, _ := NewDocument(content, 0)
	d := Decide(Comparison{Git: git, Desired: nil})
	assert.Equal(t, ActionDelete, d.Action)
}

func TestDecide_ReplaceWhenRootNotMapping(t *testing.T) {
	git, _ := NewDocument([]byte("- a\n- b\n"), 0)
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	assert.Equal(t, ActionReplace, Decide(Comparison{Git: git, Desired: desired}).Action)
}

// Git is required: a Comparison always describes an existing document. Creating a
// brand-new resource is a placement decision owned upstream, not a content edit.
func TestDecide_NilGitIsInvalid(t *testing.T) {
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	d := Decide(Comparison{Git: nil, Desired: desired})
	assert.Equal(t, ActionSkip, d.Action)

	res, diags := Apply(Comparison{Git: nil, Desired: desired}, d)
	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagError, diags[0].Level)
}

// Decide is a pure preflight: it must never mutate Git, or a "decision" could
// silently change Git before anything is applied.
func TestDecide_DoesNotMutateGit(t *testing.T) {
	original := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  replicas: 1\n  stale: drop\n"
	content := []byte(original)
	git, _ := NewDocument(content, 0)
	desired := mustObj(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  replicas: 2\n")

	_ = Decide(Comparison{Git: git, Desired: desired})
	assert.Equal(t, original, string(content), "Decide must not touch the underlying bytes")
	assert.Equal(t, original, string(git.Content))
}

// Apply re-parses c.Git and validates the snapshot, refusing if the target
// document drifted since Decide compared it. One source of truth; no stale edit
// applied to a changed shape.
func TestApply_SnapshotDriftSkips(t *testing.T) {
	before := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")

	gitBefore, _ := NewDocument(before, 0)
	d := Decide(Comparison{Git: gitBefore, Desired: desired})
	require.Equal(t, ActionPatch, d.Action)

	// The file changed out from under the decision.
	after := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: green\n")
	gitAfter, _ := NewDocument(after, 0)
	res, diags := Apply(Comparison{Git: gitAfter, Desired: desired, Options: EditOptions{Render: testRender}}, d)

	assert.Equal(t, EditSkipped, res.Mode)
	assert.Equal(t, after, res.Content, "a drifted document is left untouched")
	require.NotEmpty(t, diags)
	assert.Contains(t, diags[0].Message, "changed since decision")
}

// Apply enforces the full snapshot contract: a Decision carried to a different
// document index must not apply there, even if the bytes happen to match.
func TestApply_IndexMismatchSkips(t *testing.T) {
	doc := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n"
	// Two byte-identical documents: only the index distinguishes them.
	content := []byte(doc + "---\n" + doc)
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")

	git0, _ := NewDocument(content, 0)
	d := Decide(Comparison{Git: git0, Desired: desired})
	require.Equal(t, ActionPatch, d.Action)
	require.Equal(t, 0, d.Snapshot.DocumentIndex)

	// Apply the doc-0 decision against doc 1 (same bytes, so body hash alone would
	// not catch it). The explicit index check must.
	git1, _ := NewDocument(content, 1)
	res, diags := Apply(Comparison{Git: git1, Desired: desired, Options: EditOptions{Render: testRender}}, d)

	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
	assert.Contains(t, diags[0].Message, "decision was for document 0, applying to 1")
}

// Apply names the file in its diagnostics when the Document carries a path.
func TestApply_DiagnosticsCarryPath(t *testing.T) {
	git, _ := NewDocumentAt("apps/deploy.yaml", []byte("- a\n- b\n"), 0)
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	c := Comparison{Git: git, Desired: desired} // no renderer -> loud skip with a path
	_, diags := Apply(c, Decide(c))

	require.NotEmpty(t, diags)
	assert.Equal(t, "apps/deploy.yaml", diags[0].Path, "the diagnostic names the file, not just the index")
}

// A sibling document changing must not invalidate the target edit: the snapshot
// fingerprints the target document body, not the whole file.
func TestApply_SiblingChangeDoesNotBlockEdit(t *testing.T) {
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")
	target := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n"

	before := []byte(target + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sibling\ndata:\n  x: 1\n")
	gitBefore, _ := NewDocument(before, 0)
	d := Decide(Comparison{Git: gitBefore, Desired: desired})
	require.Equal(t, ActionPatch, d.Action)

	// Only the sibling (doc 1) changes; the target (doc 0) is byte-identical.
	after := []byte(target + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sibling\ndata:\n  x: 999\n")
	gitAfter, _ := NewDocument(after, 0)
	res, _ := Apply(Comparison{Git: gitAfter, Desired: desired, Options: EditOptions{Render: testRender}}, d)

	assert.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "color: red")
	assert.Contains(t, string(res.Content), "x: 999", "the sibling change survives")
}

// There is no silent production default: a path that needs canonical output with
// no renderer injected must fail loudly with a diagnostic.
func TestApply_ReplaceWithoutRendererFailsLoudly(t *testing.T) {
	git, _ := NewDocument([]byte("- a\n- b\n"), 0)
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n")
	c := Comparison{Git: git, Desired: desired} // no Options.Render
	res, diags := Apply(c, Decide(c))

	assert.Equal(t, EditSkipped, res.Mode)
	require.NotEmpty(t, diags)
	assert.Equal(t, DiagError, diags[0].Level)
	assert.Contains(t, diags[0].Message, "no Render injected")
}

// The preservation and delete paths need no renderer at all.
func TestApply_PatchAndDeleteNeedNoRenderer(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")

	git, _ := NewDocument(content, 0)
	patchC := Comparison{Git: git, Desired: desired} // no renderer
	patchRes, patchDiags := Apply(patchC, Decide(patchC))
	assert.Equal(t, EditPatched, patchRes.Mode)
	assert.Empty(t, patchDiags)

	delC := Comparison{Git: git, Desired: nil} // no renderer
	delRes, delDiags := Apply(delC, Decide(delC))
	assert.Equal(t, EditDeleted, delRes.Mode)
	assert.Empty(t, delDiags)
}

// The product policy is API-first, whole-object truth: production always passes
// Owns == nil, so a field absent from the desired projection is deleted from Git
// (see docs/future/manifestedit-field-ownership-spike.md). This is the only
// supported behavior, pinned here.
func TestApply_WholeObjectTruth_AbsentFieldIsDeleted(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\nextra: drop\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n")

	res, _ := PatchDocument(content, 0, desired, EditOptions{Render: testRender}) // Owns nil = own all
	assert.Equal(t, EditPatched, res.Mode)
	assert.NotContains(t, string(res.Content), "extra", "whole-object truth: an absent field is deleted from Git")
}

// TestApply_OwnsSeam_DormantMechanism exercises the dormant Owns seam directly.
// It is NOT a product feature: production must never set Owns (see the field
// ownership decision). This test exists only to keep the mechanism honest — an
// unowned path is left in Git — so the seam cannot silently rot. Do not read it
// as partial ownership being supported.
func TestApply_OwnsSeam_DormantMechanism(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\nextra: keep\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")

	owns := func(path FieldPath) bool { return len(path) != 1 || path[0] != "extra" }
	res, _ := PatchDocument(content, 0, desired, EditOptions{Render: testRender, Owns: owns})

	assert.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "color: red", "an owned field is still updated")
	assert.Contains(t, string(res.Content), "extra: keep", "the dormant seam leaves an unowned path in Git")
}
