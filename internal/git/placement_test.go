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
		ByType: map[string]string{"v1/configmaps": "{namespace}/configmaps.yaml"},
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

// A new resource whose siblings are in a kustomize-namespace-inferred bundle
// must not write metadata.namespace into that bundle — otherwise an incidental
// resource sharing the namespace (e.g. a cluster-injected ConfigMap watched by
// too broad a WatchRule) would break the "no namespace: in this file"
// convention every other document in the bundle already follows.
func TestPlacement_BundleAppend_OmitsNamespaceInKustomizeContext(t *testing.T) {
	worktree := newWorktreeForTest(t)
	kustYAML := "namespace: app\nresources:\n  - all.yaml\n"
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml", kustYAML)
	seeded := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\ndata:\n  k: v\n" +
		"---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\ndata:\n  k: v\n"
	full := seedPlacedManifest(t, worktree, "overlays/test/all.yaml", seeded)

	changed := applyEventsWithPolicy(t, worktree, nil, newConfigMapEvent("cache", "app"))
	require.True(t, changed)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: a", "the first existing document must survive")
	assert.Contains(t, string(got), "name: b", "the second existing document must survive")
	assert.Contains(t, string(got), "name: cache", "the new document must be appended")
	assert.NotContains(t, string(got), "namespace:",
		"the new document must not break the bundle's namespace-omitted convention")
}

// A misconfigured declared template (missing {name}) makes two distinct secrets
// collide on the same rendered path. createNew must skip the write rather than
// crash or corrupt the existing file — the next event or resync retries once the
// policy is fixed.
func TestPlacement_SensitiveCollision_SkipsWithoutCrashing(t *testing.T) {
	worktree := newWorktreeForTest(t)
	existing := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: other\n  namespace: app\nsops:\n  version: \"3\"\n"
	full := seedPlacedManifest(t, worktree, "secrets/app.sops.yaml", existing)
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/secrets": "secrets/{namespace}.sops.yaml"},
	}

	event := Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "api-token", "namespace": "app"},
		}},
		Identifier: types.NewResourceIdentifier("", "v1", "secrets", "app", "api-token"),
		Operation:  "CREATE",
	}
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{})}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", []Event{event}, policy)

	require.NoError(t, err, "a placement conflict must be skipped, not returned as a batch error")
	assert.False(t, changed, "no file should be written when placement cannot be resolved safely")

	got, readErr := os.ReadFile(full)
	require.NoError(t, readErr)
	assert.Equal(t, existing, string(got), "the existing secret must survive byte-for-byte")
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

// A kustomization whose resources: field is malformed (not a sequence) is still
// accepted by the analyzer (only specific disallowed keys, not resources: shape,
// disqualify a kustomization) — so the writer must still place the resource's own
// file, but the resources: entry add is skipped rather than corrupting the
// kustomization, leaving it exactly as it was for a human to fix.
func TestPlacement_KustomizeEntryAppendSkipped_MalformedResourcesField(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()
	kustYAML := "namespace: app\nresources: not-a-list\n"
	seedPlacedManifest(t, worktree, "overlays/test/kustomization.yaml", kustYAML)

	changed := applyEventsWithPolicy(t, worktree, nil, newConfigMapEvent("cache", "app"))
	require.True(t, changed, "the new resource's own file must still be written")

	_, err := os.Stat(filepath.Join(root, "overlays/test/cache.yaml"))
	require.NoError(t, err, "the resource itself is written even though the kustomize entry could not be added")

	kust, err := os.ReadFile(filepath.Join(root, "overlays/test/kustomization.yaml"))
	require.NoError(t, err)
	// Byte-exact, not YAMLEq: the guarantee under test is that a skipped edit
	// returns the original content untouched, not merely a semantically
	// equivalent one.
	assert.Equal(t, kustYAML, string(kust), //nolint:testifylint
		"a malformed resources: field must be left untouched, not corrupted")
}

func newTestWriteBatch(t *testing.T) *writeBatch {
	t.Helper()
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	return newWriteBatch(context.Background(), writer, nil, manifestanalyzer.FolderScan{}, nil)
}

func TestAppendYAMLDocument(t *testing.T) {
	if got := appendYAMLDocument(nil, []byte("a: 1\n")); string(got) != "a: 1\n" {
		t.Errorf("empty existing should return newDoc verbatim, got %q", got)
	}
	if got := appendYAMLDocument([]byte("a: 1"), []byte("b: 2\n")); string(got) != "a: 1\n---\nb: 2\n" {
		t.Errorf("got %q, want a newline inserted before the separator when existing lacks a trailing one", got)
	}
	if got := appendYAMLDocument([]byte("a: 1\n"), []byte("b: 2\n")); string(got) != "a: 1\n---\nb: 2\n" {
		t.Errorf("got %q, want no extra blank line when existing already ends in a newline", got)
	}
}

func TestAppendNewDocument_BuildContentErrorIsPropagated(t *testing.T) {
	wb := newTestWriteBatch(t)
	event := Event{
		Object:     nil,
		Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "app", "x"),
	}

	_, err := wb.appendNewDocument(context.Background(), event, "all.yaml")

	require.Error(t, err, "a nil object cannot be marshaled, and that error must not be swallowed")
}

// If the kustomization file was removed by an earlier delete in the same batch
// (an edge case that should not happen in practice, since kustomization.yaml is
// a retained build directive, but the writer must never panic on it),
// appendKustomizationResource must no-op rather than dereference a nil buffer.
func TestAppendKustomizationResource_VanishedBuffer_NoPanic(t *testing.T) {
	wb := newTestWriteBatch(t)
	kustPath := "overlays/test/kustomization.yaml"
	wb.buffers[kustPath] = &fileBuffer{rel: kustPath, current: nil}
	placement := manifestanalyzer.PlacementResult{
		Path:          "overlays/test/new.yaml",
		Kustomization: &manifestanalyzer.KustomizationInfo{Path: kustPath, Resources: []string{"deployment.yaml"}},
	}
	event := Event{Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "app", "new")}

	assert.NotPanics(t, func() {
		wb.appendKustomizationResource(context.Background(), event, placement)
	})
	assert.Nil(t, wb.buffers[kustPath].current, "a vanished buffer must not be resurrected")
}

// Two new resources that both render to the same brand-new declared path (a
// collision LocateNew cannot see coming, since it only consults the pre-batch
// store) must form one deterministic multi-document file — never silently
// overwrite one another — regardless of which event the writer processes
// first. This is the design doc's "if several new plaintext resources in one
// plan render to the same path, write a multi-document file in deterministic
// resource-identity order".
func TestPlacement_ColdBundleCollision_BothSurviveRegardlessOfOrder(t *testing.T) {
	policy := &manifestanalyzer.PlacementPolicy{Default: "all.yaml"}
	first := newConfigMapEvent("alpha", "app")
	second := newConfigMapEvent("beta", "app")

	forward := newWorktreeForTest(t)
	changed := applyEventsWithPolicy(t, forward, policy, first, second)
	require.True(t, changed)
	forwardBody, err := os.ReadFile(filepath.Join(forward.Filesystem.Root(), "all.yaml"))
	require.NoError(t, err)

	reversed := newWorktreeForTest(t)
	changed = applyEventsWithPolicy(t, reversed, policy, second, first)
	require.True(t, changed)
	reversedBody, err := os.ReadFile(filepath.Join(reversed.Filesystem.Root(), "all.yaml"))
	require.NoError(t, err)

	assert.Contains(t, string(forwardBody), "name: alpha", "the first resource must survive")
	assert.Contains(t, string(forwardBody), "name: beta", "the second resource must survive")
	assert.Equal(t, 2, strings.Count(string(forwardBody), "kind: ConfigMap"), "both must land as separate documents")
	assert.Equal(t, string(forwardBody), string(reversedBody),
		"the resulting file must not depend on event processing order")
}

// A third collision on the same batch-cold path must also survive and stay
// sorted, proving the fix generalizes beyond exactly two resources.
func TestPlacement_ColdBundleCollision_ThreeResourcesAllSurvive(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{Default: "all.yaml"}

	changed := applyEventsWithPolicy(t, worktree, policy,
		newConfigMapEvent("charlie", "app"),
		newConfigMapEvent("alpha", "app"),
		newConfigMapEvent("bravo", "app"),
	)
	require.True(t, changed)

	got, err := os.ReadFile(filepath.Join(worktree.Filesystem.Root(), "all.yaml"))
	require.NoError(t, err)
	body := string(got)
	assert.Equal(t, 3, strings.Count(body, "kind: ConfigMap"))
	// Sorted by resource identity ("…/app/alpha" < "…/app/bravo" < "…/app/charlie"),
	// independent of the arrival order above.
	assert.Less(t, strings.Index(body, "name: alpha"), strings.Index(body, "name: bravo"))
	assert.Less(t, strings.Index(body, "name: bravo"), strings.Index(body, "name: charlie"))
}

// A sensitive resource must never be merged into a shared file, even when the
// collision is with another new resource within the same batch (not a
// pre-existing file, which the analyzer-level test already covers).
func TestPlacement_ColdBundleCollision_SensitiveNeverMerged(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/secrets": "secrets/{namespace}.sops.yaml"},
	}
	newSecretEvent := func(name string) Event {
		return Event{
			Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata":   map[string]interface{}{"name": name, "namespace": "app"},
			}},
			Identifier: types.NewResourceIdentifier("", "v1", "secrets", "app", name),
			Operation:  "CREATE",
		}
	}
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	writer.setEncryptor(&stubEncryptor{result: []byte(
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: first\n  namespace: app\n" +
			"data:\n  k: ENC[AES256,data:x,iv:y,tag:z]\nsops:\n  version: 3.9.0\n",
	)}, "test-scope")
	w := &BranchWorker{contentWriter: writer}

	changed, err := w.flushEventsToWorktree(
		context.Background(), worktree, "", []Event{newSecretEvent("first"), newSecretEvent("second")}, policy,
	)

	require.NoError(t, err)
	assert.True(t, changed, "the first secret must still be written")

	got, readErr := os.ReadFile(filepath.Join(worktree.Filesystem.Root(), "secrets/app.sops.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(got), "kind: Secret"),
		"the second secret must be skipped, never merged into the first's file")
}

// A sensitive and a plaintext resource that a bundling default routes to the same
// brand-new file must never co-mingle in it, whichever event the writer processes
// first. This is the same-batch half of Option B2's write-safety guard: the single
// declared map is consulted for both classes, so a Secret and a ConfigMap can now
// resolve to one path, and the guard keeps encrypted and cleartext documents out of
// the same file (the first arrival wins; the other is skipped and retried).
func TestPlacement_ColdBundleCollision_SensitiveAndPlaintextNeverMix(t *testing.T) {
	policy := &manifestanalyzer.PlacementPolicy{Default: "all.yaml"}
	secretEvent := Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "cred", "namespace": "app"},
		}},
		Identifier: types.NewResourceIdentifier("", "v1", "secrets", "app", "cred"),
		Operation:  "CREATE",
	}
	configMapEvent := newConfigMapEvent("cache", "app")

	newWriter := func() *contentWriter {
		writer := newContentWriter(types.SensitiveResourcePolicy{})
		writer.setEncryptor(&stubEncryptor{result: []byte(
			"apiVersion: v1\nkind: Secret\nmetadata:\n  name: cred\n  namespace: app\n" +
				"data:\n  k: ENC[AES256,data:x,iv:y,tag:z]\nsops:\n  version: 3.9.0\n",
		)}, "test-scope")
		return writer
	}

	// Secret first: the Secret wins the file, the ConfigMap is skipped.
	secretFirst := newWorktreeForTest(t)
	wsf := &BranchWorker{contentWriter: newWriter()}
	_, err := wsf.flushEventsToWorktree(
		context.Background(), secretFirst, "", []Event{secretEvent, configMapEvent}, policy,
	)
	require.NoError(t, err)
	secretFirstBody, readErr := os.ReadFile(filepath.Join(secretFirst.Filesystem.Root(), "all.yaml"))
	require.NoError(t, readErr)
	assert.Contains(t, string(secretFirstBody), "kind: Secret")
	assert.NotContains(t, string(secretFirstBody), "kind: ConfigMap",
		"a plaintext resource must never join a file that already holds an encrypted document")

	// ConfigMap first: the ConfigMap wins the file, the Secret is skipped.
	configMapFirst := newWorktreeForTest(t)
	wcf := &BranchWorker{contentWriter: newWriter()}
	_, err = wcf.flushEventsToWorktree(
		context.Background(), configMapFirst, "", []Event{configMapEvent, secretEvent}, policy,
	)
	require.NoError(t, err)
	configMapFirstBody, readErr := os.ReadFile(filepath.Join(configMapFirst.Filesystem.Root(), "all.yaml"))
	require.NoError(t, readErr)
	assert.Contains(t, string(configMapFirstBody), "kind: ConfigMap")
	assert.NotContains(t, string(configMapFirstBody), "kind: Secret",
		"a sensitive resource must never share a file with a plaintext resource")
}

// Resync (M8) folds its whole desired snapshot through the same createNew path
// as steady-state events, so the same brand-new-path collision can occur there
// too — proving the fix covers both entry points the design doc calls out.
func TestPlacement_ColdBundleCollision_ViaResync(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{Default: "all.yaml"}
	desired := []manifestanalyzer.DesiredResource{
		{
			Resource: types.NewResourceIdentifier("", "v1", "configmaps", "app", "alpha"),
			Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": "alpha", "namespace": "app"},
			}},
		},
		{
			Resource: types.NewResourceIdentifier("", "v1", "configmaps", "app", "beta"),
			Object: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{"name": "beta", "namespace": "app"},
			}},
		},
	}

	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: configMapMapper()}
	_, changed, err := w.applyResyncToWorktree(context.Background(), worktree, "", desired, nil, policy)

	require.NoError(t, err)
	assert.True(t, changed)

	got, readErr := os.ReadFile(filepath.Join(worktree.Filesystem.Root(), "all.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, 2, strings.Count(string(got), "kind: ConfigMap"), "both resync creates must survive")
}
