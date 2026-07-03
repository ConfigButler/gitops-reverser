// SPDX-License-Identifier: Apache-2.0

package git

// These tests guard writer behaviors that the M7 plan-then-flush path must keep:
//
//   - File-agnostic placement: the writer matches a resource by content identity and
//     edits or deletes it where it actually lives, only falling back to the canonical
//     identity path for a genuinely new resource. A manifest a user placed at
//     apps/foo.yaml is updated and deleted in place, never duplicated at the canonical
//     path.
//
//   - No empty commits: an update that resolves to no byte change reports no change,
//     so the commit executor does not attempt an empty commit.
//
//   - No data loss: a wholesale write must never drop sibling documents in a
//     multi-document file.

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

// Flexible placement, update path: a user placed the ConfigMap at a hand-chosen path
// (apps/foo.yaml), not the canonical identity path. A cluster update must edit the
// resource where it already lives and must NOT spawn a duplicate at the canonical path.
func TestPlanFlush_UpdateFollowsExistingPlacement(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	placedFull := seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	event := inplaceCMEvent("green")
	applyEventsViaPlanFlush(t, writer, worktree, event)

	placedAfter, err := os.ReadFile(placedFull)
	require.NoError(t, err)
	assert.Contains(t, string(placedAfter), "color: green",
		"the update must land in the existing manifest at apps/foo.yaml")

	canonicalFull := filepath.Join(worktree.Filesystem.Root(), writer.filePathForIdentifier(event.Identifier))
	_, statErr := os.Stat(canonicalFull)
	assert.Truef(t, os.IsNotExist(statErr),
		"no duplicate copy must be created at the canonical path %s", canonicalFull)
}

// Flexible placement, delete path: deleting the resource from the cluster must remove
// the manifest where it actually lives (apps/foo.yaml). The delete event here carries
// the object, so the writer content-matches it to its real location.
func TestPlanFlush_DeleteFollowsExistingPlacement(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)

	placedFull := seedPlacedManifest(t, worktree, placedManifestPath, placedManifestBlue)

	event := inplaceCMEvent("blue")
	event.Operation = "DELETE"
	changed := applyEventsViaPlanFlush(t, writer, worktree, event)

	assert.True(t, changed, "deleting the resource must remove the manifest where it lives")
	_, statErr := os.Stat(placedFull)
	assert.True(t, os.IsNotExist(statErr), "apps/foo.yaml must be deleted, not orphaned")
}

// An in-place no-op must report no change. Multi-doc file: an unrelated resource is
// the first document, and the SECOND document is the exact canonical rendering of the
// watched object — so editing it in place is a genuine no-op. manifestedit returns the
// file unchanged, and the flush must report changed=false rather than drive an empty
// commit.
func TestPlanFlush_NoOpInMultiDocReportsNoChange(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("blue")
	full := filepath.Join(root, placedManifestPath)

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

	changed := applyEventsViaPlanFlush(t, writer, worktree, event)

	after, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a no-op edit must not rewrite the file")
	assert.False(t, changed,
		"a no-op in-place edit must report no change, otherwise an empty commit is attempted")
}

// Data-loss guard: a wholesale write must never drop sibling documents. The canonical
// path holds a multi-document file whose target document is non-editable (it uses a
// YAML merge key), so it does not claim its identity and is not matched for an in-place
// patch. The writer must refuse to overwrite the multi-document file wholesale (which
// would drop the unrelated first document) and report no change.
func TestPlanFlush_MultiDocCanonicalDoesNotDropSiblings(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("green")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	// Document 0: an unrelated, editable resource that must survive.
	other := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: other\n  namespace: default\n" +
		"data:\n  k: v\n"
	// Document 1: the target (default/app) written with a merge key, which
	// manifestedit refuses to edit — so it does not claim its identity for the in-place
	// match, and the canonical-path file is multi-document.
	targetUneditable := "apiVersion: v1\nkind: ConfigMap\n" +
		"metadata:\n  name: app\n  namespace: default\n" +
		"data: &d\n  color: blue\nextra:\n  <<: *d\n"
	seeded := other + "---\n" + targetUneditable
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(seeded), 0o600))

	changed := applyEventsViaPlanFlush(t, writer, worktree, event)

	after, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.False(t, changed, "an unsafe multi-document write must report no change")
	assert.Equal(t, seeded, string(after),
		"the sibling document must survive: the file must not be overwritten wholesale")
}
