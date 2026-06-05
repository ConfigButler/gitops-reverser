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

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// configMapMapper resolves v1/ConfigMap to a served, allowed resource, so the resync
// planner treats ConfigMap documents as watched, resolved, sweepable managed members.
func configMapMapper() mapping.ResourceMapper {
	return mapping.NewStaticSnapshotMapper(mapping.Snapshot{Entries: []mapping.Entry{{
		GVK:        schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespaced: true,
		Allowed:    true,
	}}})
}

// desiredCM builds a desired ConfigMap snapshot entry for the default namespace.
func desiredCM(name, color string) manifestanalyzer.DesiredResource {
	return manifestanalyzer.DesiredResource{
		Resource: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: "default", Name: name,
		},
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"data":       map[string]interface{}{"color": color},
		}},
	}
}

// cmManifest renders the canonical ConfigMap YAML used to seed the worktree.
func cmManifest(name, color string) string {
	return "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: " + name + "\n  namespace: default\n" +
		"data:\n  color: " + color + "\n"
}

// applyResyncViaWorktree drives the M8 resync apply for tests: it folds the desired
// snapshot over the worktree at base "" with the given mapper. A nil mapper makes the
// store structure-only (no resolved mappings), so no managed drop is ever planned.
func applyResyncViaWorktree(
	t *testing.T,
	writer *contentWriter,
	mapper mapping.ResourceMapper,
	worktree *gogit.Worktree,
	desired ...manifestanalyzer.DesiredResource,
) (ResyncStats, bool) {
	t.Helper()
	w := &BranchWorker{contentWriter: writer, mapper: mapper}
	stats, changed, err := w.applyResyncToWorktree(context.Background(), worktree, "", desired)
	require.NoError(t, err)
	return stats, changed
}

// A desired resource with no managed document in Git is created at its canonical
// placement path during resync.
func TestResync_CreatesMissingResource(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("api", "green"))
	require.True(t, changed, "a missing resource must be created")
	assert.Equal(t, 1, stats.Created)
	assert.Equal(t, 0, stats.Deleted)

	id := desiredCM("api", "green").Resource
	canonical := filepath.Join(root, writer.filePathForIdentifier(id))
	got, err := os.ReadFile(canonical)
	require.NoError(t, err)
	assert.Contains(t, string(got), "color: green")
}

// A managed document that differs from its desired resource is patched in place,
// preserving hand-authored formatting (the file-agnostic-placement guarantee).
func TestResync_UpdatesDriftedResourceInPlace(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	seeded := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: app\n  namespace: default\n" +
		"data:\n  # operator note kept across edits\n  color: blue\n"
	full := seedPlacedManifest(t, worktree, "apps/app.yaml", seeded)

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("app", "green"))
	require.True(t, changed, "a drifted resource must be updated")
	assert.Equal(t, 1, stats.Updated)
	assert.Equal(t, 0, stats.Created)
	assert.Equal(t, 0, stats.Deleted)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "color: green", "the field is rewritten")
	assert.Contains(t, string(got), "operator note kept", "the comment survives the in-place patch")
}

// A resync over a mirror already matching the cluster changes nothing and creates no
// commit-worthy diff.
func TestResync_InSyncResourceIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "apps/app.yaml", cmManifest("app", "blue"))

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("app", "blue"))
	assert.False(t, changed, "an in-sync resource is not rewritten")
	assert.Zero(t, stats.Created)
	assert.Zero(t, stats.Updated)
	assert.Zero(t, stats.Deleted)
}

// The core M8 behavior: a watched, resolved managed document absent from the desired
// snapshot is a managed drop. Its file is deleted (mark-and-sweep).
func TestResync_DropsManagedResourceAbsentFromCluster(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	keepFull := seedPlacedManifest(t, worktree, "apps/keep.yaml", cmManifest("keep", "blue"))
	dropFull := seedPlacedManifest(t, worktree, "apps/drop.yaml", cmManifest("drop", "blue"))

	// Desired snapshot holds only "keep"; "drop" left the cluster.
	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("keep", "blue"))
	require.True(t, changed, "the orphaned managed resource must be swept")
	assert.Equal(t, 1, stats.Deleted)

	_, keepErr := os.Stat(keepFull)
	require.NoError(t, keepErr, "the still-present resource is retained")
	_, dropErr := os.Stat(dropFull)
	assert.True(t, os.IsNotExist(dropErr), "the orphaned resource's file is deleted")
}

// A manifest a human moved off its canonical path is still swept by content identity
// when it is absent from the cluster — the moved-manifest disease M8 cures: the sweep
// targets the document's real RecordRef, not a regenerated canonical path.
func TestResync_DropsMovedManifestByContentIdentity(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	movedFull := seedPlacedManifest(t, worktree, "legacy/moved.yaml", cmManifest("app", "blue"))

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree)
	require.True(t, changed, "the moved orphan must be swept")
	assert.Equal(t, 1, stats.Deleted)
	_, statErr := os.Stat(movedFull)
	assert.True(t, os.IsNotExist(statErr), "the moved manifest is deleted at its real path, not orphaned")
}

// An empty desired snapshot (the cluster genuinely holds no watched resources) sweeps
// every managed resolved document — the authoritative "empty cluster, empty mirror".
func TestResync_EmptyClusterSweepsAllManaged(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	aFull := seedPlacedManifest(t, worktree, "apps/a.yaml", cmManifest("a", "x"))
	bFull := seedPlacedManifest(t, worktree, "apps/b.yaml", cmManifest("b", "y"))

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree)
	require.True(t, changed)
	assert.Equal(t, 2, stats.Deleted)
	for _, full := range []string{aFull, bFull} {
		_, err := os.Stat(full)
		assert.True(t, os.IsNotExist(err), "every managed document is swept")
	}
}

// Without a mapper the store is structure-only: no document resolves to a watched
// resource, so an empty desired snapshot sweeps nothing. This preserves the no-cluster
// safety promise — a resync can never drop what it could not classify as watched.
func TestResync_StructureOnlyNeverDrops(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	full := seedPlacedManifest(t, worktree, "apps/app.yaml", cmManifest("app", "blue"))

	stats, changed := applyResyncViaWorktree(t, writer, nil, worktree)
	assert.False(t, changed, "a structure-only resync drops nothing")
	assert.Zero(t, stats.Deleted)
	_, err := os.Stat(full)
	assert.NoError(t, err, "the unclassified document is left in place")
}

// One resync folds creates, in-place updates, and managed drops together over the same
// content-derived store, and reports each in its stats.
func TestResync_FoldsCreateUpdateDropTogether(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	keepFull := seedPlacedManifest(t, worktree, "apps/keep.yaml", cmManifest("keep", "blue"))
	dropFull := seedPlacedManifest(t, worktree, "apps/drop.yaml", cmManifest("drop", "blue"))

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree,
		desiredCM("keep", "green"), // drift -> update
		desiredCM("fresh", "red"),  // missing -> create
		// "drop" omitted -> managed drop
	)
	require.True(t, changed)
	assert.Equal(t, 1, stats.Created, "fresh is created")
	assert.Equal(t, 1, stats.Updated, "keep is updated")
	assert.Equal(t, 1, stats.Deleted, "drop is swept")

	keep, err := os.ReadFile(keepFull)
	require.NoError(t, err)
	assert.Contains(t, string(keep), "color: green")

	_, dropErr := os.Stat(dropFull)
	assert.True(t, os.IsNotExist(dropErr))

	freshCanonical := filepath.Join(root, writer.filePathForIdentifier(desiredCM("fresh", "red").Resource))
	_, freshErr := os.Stat(freshCanonical)
	assert.NoError(t, freshErr, "the created resource lands at its canonical path")
}

// A sensitive (SOPS) resource that the resync re-encrypts is counted as Updated, not
// Skipped. The planner marks an encrypted document PlanSkip (it cannot patch it in
// place), but applyUpsert re-encrypts and commits it — so stats must come from the
// actual apply, or status would report a real Secret update as skipped and the commit
// message count would omit it.
func TestResync_SensitiveUpdateCountsAsUpdatedNotSkipped(t *testing.T) {
	enc := &stubEncryptor{result: []byte(
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: default\n" +
			"data:\n  k: ENC[AES256,data:NEW,iv:cc,tag:dd]\nsops:\n  version: 3.9.0\n  mac: NEW\n")}
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	writer.setEncryptor(enc, "test-scope")
	worktree := newWorktreeForTest(t)

	// An already-encrypted Secret in Git whose encrypted bytes differ from the new render.
	seeded := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: default\n" +
		"data:\n  k: ENC[AES256,data:OLD,iv:aa,tag:bb]\nsops:\n  version: 3.9.0\n  mac: OLD\n"
	full := seedPlacedManifest(t, worktree, "secrets/app.sops.yaml", seeded)

	secretsMapper := mapping.NewStaticSnapshotMapper(mapping.Snapshot{Entries: []mapping.Entry{{
		GVK:        schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
		GVR:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		Namespaced: true,
		Allowed:    true,
	}}})
	desired := manifestanalyzer.DesiredResource{
		Resource: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "secrets", Namespace: "default", Name: "app",
		},
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "app", "namespace": "default"},
			"data":       map[string]interface{}{"k": "dg=="},
		}},
	}

	w := &BranchWorker{contentWriter: writer, mapper: secretsMapper}
	stats, changed, err := w.applyResyncToWorktree(
		context.Background(),
		worktree,
		"",
		[]manifestanalyzer.DesiredResource{desired},
	)
	require.NoError(t, err)
	require.True(t, changed, "the secret is re-encrypted")
	assert.Equal(t, 1, stats.Updated, "a re-encrypted sensitive resource is Updated, not Skipped")
	assert.Zero(t, stats.Created)
	assert.Zero(t, stats.Deleted)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Equal(t, string(enc.result), string(got), "the secret is re-encrypted in place")
}

// Sweeping one document from a multi-document file removes only that document and keeps
// the file when a managed sibling survives.
func TestResync_DropsOneDocFromMultiDocKeepsSiblings(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	rel := "apps/multi.yaml"
	full := filepath.Join(root, rel)
	seedPlacedManifest(t, worktree, rel, cmManifest("keep", "blue")+"---\n"+cmManifest("drop", "blue"))

	stats, changed := applyResyncViaWorktree(t, writer, configMapMapper(), worktree, desiredCM("keep", "blue"))
	require.True(t, changed)
	assert.Equal(t, 1, stats.Deleted)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: keep", "the surviving managed sibling is kept")
	assert.NotContains(t, string(got), "name: drop", "the orphaned document is removed")
}
