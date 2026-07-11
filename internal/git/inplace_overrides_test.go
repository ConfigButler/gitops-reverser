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

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The F1 edit-through scenarios (docs/design/gitops-api/finished/f1-images-replicas-edit-through.md):
// a live change produced by a kustomization's images:/replicas: entry lands on
// the entry, and the source manifest keeps its bytes.

const overridesDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/example/podinfo:6.3.0 # base tag, shadowed by the overlay
`

const overridesKustomizationYAML = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - apps/deployment.yaml
images:
  - name: ghcr.io/example/podinfo
    newTag: "6.4.0"
replicas:
  - name: web
    count: 3
`

func overridesDeploymentEvent(image string, replicas int64) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "web", "namespace": "default"},
			"spec": map[string]interface{}{
				"replicas": replicas,
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "web"}},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "podinfo", "image": image},
						},
					},
				},
			},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "web",
		},
		Operation: "UPDATE",
	}
}

func deploymentMapper() typeset.Lookup {
	return typeset.NewSnapshotRegistry(typeset.Snapshot{
		Entries: []typeset.Entry{{
			GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespaced: true,
			Allowed:    true,
		}},
	})
}

// seedOverridesWorktree writes the deployment + kustomization pair and returns
// the two absolute paths (deployment first).
func seedOverridesWorktree(t *testing.T, root string) (string, string) {
	t.Helper()
	deployPath := filepath.Join(root, "apps", "deployment.yaml")
	kustPath := filepath.Join(root, "kustomization.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(deployPath), 0o750))
	require.NoError(t, os.WriteFile(deployPath, []byte(overridesDeploymentYAML), 0o600))
	require.NoError(t, os.WriteFile(kustPath, []byte(overridesKustomizationYAML), 0o600))
	return deployPath, kustPath
}

// assertFileBytes pins a file to exact bytes: these scenarios are ABOUT
// formatting preservation, so a semantic (YAML-equal) comparison would hide
// exactly the churn they exist to catch.
func assertFileBytes(t *testing.T, path, want, msg string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	if string(got) != want {
		t.Errorf("%s\n--- want ---\n%s\n--- got ---\n%s", msg, want, got)
	}
}

// A live tag bump governed by the images entry updates kustomization.yaml and
// leaves the source manifest byte-for-byte untouched.
func TestPlanFlush_RoutesImageTagToKustomizationEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.5.0", 3))
	require.True(t, changed, "the tag bump must land somewhere")

	assertFileBytes(t, deployPath, overridesDeploymentYAML,
		"the source manifest must keep its bytes; the entry owns the tag")

	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), `newTag: "6.5.0"`, "the entry absorbs the live tag")
	assert.NotContains(t, string(kust), `newTag: "6.4.0"`)
}

// Live state equal to the overlay's render is a full no-op: the source file's
// "stale" tag is dead text the entry shadows, and must NOT be overwritten
// (the write-through fix F1 exists for).
func TestPlanFlush_LiveMatchingOverlayRenderIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.4.0", 3))
	assert.False(t, changed, "live == render must write nothing")

	assertFileBytes(t, deployPath, overridesDeploymentYAML, "source manifest must be untouched")
	assertFileBytes(t, kustPath, overridesKustomizationYAML, "kustomization must be untouched")
}

// A live scale governed by the replicas entry updates count and leaves the
// source manifest untouched — including keeping spec.replicas ABSENT, since the
// transformer supplies the field.
func TestPlanFlush_RoutesReplicaCountToKustomizationEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.4.0", 5))
	require.True(t, changed, "the scale must land somewhere")

	assertFileBytes(t, deployPath, overridesDeploymentYAML,
		"spec.replicas must stay out of the source manifest")

	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), "count: 5", "the entry absorbs the live count")
	assert.NotContains(t, string(kust), "count: 3")
}

// A /scale field patch on a governed Deployment routes to the replicas entry;
// spec.replicas never lands in the source manifest.
func TestPlanFlush_RoutesScaleFieldPatchToKustomizationEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	scale := Event{
		Identifier: types.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "web",
		},
		Operation: "UPDATE",
		FieldPatch: &FieldPatch{
			Source: "scale",
			Assignments: []manifestedit.FieldAssignment{
				{Path: []string{"spec", "replicas"}, Value: int64(7)},
			},
		},
	}
	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(), scale)
	require.True(t, changed, "the scale must land in the kustomization")

	assertFileBytes(t, deployPath, overridesDeploymentYAML,
		"a governed scale must not write spec.replicas into the source manifest")

	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), "count: 7")
}

// An override edit that cannot land (the entry lacks the field, or the
// kustomization is not in the subtree) is dropped without touching any buffer —
// the source file was already left in source form, so the next event re-decides.
func TestApplyOverrideEdits_SkipLeavesBuffersUntouched(t *testing.T) {
	kust := "images:\n  - name: app\n    newName: mirror/app\n"
	scan := manifestanalyzer.FolderScan{YAMLFiles: []manifestedit.FileContent{
		{Path: "kustomization.yaml", Content: []byte(kust)},
	}}
	wb := newWriteBatch(context.Background(), newContentWriter(types.SensitiveResourcePolicy{}), nil, scan, nil)

	fieldMissing := manifestanalyzer.OverrideEdit{
		KustomizationPath: "kustomization.yaml",
		Edit: manifestedit.KustomizationEdit{
			Section: manifestedit.KustomizationSectionImages, EntryIndex: 0,
			EntryName: "app", Field: "newTag", Value: "2.0.0",
		},
	}
	fileMissing := manifestanalyzer.OverrideEdit{
		KustomizationPath: "gone/kustomization.yaml",
		Edit:              fieldMissing.Edit,
	}
	changed := wb.applyOverrideEdits(context.Background(), Event{}, []manifestanalyzer.OverrideEdit{
		fieldMissing, fileMissing,
	})
	assert.False(t, changed, "no edit can land, so no buffer may change")
	assert.Equal(t, kust, string(wb.buffer("kustomization.yaml").current),
		"a refused edit must leave the kustomization bytes untouched")
}

// A resync over a governed folder whose live state equals the overlay render is
// a complete no-op: no churn on the source manifest OR the kustomization. This
// pins the mark-and-sweep path (M8) explicitly, not just via shared code.
func TestResync_GovernedFolderInSyncIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	stats, changed := applyResyncViaWorktree(t, writer, deploymentMapper(), worktree,
		desiredOverridesDeployment("ghcr.io/example/podinfo:6.4.0", 3))
	assert.False(t, changed, "live == render must resync to zero changes")
	assert.Zero(t, stats.Created+stats.Updated+stats.Deleted)
	assertFileBytes(t, deployPath, overridesDeploymentYAML, "resync must not churn the source manifest")
	assertFileBytes(t, kustPath, overridesKustomizationYAML, "resync must not churn the kustomization")
}

// A resync that finds governed drift routes it to the kustomization entry, just
// like the steady-state path.
func TestResync_GovernedDriftRoutesToKustomizationEntry(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	deployPath, kustPath := seedOverridesWorktree(t, worktree.Filesystem.Root())

	stats, changed := applyResyncViaWorktree(t, writer, deploymentMapper(), worktree,
		desiredOverridesDeployment("ghcr.io/example/podinfo:6.5.0", 3))
	require.True(t, changed)
	assert.Equal(t, 1, stats.Updated, "the routed edit counts as one update")
	assertFileBytes(t, deployPath, overridesDeploymentYAML, "the source manifest keeps its bytes")

	kust, err := os.ReadFile(kustPath)
	require.NoError(t, err)
	assert.Contains(t, string(kust), `newTag: "6.5.0"`)
}

// desiredOverridesDeployment adapts the live Deployment fixture into a resync
// snapshot entry.
func desiredOverridesDeployment(image string, replicas int64) manifestanalyzer.DesiredResource {
	event := overridesDeploymentEvent(image, replicas)
	return manifestanalyzer.DesiredResource{Resource: event.Identifier, Object: event.Object}
}

// A change no entry governs keeps today's behavior: it is patched into the
// source manifest and the kustomization stays untouched.
func TestPlanFlush_UngovernedChangeStillPatchesSourceFile(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	deployPath, kustPath := seedOverridesWorktree(t, root)
	// Repoint the images entry at an image this Deployment does not use.
	ungoverned := "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		"namespace: default\n" +
		"resources:\n  - apps/deployment.yaml\n" +
		"images:\n  - name: someone/else\n    newTag: \"1.0.0\"\n"
	require.NoError(t, os.WriteFile(kustPath, []byte(ungoverned), 0o600))

	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, deploymentMapper(),
		overridesDeploymentEvent("ghcr.io/example/podinfo:6.5.0", 1))
	require.True(t, changed)

	deploy, err := os.ReadFile(deployPath)
	require.NoError(t, err)
	assert.Contains(t, string(deploy), "ghcr.io/example/podinfo:6.5.0",
		"an ungoverned tag change writes through to the source manifest")
	assert.Contains(t, string(deploy), "# base tag, shadowed by the overlay",
		"the in-place patch keeps hand-authored comments")

	assertFileBytes(t, kustPath, ungoverned, "the kustomization stays untouched")
}
