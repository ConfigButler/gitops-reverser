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

// The Owns predicate is the field-ownership seam: a field present in Git but
// absent from desired is deleted only when owned. The default (nil) owns all.
func TestApply_UnownedFieldIsPreserved(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\nextra: keep\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: red\n")

	owns := func(path FieldPath) bool { return len(path) != 1 || path[0] != "extra" }
	res, _ := PatchDocument(content, 0, desired, EditOptions{Render: testRender, Owns: owns})

	assert.Equal(t, EditPatched, res.Mode)
	assert.Contains(t, string(res.Content), "color: red", "an owned field is still updated")
	assert.Contains(t, string(res.Content), "extra: keep", "an unowned field is left in Git")
}

func TestApply_OwnedFieldIsDeletedByDefault(t *testing.T) {
	content := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\nextra: drop\n")
	desired := mustObj(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  color: blue\n")

	res, _ := PatchDocument(content, 0, desired, EditOptions{Render: testRender})
	assert.Equal(t, EditPatched, res.Mode)
	assert.NotContains(t, string(res.Content), "extra", "the default owns all, so an absent field is deleted")
}
