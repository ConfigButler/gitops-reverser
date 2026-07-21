// SPDX-License-Identifier: Apache-2.0

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

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// inplaceCMEvent builds an UPDATE event for the default/app ConfigMap with its
// single data key set to color.
func inplaceCMEvent(color string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "app", "namespace": "default"},
			"data":       map[string]interface{}{"color": color},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: "default", Name: "app",
		},
		Operation: "UPDATE",
	}
}

// newWorktreeForTest gives a real git worktree rooted at a fresh temp dir.
func newWorktreeForTest(t *testing.T) *gogit.Worktree {
	t.Helper()
	repo, err := gogit.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	return worktree
}

// applyEventsViaPlanFlush drives the M7 plan-then-flush write path for tests: it
// builds a minimal structure-only worker (no mapper) and folds the events over the
// worktree at base "" (events carry their own relative paths under the root). It is
// the unit-level entry point that replaced the per-event applyEventToWorktree.
func applyEventsViaPlanFlush(t *testing.T, writer *contentWriter, worktree *gogit.Worktree, events ...Event) bool {
	t.Helper()
	w := &BranchWorker{contentWriter: writer}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", events, nil, v1alpha3.PruneOnEvent)
	require.NoError(t, err)
	return changed
}

func applyEventsViaPlanFlushWithMapper(
	t *testing.T,
	writer *contentWriter,
	worktree *gogit.Worktree,
	mapper typeset.Lookup,
	events ...Event,
) bool {
	t.Helper()
	w := &BranchWorker{contentWriter: writer, mapper: mapper}
	changed, err := w.flushEventsToWorktree(context.Background(), worktree, "", events, nil, v1alpha3.PruneOnEvent)
	require.NoError(t, err)
	return changed
}

// When the file on disk is hand-authored (carries a comment), an update edits it
// in place: the comment survives and only the changed field is rewritten. This is
// the file-agnostic-placement "magic" landing in the live writer's plan-then-flush
// path: the document is matched by content identity and patched, not overwritten.
func TestPlanFlush_PreservesHandAuthoredFormatting(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("green")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	seeded := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: app\n  namespace: default\n" +
		"data:\n  # keep this operator note across edits\n  color: blue\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(seeded), 0o600))

	changed := applyEventsViaPlanFlush(t, writer, worktree, event)
	require.True(t, changed, "a real value change must be written")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "# keep this operator note across edits",
		"the hand-authored comment must survive the in-place edit")
	assert.Contains(t, string(got), "color: green", "the changed value is applied")
	assert.NotContains(t, string(got), "color: blue")
}

func TestPlanFlush_PreservesKustomizeNamespaceStyle(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	relPath := "apps/bundle.yaml"
	full := filepath.Join(root, relPath)
	seeded := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: app\n" +
		"data:\n  # keep this operator note across edits\n  color: blue\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(seeded), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: default\nresources:\n- apps/bundle.yaml\n",
	), 0o600))

	mapper := typeset.NewSnapshotRegistry(typeset.Snapshot{
		Entries: []typeset.Entry{{
			GVK:        schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			GVR:        schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			Namespaced: true,
			Allowed:    true,
		}},
	})
	changed := applyEventsViaPlanFlushWithMapper(t, writer, worktree, mapper, inplaceCMEvent("green"))
	require.True(t, changed, "a real value change must be written")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, "# keep this operator note across edits")
	assert.Contains(t, body, "color: green")
	assert.NotContains(t, body, "color: blue")
	assert.NotContains(t, body, "namespace:", "namespace should stay in kustomization.yaml, not the resource")
}

// A change against a canonical file applies the new value. The plan-then-flush path
// deliberately patches in place (preserving any layout) rather than the old writer's
// wholesale re-render, so the assertion is on the resulting value, not on byte
// identity with a wholesale render.
func TestPlanFlush_AppliesChangeToCanonicalFile(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("green")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	// Seed with the canonical rendering of a *different* value, so the update is a
	// real change against an operator-canonical file.
	seedCanonical, err := writer.buildContentForWrite(context.Background(), inplaceCMEvent("blue"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, seedCanonical, 0o600))

	changed := applyEventsViaPlanFlush(t, writer, worktree, event)
	require.True(t, changed)

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Contains(t, string(got), "color: green", "the changed value is applied")
	assert.NotContains(t, string(got), "color: blue")
}

// Re-applying the identical desired state is a no-op: the byte state machine and the
// manifestedit no-op decision agree, so nothing is written and the flush reports no
// change (avoiding an empty commit).
func TestPlanFlush_IdenticalUpdateIsNoOp(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem.Root()

	event := inplaceCMEvent("blue")
	relPath := writer.filePathForIdentifier(event.Identifier)
	full := filepath.Join(root, relPath)

	seed, err := writer.buildContentForWrite(context.Background(), event)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, seed, 0o600))

	changed := applyEventsViaPlanFlush(t, writer, worktree, event)
	assert.False(t, changed, "an identical update must report no change")

	got, err := os.ReadFile(full)
	require.NoError(t, err)
	assert.Equal(t, string(seed), string(got), "a no-op must not rewrite the file")
}
