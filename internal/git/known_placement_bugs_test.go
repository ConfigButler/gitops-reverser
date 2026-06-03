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

// These tests guard two writer defects that were fixed on this branch. They
// started red (as the executable spec for the fix) and now pin the corrected
// behavior so it cannot regress:
//
//   - High:   the production writer must be file-agnostic. applyEventToWorktree
//     asks the inventory where the resource already lives (match-first) and edits
//     or deletes it there; only a genuinely new resource falls back to the
//     deterministic identity path. A manifest a user placed at apps/foo.yaml is
//     updated and deleted in place, never duplicated at the canonical path.
//
//   - Medium: an in-place no-op must report no change. When the whole-file
//     semantic guard diverges from manifestedit's per-document decision (multi-doc
//     files), handleCreateOrUpdateOperation must not stage identical bytes and
//     return true, which the commit executor would treat as commit-worthy.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// a ConfigMap manifest a user committed at a hand-chosen, non-canonical path.
const placedManifestPath = "apps/foo.yaml"

const placedManifestBlue = "apiVersion: v1\nkind: ConfigMap\n" +
	"metadata:\n  name: app\n  namespace: default\n" +
	"data:\n  color: blue\n"

// seedPlacedManifest writes a manifest at relPath and stages it, modelling a
// resource that already lives (committed) at that location in Git. It returns the
// absolute path so callers can assert on the file afterwards.
func seedPlacedManifest(t *testing.T, worktree *gogit.Worktree, relPath, content string) string {
	t.Helper()
	full := filepath.Join(worktree.Filesystem.Root(), relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	_, err := worktree.Add(relPath)
	require.NoError(t, err)
	return full
}

// HIGH BUG (fixed) — flexible placement, update path.
//
// A user placed the ConfigMap at a hand-chosen path (apps/foo.yaml), not the
// canonical identity path. A cluster update must edit the resource where it
// already lives and must NOT spawn a duplicate at the canonical path.
func TestApplyEvent_UpdateMustFollowExistingPlacement(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	placedFull := seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	event := inplaceCMEvent("green")
	_, err := applyEventToWorktree(context.Background(), writer, event, newManifestLocator(worktree))
	require.NoError(t, err)

	placedAfter, err := os.ReadFile(placedFull)
	require.NoError(t, err)
	assert.Contains(t, string(placedAfter), "color: green",
		"the update must land in the existing manifest at apps/foo.yaml")

	canonicalFull := filepath.Join(worktree.Filesystem.Root(), writer.filePathForIdentifier(event.Identifier))
	_, statErr := os.Stat(canonicalFull)
	assert.Truef(t, os.IsNotExist(statErr),
		"no duplicate copy must be created at the canonical path %s", canonicalFull)
}

// HIGH BUG (fixed) — flexible placement, delete path.
//
// Deleting the resource from the cluster must remove the manifest where it
// actually lives (apps/foo.yaml), not leave it orphaned because the deterministic
// path no longer matches. The delete event here carries the object, so the writer
// can content-match it to its real location.
func TestApplyEvent_DeleteMustFollowExistingPlacement(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	placedFull := seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	event := inplaceCMEvent("blue")
	event.Operation = "DELETE"
	changed, err := applyEventToWorktree(context.Background(), writer, event, newManifestLocator(worktree))
	require.NoError(t, err)

	assert.True(t, changed, "deleting the resource must remove the manifest where it lives")
	_, statErr := os.Stat(placedFull)
	assert.True(t, os.IsNotExist(statErr), "apps/foo.yaml must be deleted, not orphaned")
}

// MEDIUM BUG (fixed) — an in-place no-op must report no change.
//
// Multi-doc file: an unrelated resource is the first document, and the SECOND
// document is the exact canonical rendering of the watched object — so editing
// it in place is a genuine no-op. The whole-file semantic guard only canonicalizes
// the first document (sigs.k8s.io/yaml decodes one doc), so it does not short-
// circuit; the in-place editor returns the file unchanged, and
// handleCreateOrUpdateOperation must report changed=false rather than drive an
// empty commit.
func TestHandleCreateOrUpdate_NoOpInMultiDocReportsNoChange(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("blue")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	// The target document, byte-for-byte as the operator would render it.
	targetDoc, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)

	// An unrelated, editable resource as the first document of the file.
	other := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: other\n  namespace: default\n" +
		"data:\n  k: v\n"
	seeded := other + "---\n" + string(targetDoc)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(seeded), 0o600))

	before, err := os.ReadFile(full)
	require.NoError(t, err)

	changed, err := handleCreateOrUpdateOperation(
		context.Background(), writer, event, manifestTarget{filePath: relPath}, full, worktree)
	require.NoError(t, err)

	after, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a no-op edit must not rewrite the file")
	assert.False(t, changed,
		"a no-op in-place edit must report no change, otherwise an empty commit is attempted")
}

// PERF REGRESSION GUARD — the locator scans each base path once per batch.
//
// Match-first must not re-scan the tree per event: a snapshot of many large
// manifests (e.g. a cluster-wide CRD watch) would be O(events × tree) and miss
// commit deadlines. We prove the cache by scanning, then writing a file, then
// scanning again: the batch sees a single snapshot of the checked-out commit, so
// the second scan must return the cached (empty) inventory, not the new file.
func TestManifestLocator_ScansOncePerBatch(t *testing.T) {
	worktree := newWorktreeForTest(t)
	locator := newManifestLocator(worktree)

	first := locator.inventoryFor("")
	require.Empty(t, first.Records, "a fresh worktree indexes no manifests")

	seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	second := locator.inventoryFor("")
	assert.Empty(t, second.Records,
		"the inventory is scanned once per batch and cached; a mid-batch write must not re-trigger a scan")
}
