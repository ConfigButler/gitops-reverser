// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// Deleting a managed document is only half a delete when the file is named in a
// kustomization's resources:. A resources: entry pointing at a file that no longer exists is
// one kustomize refuses to build over —
//
//	accumulating resources ... '/scan/api.yaml' doesn't exist
//
// — so the repository is left in a state no GitOps controller can deploy.
//
// The entry must come out. But ONLY when the file itself is actually gone: a file holding
// several documents survives the deletion of one of them, and pulling its resources: entry
// would then un-deploy every OTHER resource in it. That is the sharp edge here, and it is the
// first test below.

const deleteKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - web.yaml
  - bundle.yaml
`

// bundle.yaml holds TWO documents. Deleting one of them must leave the file — and therefore
// its resources: entry — in place, or the surviving document silently stops being deployed.
const deleteBundleYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: keep-me
data:
  a: "1"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: delete-me
data:
  b: "2"
`

// seedDeleteWorktree stages the files in git, not merely on disk: removing a file the index
// has never seen fails with "entry not found", so a delete test that only wrote to the
// filesystem would be testing the fixture rather than the writer.
func seedDeleteWorktree(t *testing.T, worktree *gogit.Worktree, kustomization string) {
	t.Helper()
	seedPlacedManifest(t, worktree, "web.yaml", sharedImageDeploymentYAML("web"))
	seedPlacedManifest(t, worktree, "bundle.yaml", deleteBundleYAML)
	seedPlacedManifest(t, worktree, "kustomization.yaml", kustomization)
}

func deleteConfigMapEvent(name string) Event {
	return Event{
		Identifier: types.ResourceIdentifier{
			Version: "v1", Resource: "configmaps", Namespace: "default", Name: name,
		},
		Operation: "DELETE",
	}
}

// mergedMapper resolves both kinds the delete-plus-governed-write flush needs.
func mergedMapper() typeset.Lookup {
	return typeset.NewSnapshotRegistry(typeset.Snapshot{
		Entries: []typeset.Entry{
			{
				GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
				Namespaced: true,
				Allowed:    true,
			},
			{
				GVK:        schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				GVR:        schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
				Namespaced: true,
				Allowed:    true,
			},
		},
	})
}

// THE ONE THAT MATTERS. bundle.yaml holds two ConfigMaps; one is deleted. The file still
// holds the other, so the file stays — and its resources: entry MUST stay with it. Removing
// the entry here would un-deploy keep-me, a resource nobody asked to touch.
func TestPlanFlush_DeletingOneDocumentOfAFileKeepsTheFileAndItsResourcesEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedDeleteWorktree(t, worktree, deleteKustomizationYAML)

	changed, err := flushEventsForTest(t, writer, worktree, configMapMapper(),
		deleteConfigMapEvent("delete-me"))

	require.NoError(t, err)
	require.True(t, changed, "the document must have been removed")

	bundle, readErr := os.ReadFile(filepath.Join(root, "bundle.yaml"))
	require.NoError(t, readErr, "the file must survive: it still holds another document")
	assert.Contains(t, string(bundle), "name: keep-me", "the surviving document must be untouched")
	assert.NotContains(t, string(bundle), "name: delete-me")

	assertFileBytes(t, filepath.Join(root, "kustomization.yaml"), deleteKustomizationYAML,
		"the file still exists, so its resources: entry must NOT be removed — "+
			"pulling it would un-deploy keep-me")
}

// The file's LAST document is deleted, so the file goes. Its resources: entry now names a
// file that does not exist, which kustomize refuses to build over — so the entry goes too,
// and the repository stays deployable.
func TestPlanFlush_DeletingTheLastDocumentAlsoRemovesTheResourcesEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedDeleteWorktree(t, worktree, deleteKustomizationYAML)

	changed, err := flushEventsForTest(t, writer, worktree, configMapMapper(),
		deleteConfigMapEvent("keep-me"),
		deleteConfigMapEvent("delete-me"),
	)

	require.NoError(t, err)
	require.True(t, changed)

	_, statErr := os.Stat(filepath.Join(root, "bundle.yaml"))
	assert.True(t, os.IsNotExist(statErr), "the file held nothing else, so it is gone")

	kust, readErr := os.ReadFile(filepath.Join(root, "kustomization.yaml"))
	require.NoError(t, readErr)
	assert.NotContains(t, string(kust), "bundle.yaml",
		"a resources: entry naming a file that no longer exists makes the folder unbuildable")
	assert.Contains(t, string(kust), "web.yaml", "and every other entry is left exactly as it was")
}

// The delete lands in the same flush as a governed write. Both must survive: without the
// entry removal the re-render cannot build the tree at all, and the oracle would refuse the
// whole flush — losing the delete AND the unrelated image bump.
func TestPlanFlush_DeleteAndGovernedWriteInOneFlush(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedDeleteWorktree(t, worktree, deleteKustomizationYAML+`images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`)

	changed, err := flushEventsForTest(t, writer, worktree, mergedMapper(),
		sharedImageEvent("web", "ghcr.io/example/shared:2.0.0"),
		deleteConfigMapEvent("keep-me"),
		deleteConfigMapEvent("delete-me"),
	)

	require.NoError(t, err, "the tree must still build with the delete applied, or the oracle refuses everything")
	require.True(t, changed)

	kust, readErr := os.ReadFile(filepath.Join(root, "kustomization.yaml"))
	require.NoError(t, readErr)
	assert.Contains(t, string(kust), `newTag: "2.0.0"`, "the governed write must land")
	assert.NotContains(t, string(kust), "bundle.yaml", "and the dangling entry must be gone")
}
