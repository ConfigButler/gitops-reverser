// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// newConfigMapEvent builds a CREATE event for a new ConfigMap with no existing
// document anywhere in the fixture — the only case F4 placement runs for.
func newConfigMapEvent(name, namespace string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
			"data":       map[string]interface{}{"color": "blue"},
		}},
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", namespace, name),
		Operation:  "CREATE",
	}
}

func applyEventsWithPolicy(
	t *testing.T,
	worktree *gogit.Worktree,
	policy *manifestanalyzer.PlacementPolicy,
	events ...Event,
) bool {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", events, policy)
	require.NoError(t, err)
	return changed
}

func TestPlacement_DeclaredPolicy_NewFile(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	policy := &manifestanalyzer.PlacementPolicy{
		Normal: manifestanalyzer.PlacementPolicyClass{
			ByType: map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
		},
	}

	changed := applyEventsWithPolicy(t, worktree, policy, newConfigMapEvent("cache", "app"))
	require.True(t, changed)

	got, err := os.ReadFile(filepath.Join(root, "app", "configmaps.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: cache")
	assert.Contains(t, string(got), "color: blue")
}

func TestPlacement_SiblingInference_BesideExistingFile(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	seedPlacedManifest(t, worktree, "overlays/test/configmap-existing.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: existing\n  namespace: podinfo-test\ndata:\n  k: v\n")

	changed := applyEventsWithPolicy(t, worktree, nil, newConfigMapEvent("cache", "podinfo-test"))
	require.True(t, changed)

	got, err := os.ReadFile(filepath.Join(root, "overlays/test/cache.yaml"))
	require.NoError(t, err, "the new file should land beside its sibling, not at the canonical path")
	assert.Contains(t, string(got), "name: cache")
}

func TestPlacement_BundleAppend_ExistingMultiDocFile(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seeded := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: app\ndata:\n  k: v\n" +
		"---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n  namespace: app\ndata:\n  k: v\n"
	full := seedPlacedManifest(t, worktree, "all.yaml", seeded)

	changed := applyEventsWithPolicy(t, worktree, nil, newConfigMapEvent("cache", "app"))
	require.True(t, changed)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: a", "the first existing document must survive")
	assert.Contains(t, string(got), "name: b", "the second existing document must survive")
	assert.Contains(t, string(got), "name: cache", "the new document must be appended")
	assert.Equal(t, 3, strings.Count(string(got), "kind: ConfigMap"),
		"exactly one document must be added, not a replace")
}

func TestPlacement_KustomizeEntryAppended_SameCommit(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	kustYAML := "# overlay for test\n" +
		"namespace: podinfo-test\n" +
		"resources:\n" +
		"  - deployment.yaml\n"
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml", kustYAML)
	seedPlacedManifest(t, worktree, "overlays/test/deployment.yaml",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: podinfo-test\n")

	changed := applyEventsWithPolicy(t, worktree, nil, newConfigMapEvent("debug-toolbox", "podinfo-test"))
	require.True(t, changed)

	newFile, err := os.ReadFile(filepath.Join(root, "overlays/test/debug-toolbox.yaml"))
	require.NoError(t, err, "the new resource should land inside the overlay directory")
	assert.Contains(t, string(newFile), "name: debug-toolbox")

	kust, err := os.ReadFile(filepath.Join(root, "overlays/test/kustomization.yaml"))
	require.NoError(t, err)
	kustStr := string(kust)
	assert.Contains(t, kustStr, "# overlay for test", "hand-authored comments must survive")
	assert.Contains(t, kustStr, "- deployment.yaml", "the existing entry must survive")
	assert.Contains(t, kustStr, "- debug-toolbox.yaml", "the new file must be added to resources:")
}

func TestPlacement_KustomizeEntryIdempotent_OnRepeatedApply(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	kustYAML := "namespace: podinfo-test\nresources:\n  - deployment.yaml\n"
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml", kustYAML)
	seedPlacedManifest(t, worktree, "overlays/test/deployment.yaml",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: podinfo-test\n")

	event := newConfigMapEvent("debug-toolbox", "podinfo-test")
	require.True(t, applyEventsWithPolicy(t, worktree, nil, event))
	// A second apply of the same create (e.g. a resync re-observing the same live
	// object) must not duplicate the resources: entry.
	applyEventsWithPolicy(t, worktree, nil, event)

	kust, err := os.ReadFile(filepath.Join(root, "overlays/test/kustomization.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(kust), "debug-toolbox.yaml"),
		"the resources: entry must appear exactly once")
}
