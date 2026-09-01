// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// The two folder shapes below are the ones where the manifest store's view of a
// document's namespace and the namespace kustomize actually renders it into can
// disagree. Neither is refused when the folder is read — one is indexed under the
// namespace the file names, the other is indexed under no namespace at all — so the
// only thing standing between them and a wrong commit is the render check at the
// write path. These tests pin that backstop: without them, a change that relaxed the
// check would turn a refusal into a silent write and nothing would fail.
//
// The read-side halves live in the contextual-namespace corpus as
// unsupported/conflicting-explicit-namespace and unsupported/nested-both-namespaces
// (docs/layout/contextual-namespace.md).

// namespaceProbeMapper serves ConfigMap as a namespaced, followable type, which is what
// the namespace context resolution needs to run at all.
func namespaceProbeMapper() typeset.Lookup {
	return typeset.NewSnapshotRegistry(typeset.Snapshot{Entries: []typeset.Entry{{
		GVK:        schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		GVR:        schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		Namespaced: true,
		Allowed:    true,
	}}})
}

func namespaceProbeEvent(namespace, name, color string) Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
			"data":       map[string]interface{}{"color": color},
		}},
		Identifier: types.ResourceIdentifier{
			Group: "", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: name,
		},
		Operation: "UPDATE",
	}
}

func seedFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	return full
}

// A document carrying metadata.namespace: beta inside a folder whose kustomization
// sets namespace: alpha is indexed under beta — an explicit namespace is authoritative
// as written, and the store does not consult the transformer. kustomize disagrees: its
// namespace transformer overrides an explicit metadata.namespace, so the folder renders
// alpha/cm. The write must be refused rather than committed against a document the
// folder does not actually deploy.
func TestPlanFlush_RefusesWhenTransformerOverridesExplicitNamespace(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()

	seedFile(t, root, "kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: alpha\nresources:\n- cm.yaml\n")
	docPath := seedFile(t, root, "cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: beta\ndata:\n  color: blue\n")

	worker := &BranchWorker{contentWriter: writer, mapper: namespaceProbeMapper()}
	changed, err := worker.flushEventsToWorktree(
		t.Context(), worktree, "",
		[]Event{namespaceProbeEvent("beta", "cm", "green")}, nil, v1alpha3.PruneOnEvent)

	require.Error(t, err, "the folder renders alpha/cm while the mirror holds beta/cm; the write must refuse")
	assert.Contains(t, err.Error(), "does not render to the live object")
	assert.False(t, changed)

	got, readErr := os.ReadFile(docPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(got), "color: blue", "a refused flush leaves the document untouched")
}

// A parent root and its child root both set namespace:, so two namespaces reach one
// document and the store refuses to infer either — leaving the document namespace-less
// and therefore unmatchable by identity. Placement then treats the live object as new
// and proposes a second file, which would render two ConfigMaps into the same namespace
// under the same name. The render check is what stops it, and it must stop it before
// anything reaches the worktree, including the resources: entry the new file would need.
func TestPlanFlush_RefusesWhenNestedRootsBothSetNamespace(t *testing.T) {
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()

	rootKustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
		"namespace: outer\nresources:\n- media\n"
	seedFile(t, root, "kustomization.yaml", rootKustomization)
	seedFile(t, root, "media/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: inner\nresources:\n- cm.yaml\n")
	seedFile(t, root, "media/cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: m\ndata:\n  color: blue\n")

	worker := &BranchWorker{contentWriter: writer, mapper: namespaceProbeMapper()}
	changed, err := worker.flushEventsToWorktree(
		t.Context(), worktree, "",
		[]Event{namespaceProbeEvent("outer", "m", "green")}, nil, v1alpha3.PruneOnEvent)

	require.Error(t, err, "a second document for the same rendered object must not be committed")
	assert.False(t, changed)

	_, statErr := os.Stat(filepath.Join(root, "outer", "configmaps", "m.yaml"))
	assert.True(t, os.IsNotExist(statErr), "the duplicate file placement must not survive the refusal")

	gotRoot, readErr := os.ReadFile(filepath.Join(root, "kustomization.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, rootKustomization, string(gotRoot),
		"the resources: entry for the refused file must not survive either")
}
