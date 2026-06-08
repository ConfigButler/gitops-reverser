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

package git

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// deploymentScalePatch builds a deployments/scale-shaped field-patch event: no
// object, just a spec.replicas assignment against a parent Deployment identity.
func deploymentScalePatch(name string, replicas int64) Event {
	return Event{
		FieldPatch: &FieldPatch{
			Assignments: []manifestedit.FieldAssignment{
				{Path: []string{"spec", "replicas"}, Value: replicas},
			},
			ParentKind: "Deployment",
			Source:     "deployments/scale",
		},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: name,
		},
		Operation: "UPDATE",
	}
}

// A field-patch event scales an existing Deployment manifest in place: only
// spec.replicas changes, and the hand-authored comments, the selector, and the
// container spec all survive. The manifest is seeded off its canonical path to also
// prove it is located by content identity, not by path.
func TestPlanFlush_FieldPatchUpdatesExistingManifest(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	rel := "infra/web.yaml"
	seeded := "# web tier — replicas owned by GitOps\n" +
		"apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"metadata:\n  name: web\n  namespace: default\n  labels:\n    app: web\n" +
		"spec:\n  replicas: 1   # current scale\n" +
		"  selector:\n    matchLabels:\n      app: web\n" +
		"  template:\n    metadata:\n      labels:\n        app: web\n" +
		"    spec:\n      containers:\n        - name: web\n          image: nginx:1.25\n"
	full := seedPlacedManifest(t, worktree, rel, seeded)

	changed := applyEventsViaPlanFlush(t, writer, worktree, deploymentScalePatch("web", 3))
	require.True(t, changed, "scaling an existing manifest must write")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	out := string(got)
	assert.Contains(t, out, "replicas: 3", "the scaled value is applied")
	assert.NotContains(t, out, "replicas: 1")
	assert.Contains(t, out, "# current scale", "the inline comment survives the patch")
	assert.Contains(t, out, "# web tier — replicas owned by GitOps", "the header comment survives")
	assert.Contains(t, out, "image: nginx:1.25", "unrelated fields survive")
	assert.Contains(t, out, "matchLabels", "the selector survives")
}

// The production path: a translator-emitted field patch leaves ParentKind empty (the
// consumer cannot cheaply resolve GVR->GVK), so the writer must resolve the parent
// Deployment from its objectRef GVR through the mapper-built resource index — the same
// resolution the GVR-only delete uses — and patch only spec.replicas.
func TestPlanFlush_FieldPatchResolvesParentByGVRWhenKindUnset(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	rel := "infra/web.yaml"
	seeded := "apiVersion: apps/v1\nkind: Deployment\n" +
		"metadata:\n  name: web\n  namespace: default\n" +
		"spec:\n  replicas: 1\n  paused: false\n"
	full := seedPlacedManifest(t, worktree, rel, seeded)

	mapper := mapping.NewStaticSnapshotMapper(mapping.Snapshot{
		Entries: []mapping.Entry{{
			GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespaced: true,
			Allowed:    true,
		}},
	})
	w := &BranchWorker{contentWriter: writer, mapper: mapper}

	patch := Event{
		FieldPatch: &FieldPatch{
			Assignments: []manifestedit.FieldAssignment{{Path: []string{"spec", "replicas"}, Value: int64(4)}},
			Source:      "deployments/scale",
		},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "web",
		},
		Operation: "UPDATE",
	}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{patch})
	require.NoError(t, err)
	require.True(t, changed, "the parent must be resolved by GVR and patched")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "replicas: 4", "the scaled value is applied")
	assert.Contains(t, string(got), "paused: false", "an unrelated field is preserved")
}

// A field patch whose parent is not present in Git fabricates nothing: there is no
// creation path, because guessing every unaudited field would be worse than the drop.
func TestPlanFlush_FieldPatchWithNoParentIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	changed := applyEventsViaPlanFlush(t, writer, worktree, deploymentScalePatch("ghost", 3))
	assert.False(t, changed, "a field patch with no parent in Git writes nothing")
}

// An encrypted parent is never patched in place: that would drop the SOPS metadata and
// write cleartext. The document is left intact and nothing is committed.
func TestPlanFlush_FieldPatchEncryptedParentIsSkipped(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	rel := "infra/web.yaml"
	seeded := "apiVersion: apps/v1\nkind: Deployment\n" +
		"metadata:\n  name: web\n  namespace: default\n" +
		"spec:\n  replicas: 1\n" +
		"sops:\n  mac: ENC[placeholder]\n"
	full := seedPlacedManifest(t, worktree, rel, seeded)

	changed := applyEventsViaPlanFlush(t, writer, worktree, deploymentScalePatch("web", 3))
	assert.False(t, changed, "an encrypted parent is never patched in place")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "sops:", "the encrypted document is left intact")
	assert.NotContains(t, string(got), "replicas: 3")
}
