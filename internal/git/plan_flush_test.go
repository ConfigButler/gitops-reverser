// SPDX-License-Identifier: Apache-2.0

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

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
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

	mapper := typeset.NewSnapshotRegistry(typeset.Snapshot{
		Entries: []typeset.Entry{{
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

// A sensitive (SOPS) resource a user moved off its canonical .sops path must be
// re-encrypted wholesale AT ITS EXISTING PATH — never patched in place (which would
// leak the secret in cleartext) and never duplicated at the canonical path (which would
// orphan the moved copy). This pins the regression where sensitive upserts skipped
// content placement and always wrote the canonical path.
func TestPlanFlush_SensitiveMovedResourceRewritesInPlaceNotCanonical(t *testing.T) {
	enc := &stubEncryptor{result: []byte(
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: default\n" +
			"data:\n  k: ENC[AES256,data:NEW,iv:cc,tag:dd]\nsops:\n  version: 3.9.0\n  mac: NEW\n")}
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	writer.setEncryptor(enc, "test-scope")
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	// A Secret moved off its canonical .sops path. It carries a cleartext identity and a
	// sops key, so the store indexes it as an encrypted managed document. The encrypted
	// data differs from the new render, so the write is a real change, not a no-op.
	movedRel := "secrets/app.sops.yaml"
	seeded := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: default\n" +
		"data:\n  k: ENC[AES256,data:OLD,iv:aa,tag:bb]\nsops:\n  version: 3.9.0\n  mac: OLD\n"
	movedFull := seedPlacedManifest(t, worktree, movedRel, seeded)

	event := Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]interface{}{"name": "app", "namespace": "default"},
			"data":     map[string]interface{}{"k": "dg=="},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "secrets", Namespace: "default", Name: "app",
		},
		Operation: "UPDATE",
	}
	changed := applyEventsViaPlanFlush(t, writer, worktree, event)
	require.True(t, changed)

	got, err := os.ReadFile(movedFull)
	require.NoError(t, err)
	assert.Equal(t, string(enc.result), string(got), "the moved secret is re-encrypted at its existing path")

	canonicalFull := filepath.Join(root, writer.filePathForIdentifier(event.Identifier))
	_, statErr := os.Stat(canonicalFull)
	assert.Truef(t, os.IsNotExist(statErr),
		"no duplicate secret must be created at the canonical .sops path %s", canonicalFull)
}

// Within one batch, deleting a document from a multi-document file shifts the indices of
// the documents after it. A later event updating a surviving sibling must target it by
// its CURRENT position, not the stale pre-batch index — otherwise the update is dropped
// (index out of range) or lands on the wrong document.
func TestPlanFlush_BatchDeleteThenUpdateSiblingTargetsCorrectDoc(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	rel := "apps/multi.yaml"
	first := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: first\n  namespace: default\ndata:\n  k: v\n"
	second := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: second\n  namespace: default\ndata:\n  color: blue\n"
	full := filepath.Join(root, rel)
	seedPlacedManifest(t, worktree, rel, first+"---\n"+second)

	// Event A deletes document 0 ("first"); event B updates "second", originally at
	// document 1 but at document 0 once "first" is gone.
	delFirst := cmEvent("DELETE", "first", "v")
	updSecond := cmEvent("UPDATE", "second", "green")
	changed := applyEventsViaPlanFlush(t, writer, worktree, delFirst, updSecond)
	require.True(t, changed)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.NotContains(t, string(got), "name: first", "the deleted document is gone")
	assert.Contains(t, string(got), "name: second", "the surviving document remains")
	assert.Contains(t, string(got), "color: green", "the update must land on the correct sibling")
	assert.NotContains(t, string(got), "color: blue", "the stale-index bug would have left the old value")
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
