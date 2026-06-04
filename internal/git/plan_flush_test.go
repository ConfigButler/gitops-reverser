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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// cmEvent builds an event for an arbitrary ConfigMap identity/value.
func cmEvent(op, name, color string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"data":       map[string]interface{}{"color": color},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: "default", Name: name,
		},
		Operation: op,
	}
}

// A resource with no document in Git is created at its canonical placement path with
// the canonical rendered content.
func TestPlanFlush_CreatesNewResourceAtCanonicalPath(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := cmEvent("CREATE", "fresh", "green")
	changed := applyEventsViaPlanFlush(t, writer, worktree, event)
	require.True(t, changed, "a new resource must be written")

	canonical := filepath.Join(root, writer.filePathForIdentifier(event.Identifier))
	got, err := os.ReadFile(canonical)
	require.NoError(t, err)
	want, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "a new file is the canonical rendering")
}

// A DELETE event that carries no object (the production reconcile shape) is resolved
// to the canonical placement path: a single-document canonical file is removed.
func TestPlanFlush_DeleteWithoutObjectRemovesCanonicalFile(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	seed := cmEvent("CREATE", "app", "blue")
	canonicalRel := writer.filePathForIdentifier(seed.Identifier)
	content, err := writer.buildContentForWrite(context.Background(), seed)
	require.NoError(t, err)
	full := seedPlacedManifest(t, worktree, canonicalRel, string(content))

	del := Event{
		Identifier: seed.Identifier,
		Operation:  "DELETE",
		// No Object: the writer must fall back to the canonical placement path.
	}
	changed := applyEventsViaPlanFlush(t, writer, worktree, del)
	assert.True(t, changed, "the canonical file must be removed")
	_, statErr := os.Stat(full)
	assert.True(t, os.IsNotExist(statErr), "the canonical file must be deleted")
}

// Deleting one document from a multi-document file removes only that document and
// keeps the file (and its siblings) when documents remain. The delete event carries
// its object, so it content-matches the right document.
func TestPlanFlush_DeleteOneDocFromMultiDocKeepsSiblings(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	keep := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: keep\n  namespace: default\n" +
		"data:\n  k: v\n"
	drop := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: drop\n  namespace: default\n" +
		"data:\n  k: v\n"
	rel := "apps/multi.yaml"
	full := filepath.Join(root, rel)
	seedPlacedManifest(t, worktree, rel, keep+"---\n"+drop)

	del := cmEvent("DELETE", "drop", "v")
	changed := applyEventsViaPlanFlush(t, writer, worktree, del)
	require.True(t, changed, "the targeted document must be removed")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: keep", "the sibling document must survive")
	assert.NotContains(t, string(got), "name: drop", "the targeted document must be gone")
}

// Deleting a resource the folder never materialised is a no-op: nothing to remove,
// no change reported.
func TestPlanFlush_DeleteOfAbsentResourceIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	del := Event{Identifier: cmEvent("DELETE", "ghost", "").Identifier, Operation: "DELETE"}
	changed := applyEventsViaPlanFlush(t, writer, worktree, del)
	assert.False(t, changed, "deleting an absent resource changes nothing")
}

// A GVR-only DELETE event (no object body, the reconcile/orphan shape) still finds a
// manifest moved off its canonical path when a mapper is wired: the resource-identity
// index resolves the GVR to the document's content identity (M6's PlanDelete folded
// into the writer). This is the path the live-catalog mapper enables in production.
func TestPlanFlush_DeleteByGVROnlyFollowsMovedManifestViaMapper(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	// A ConfigMap a user moved off its canonical path.
	placedFull := seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	mapper := mapping.NewStaticSnapshotMapper(mapping.Snapshot{
		Entries: []mapping.Entry{{
			GVK:        schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
			Namespaced: true,
			Allowed:    true,
		}},
	})
	w := &BranchWorker{contentWriter: writer, mapper: mapper}

	// DELETE carrying only the GVR identity — no object body.
	del := Event{
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: "default", Name: "app",
		},
		Operation: "DELETE",
	}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{del})
	require.NoError(t, err)
	assert.True(t, changed, "the moved manifest must be deleted via the resolved resource identity")
	_, statErr := os.Stat(placedFull)
	assert.True(t, os.IsNotExist(statErr), "apps/foo.yaml must be deleted, not orphaned")
}

// groupEventsByBase buckets events by their sanitized GitTarget path, preserving
// arrival order within a bucket.
func TestGroupEventsByBase(t *testing.T) {
	a1 := Event{Path: "apps", Operation: "CREATE"}
	b1 := Event{Path: "infra", Operation: "CREATE"}
	a2 := Event{Path: "apps/", Operation: "DELETE"} // sanitizes to "apps"

	byBase := groupEventsByBase([]Event{a1, b1, a2})
	require.Len(t, byBase, 2)
	require.Len(t, byBase["apps"], 2)
	require.Len(t, byBase["infra"], 1)
	assert.Equal(t, "CREATE", byBase["apps"][0].Operation)
	assert.Equal(t, "DELETE", byBase["apps"][1].Operation, "arrival order is preserved within a base")
}
