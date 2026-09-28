// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The snapshot sweep removes a document exactly as a live DELETE does (removeDocument), and an
// object the overlay inherits is removed by an operator-owned $patch: delete that survives a
// restart in Git and is retired when the object comes back.

const (
	inheritedOverlay = "overlays/test"
	inheritedPatch   = "overlays/test/configmap-shared-delete.yaml"
	inheritedBaseCM  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n  namespace: podinfo-test\n" +
		"data:\n  k: v\n"
)

func seedInheritedOverlay(t *testing.T) *gogit.Worktree {
	t.Helper()
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "base/kustomization.yaml", "resources:\n  - cm.yaml\n")
	seedPlacedManifest(t, worktree, "base/cm.yaml", inheritedBaseCM)
	seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml",
		"namespace: podinfo-test\nresources:\n  - ../../base\n")
	return worktree
}

func sharedConfigMap() manifestanalyzer.DesiredResource {
	return manifestanalyzer.DesiredResource{
		Resource: types.NewResourceIdentifier("", "v1", "configmaps", "podinfo-test", "shared"),
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "shared", "namespace": "podinfo-test"},
			"data":     map[string]interface{}{"k": "v"},
		}},
	}
}

func resyncOverlay(
	t *testing.T,
	worktree *gogit.Worktree,
	mode v1alpha3.PruneMode,
	desired ...manifestanalyzer.DesiredResource,
) (ResyncStats, bool, error) {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	scope := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "podinfo-test")
	return w.applyResyncToWorktree(context.Background(), worktree, inheritedOverlay,
		ResolvedTargetMetadata{PruneMode: mode}, desired, &scope)
}

func readRepoFile(t *testing.T, worktree *gogit.Worktree, rel string) (string, bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(worktree.Filesystem().Root(), rel))
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	return string(content), true
}

func sharedID() manifestedit.Identity {
	return manifestedit.Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "podinfo-test", Name: "shared"}
}

func TestResync_AlwaysRemovesAnInheritedObjectWithAnOwnedPatch(t *testing.T) {
	worktree := seedInheritedOverlay(t)

	stats, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, stats.Deleted)

	patch, ok := readRepoFile(t, worktree, inheritedPatch)
	require.True(t, ok, "the overlay must gain the owned delete patch")
	assert.Equal(t, string(inheritedDeletePatchDocument(sharedID())), patch)
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.Contains(t, kust, "configmap-shared-delete.yaml")
	base, _ := readRepoFile(t, worktree, "base/cm.yaml")
	assert.Equal(t, inheritedBaseCM, base, "the read-only base is untouched")

	// Idempotent: the object is already out of the render, so the next snapshot neither removes it
	// again nor counts it as retained.
	stats, changed, err = resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Zero(t, stats.Deleted)
	assert.Zero(t, stats.Retained)
}

// Re-entry after a restart: all the state is in Git, so a fresh worker retires the owned patch and
// its patches: entry, the oracle verifies the object renders again, and a second pass is a no-op.
func TestResync_ReEntryRetiresTheOwnedPatch(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	_, _, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)

	stats, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, sharedConfigMap())
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, stats.Created)
	_, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	assert.False(t, patchPresent, "the owned patch is retired")
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.NotContains(t, kust, "configmap-shared-delete.yaml")
	assert.NotContains(t, kust, "patches", "an emptied patches: list goes with its last entry")
	base, _ := readRepoFile(t, worktree, "base/cm.yaml")
	assert.Equal(t, inheritedBaseCM, base)

	_, changed, err = resyncOverlay(t, worktree, v1alpha3.PruneAlways, sharedConfigMap())
	require.NoError(t, err)
	assert.False(t, changed, "re-entry is idempotent")
}

// A patch that was the operator's before the ownership marker existed is still the operator's.
func TestResync_ReEntryRetiresAPatchWrittenBeforeTheMarker(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	seedPlacedManifest(t, worktree, inheritedPatch, string(unmarkedInheritedDeletePatch(sharedID())))
	seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml",
		"namespace: podinfo-test\nresources:\n  - ../../base\npatches:\n  - path: configmap-shared-delete.yaml\n")

	_, _, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, sharedConfigMap())
	require.NoError(t, err)
	_, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	assert.False(t, patchPresent)
}

// An edited patch or an unrelated file at the patch path is not the operator's: removal and
// re-entry both refuse the batch, and the file is left byte for byte.
func TestResync_UnownedPatchPathRefusesWithoutLoss(t *testing.T) {
	edited := string(inheritedDeletePatchDocument(sharedID())) + "# kept on purpose by a human\n"
	cases := map[string]struct {
		patch   string
		listed  bool
		desired []manifestanalyzer.DesiredResource
	}{
		"re-entry over an edited patch": {patch: edited, listed: true, desired: []manifestanalyzer.DesiredResource{
			sharedConfigMap(),
		}},
		"removal over an unrelated file": {
			patch: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keepme\ndata:\n  x: y\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			worktree := seedInheritedOverlay(t)
			seedPlacedManifest(t, worktree, inheritedPatch, tc.patch)
			resources := "  - ../../base\n  - configmap-shared-delete.yaml\n"
			patches := ""
			if tc.listed {
				resources = "  - ../../base\n"
				patches = "patches:\n  - path: configmap-shared-delete.yaml\n"
			}
			seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml",
				"namespace: podinfo-test\nresources:\n"+resources+patches)

			_, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, tc.desired...)
			var refused *manifestanalyzer.AcceptanceRefusedError
			require.ErrorAs(t, err, &refused)
			assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueUnownedDeletePatch))
			assert.False(t, changed)
			got, _ := readRepoFile(t, worktree, inheritedPatch)
			assert.Equal(t, tc.patch, got)
		})
	}
}

// Retaining modes keep the inherited object, and count it: it is absent from the selected mirror.
func TestResync_OnEventRetainsAnInheritedObject(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	stats, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneOnEvent)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, stats.Retained)
}

// An in-jail kustomize root whose documents take their namespace from the kustomization: the sweep
// locates each by its raw identity, keeps multi-document siblings, and when the last document
// leaves a file removes its resources: entry so the root still builds.
func TestResync_AlwaysRemovesNamespacelessDocumentsAndCleansResources(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(
		t,
		worktree,
		"app/kustomization.yaml",
		"namespace: app\nresources:\n  - cms.yaml\n  - keep.yaml\n",
	)
	seedPlacedManifest(t, worktree, "app/cms.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n---\n"+
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\ndata:\n  k: v\n")
	seedPlacedManifest(
		t,
		worktree,
		"app/keep.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keep\ndata:\n  k: v\n",
	)
	cm := func(name string) manifestanalyzer.DesiredResource {
		return manifestanalyzer.DesiredResource{
			Resource: types.NewResourceIdentifier("", "v1", "configmaps", "app", name),
			Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": name, "namespace": "app"},
				"data":     map[string]interface{}{"k": "v"},
			}},
		}
	}
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	scope := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "app")
	resync := func(desired ...manifestanalyzer.DesiredResource) ResyncStats {
		t.Helper()
		stats, _, err := w.applyResyncToWorktree(context.Background(), worktree, "app",
			ResolvedTargetMetadata{PruneMode: v1alpha3.PruneAlways}, desired, &scope)
		require.NoError(t, err)
		return stats
	}

	assert.Equal(t, 1, resync(cm("a"), cm("keep")).Deleted)
	cms, _ := readRepoFile(t, worktree, "app/cms.yaml")
	assert.Contains(t, cms, "name: a")
	assert.NotContains(t, cms, "name: b")

	assert.Equal(t, 1, resync(cm("keep")).Deleted)
	_, present := readRepoFile(t, worktree, "app/cms.yaml")
	assert.False(t, present)
	kust, _ := readRepoFile(t, worktree, "app/kustomization.yaml")
	assert.NotContains(t, kust, "cms.yaml")
	assert.Contains(t, kust, "keep.yaml")

	assert.Equal(t, 1, resync().Deleted, "a complete empty snapshot removes the rest")
}
