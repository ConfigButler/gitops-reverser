// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// A patch without the ownership marker proves nothing about who wrote it: a human's patch that
// happens to match the template byte for byte is refused, never retired, and is left untouched.
func TestResync_ReEntryRefusesAnUnmarkedPatchThatMatchesTheTemplate(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	unmarked := strings.TrimPrefix(string(inheritedDeletePatchDocument(sharedID())), inheritedDeletePatchMarker)
	seedPlacedManifest(t, worktree, inheritedPatch, unmarked)
	overlay := "namespace: podinfo-test\nresources:\n  - ../../base\npatches:\n  - path: configmap-shared-delete.yaml\n"
	seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml", overlay)

	_, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, sharedConfigMap())
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueUnownedDeletePatch))
	assert.False(t, changed)
	patch, _ := readRepoFile(t, worktree, inheritedPatch)
	assert.Equal(t, unmarked, patch)
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.Equal(t, overlay, kust)
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

// Re-entry into an overlay that supplies labels: the object comes back carrying the overlay's
// label, which the base does not hold. Planned against the store built while the patch hid the
// object, the upsert tried to write that label into the read-only base and was refused. Planned
// against the staged render it sees the label is the build's, and the base stays untouched.
func TestResync_ReEntryIsPlannedAgainstTheRestoredRender(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	seedPlacedManifest(t, worktree, inheritedPatch, string(inheritedDeletePatchDocument(sharedID())))
	seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml",
		"namespace: podinfo-test\ncommonLabels:\n  env: test\nresources:\n  - ../../base\n"+
			"patches:\n  - path: configmap-shared-delete.yaml\n")
	back := sharedConfigMap()
	back.Object.SetLabels(map[string]string{"env": "test"})

	stats, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, back)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, stats.Created)
	_, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	assert.False(t, patchPresent)
	base, _ := readRepoFile(t, worktree, "base/cm.yaml")
	assert.Equal(t, inheritedBaseCM, base, "the overlay's label is the build's, never written into the base")
}

// An object a HUMAN's $patch: delete already hides is outside the render too. It is not the
// operator's to remove again (under Always that authored a second patch and failed the render),
// and it is not a retained document (under OnEvent it was counted as one).
func TestResync_AnObjectAHumanPatchHidesIsOutsideTheSweep(t *testing.T) {
	for _, mode := range []v1alpha3.PruneMode{v1alpha3.PruneAlways, v1alpha3.PruneOnEvent} {
		t.Run(string(mode), func(t *testing.T) {
			worktree := seedInheritedOverlay(t)
			humanPatch := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n  namespace: podinfo-test\n" +
				"$patch: delete\n"
			seedPlacedManifest(t, worktree, inheritedOverlay+"/remove-shared.yaml", humanPatch)
			seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml",
				"namespace: podinfo-test\nresources:\n  - ../../base\npatches:\n  - path: remove-shared.yaml\n")

			stats, changed, err := resyncOverlay(t, worktree, mode)
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Zero(t, stats.Deleted)
			assert.Zero(t, stats.Retained)
			patch, _ := readRepoFile(t, worktree, inheritedOverlay+"/remove-shared.yaml")
			assert.Equal(t, humanPatch, patch)
		})
	}
}

// Re-entry of an object a patch the operator does not own hides: the operator's own patch, renamed
// and re-listed, is no longer at the path ownership is judged by. The base document already matches
// the live object, so an unguarded upsert changed nothing and reported success while the overlay
// still built without the object. It is refused instead, and both files are left as they are.
func TestResync_ReEntryRefusesAnObjectAPatchItDoesNotOwnHides(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	_, _, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)

	owned, _ := readRepoFile(t, worktree, inheritedPatch)
	require.NoError(t, os.Remove(filepath.Join(worktree.Filesystem().Root(), inheritedPatch)))
	seedPlacedManifest(t, worktree, inheritedOverlay+"/renamed-delete.yaml", owned)
	overlay := "namespace: podinfo-test\nresources:\n  - ../../base\npatches:\n  - path: renamed-delete.yaml\n"
	seedPlacedManifest(t, worktree, inheritedOverlay+"/kustomization.yaml", overlay)

	_, changed, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways, sharedConfigMap())
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueUnownedDeletePatch))
	assert.Equal(t, "base/cm.yaml", refused.Issues[0].Path)
	assert.False(t, changed)
	renamed, _ := readRepoFile(t, worktree, inheritedOverlay+"/renamed-delete.yaml")
	assert.Equal(t, owned, renamed)
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.Equal(t, overlay, kust)
}

// The same holds inside the write jail: a document its own kustomization hides with a
// $patch: delete is not the render, so editing it cannot bring the object back.
func TestResync_ReEntryRefusesAnInJailDocumentAPatchHides(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seedPlacedManifest(t, worktree, "app/kustomization.yaml",
		"namespace: app\nresources:\n  - cm.yaml\npatches:\n  - path: remove.yaml\n")
	seedPlacedManifest(t, worktree, "app/cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n")
	seedPlacedManifest(t, worktree, "app/remove.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n$patch: delete\n")
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	scope := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "app")
	live := manifestanalyzer.DesiredResource{
		Resource: types.NewResourceIdentifier("", "v1", "configmaps", "app", "a"),
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "a", "namespace": "app"},
			"data":     map[string]interface{}{"k": "v"},
		}},
	}

	_, changed, err := w.applyResyncToWorktree(context.Background(), worktree, "app",
		ResolvedTargetMetadata{PruneMode: v1alpha3.PruneAlways}, []manifestanalyzer.DesiredResource{live}, &scope)
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueUnownedDeletePatch))
	assert.False(t, changed)
}

// flushOverlayEvents applies one live batch of events to the overlay.
func flushOverlayEvents(t *testing.T, worktree *gogit.Worktree, events ...Event) (bool, error) {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	return w.flushEventsToWorktree(context.Background(), worktree, inheritedOverlay, events, nil,
		namespacePolicy{}, v1alpha3.PruneOnEvent)
}

func sharedEvent(op types.OperationType, data string) Event {
	cm := sharedConfigMap()
	if op == types.OperationDelete {
		return Event{Identifier: cm.Resource, Operation: string(op)}
	}
	cm.Object.Object["data"] = map[string]interface{}{"k": data}
	return Event{Identifier: cm.Resource, Operation: string(op), Object: cm.Object}
}

// Two events for one returning object in a single batch (a relabel back into the selection, then
// a status write, is the ordinary shape): the first retires the owned patch, and
// the second must see the object the batch just brought back, not the pre-batch render that still
// has it hidden. It read the pre-batch render, found no patch, and refused the whole batch as
// hidden by a patch the operator does not own.
func TestFlush_ReEntryThenUpdateInOneBatch(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	_, _, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)

	changed, err := flushOverlayEvents(t, worktree,
		sharedEvent(types.OperationCreate, "v"),
		sharedEvent(types.OperationUpdate, "v"))
	require.NoError(t, err)
	assert.True(t, changed)
	_, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	assert.False(t, patchPresent, "the owned patch is retired")
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.NotContains(t, kust, "configmap-shared-delete.yaml")
	base, _ := readRepoFile(t, worktree, "base/cm.yaml")
	assert.Equal(t, inheritedBaseCM, base, "the read-only base is untouched")
}

// Back and gone again in one batch: the DELETE must see the object the batch just brought back.
// It read the pre-batch render, took the object as already hidden, and did nothing, so the commit
// left the overlay rendering an object the cluster no longer holds.
func TestFlush_ReEntryThenDeleteInOneBatch(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	_, _, err := resyncOverlay(t, worktree, v1alpha3.PruneAlways)
	require.NoError(t, err)
	patchBefore, _ := readRepoFile(t, worktree, inheritedPatch)
	kustBefore, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")

	_, err = flushOverlayEvents(t, worktree,
		sharedEvent(types.OperationCreate, "v"),
		sharedEvent(types.OperationDelete, ""))
	require.NoError(t, err)
	patch, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	require.True(t, patchPresent, "the object is gone again, so its owned patch stays")
	assert.Equal(t, patchBefore, patch)
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.Equal(t, kustBefore, kust)
}

// Gone and back again in one batch, from a rendered start: the patch the DELETE authored is the
// batch's own, so the returning object retires it and the overlay is left as it was.
func TestFlush_DeleteThenReEntryInOneBatch(t *testing.T) {
	worktree := seedInheritedOverlay(t)
	kustBefore, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")

	_, err := flushOverlayEvents(t, worktree,
		sharedEvent(types.OperationDelete, ""),
		sharedEvent(types.OperationCreate, "v"))
	require.NoError(t, err)
	_, patchPresent := readRepoFile(t, worktree, inheritedPatch)
	assert.False(t, patchPresent)
	kust, _ := readRepoFile(t, worktree, inheritedOverlay+"/kustomization.yaml")
	assert.Equal(t, kustBefore, kust)
}
