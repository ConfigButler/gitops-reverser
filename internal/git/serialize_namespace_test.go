// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// spec.serializeNamespace overrides the inference at every site that decides whether
// metadata.namespace is in the bytes. The corpus (docs/layout/shapes/2-flat-namespace-free and
// 4-tree-namespace-free) pins the FIRST write of a namespace-free folder; what it cannot pin is
// everything after it — an update, a second write of the same object, and the folder that already
// supplies the namespace being overridden the other way. Those are here.

func serializeNamespacePolicy(serialize bool, sources ...string) namespacePolicy {
	return namespacePolicy{Serialize: &serialize, SourceNamespaces: sources}
}

func flushWithNamespacePolicy(
	t *testing.T,
	worktree *gogit.Worktree,
	policy namespacePolicy,
	events ...Event,
) error {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: namespaceProbeMapper()}
	_, err := w.flushEventsToWorktree(t.Context(), worktree, "", events, nil, policy, v1alpha3.PruneOnEvent)
	return err
}

func readWorktreeFile(t *testing.T, worktree *gogit.Worktree, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(worktree.Filesystem().Root(), rel))
	require.NoError(t, err)
	return string(body)
}

// TestSerializeNamespace_FalseOmitsItWhereInferenceWouldWriteIt is the flat namespace-free folder:
// nothing in it supplies a namespace, so inference writes one, and the declaration is what stops it.
func TestSerializeNamespace_FalseOmitsItWhereInferenceWouldWriteIt(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithNamespacePolicy(t, worktree,
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	body := readWorktreeFile(t, worktree, "shop/configmaps/checkout-config.yaml")
	assert.NotContains(t, body, "namespace:", "an explicit serializeNamespace: false writes no namespace")
	assert.Contains(t, body, "name: checkout-config")
}

// TestSerializeNamespace_TrueWritesItWhereInferenceWouldOmitIt is the override in the other
// direction: the folder's kustomization supplies exactly this namespace, so inference would leave
// metadata.namespace out, and true puts it back.
func TestSerializeNamespace_TrueWritesItWhereInferenceWouldOmitIt(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedFile(t, root, "kustomization.yaml", strings.Join([]string{
		"apiVersion: kustomize.config.k8s.io/v1beta1",
		"kind: Kustomization",
		"namespace: shop",
		"resources: []",
		"",
	}, "\n"))

	require.NoError(t, flushWithNamespacePolicy(t, worktree,
		serializeNamespacePolicy(true, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.Contains(t, readWorktreeFile(t, worktree, "checkout-config.yaml"), "namespace: shop",
		"an explicit serializeNamespace: true writes the namespace even where the folder supplies it")
}

// TestSerializeNamespace_UnsetKeepsInference is the default, and the whole reason the field is a
// *bool: an object that says nothing behaves exactly as it did before the field existed.
func TestSerializeNamespace_UnsetKeepsInference(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithNamespacePolicy(t, worktree, namespacePolicy{},
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.Contains(t, readWorktreeFile(t, worktree, "shop/configmaps/checkout-config.yaml"),
		"namespace: shop", "unset infers, and nothing in this folder supplies a namespace")
}

// TestSerializeNamespace_FalseUpdatesTheDocumentItAlreadyWrote is the case no first-write fixture
// can show, and the one that makes the setting shippable at all.
//
// A namespace-free document that no kustomization governs belongs, as far as the folder is
// concerned, to no namespace. Read back that way it does not match the live object it mirrors, so
// the second write of the SAME object would find nothing, place it as new, and append a second
// copy of it into the file holding the first. The declared-namespace attribution
// (manifestanalyzer.WithDeclaredNamespace) is what closes that loop.
func TestSerializeNamespace_FalseUpdatesTheDocumentItAlreadyWrote(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := serializeNamespacePolicy(false, "shop")

	require.NoError(t, flushWithNamespacePolicy(t, worktree, policy,
		namespaceProbeEvent("shop", "checkout-config", "green")))
	require.NoError(t, flushWithNamespacePolicy(t, worktree, policy,
		namespaceProbeEvent("shop", "checkout-config", "blue")))

	body := readWorktreeFile(t, worktree, "shop/configmaps/checkout-config.yaml")
	assert.Equal(t, 1, strings.Count(body, "name: checkout-config"),
		"the second write must edit the document the first one wrote, not append a second copy of it")
	assert.Contains(t, body, "color: blue", "the edit must land")
	assert.NotContains(t, body, "namespace:", "and it must still carry no namespace")
}

// TestSerializeNamespace_FalseAttributesNothingWhenTwoNamespacesReachTheTarget is the boundary of
// that attribution: with two source namespaces there is no single namespace a namespace-free
// document could belong to, so nothing is attributed. The write is refused instead — see
// TestSerializeNamespace_SecondSourceNamespaceIsRefused — and this test pins that the READ side
// does not quietly pick one in the meantime.
func TestSerializeNamespace_FalseAttributesNothingWhenTwoNamespacesReachTheTarget(t *testing.T) {
	policy := serializeNamespacePolicy(false, "billing", "shop")
	assert.Empty(t, policy.declaredNamespace(), "two namespaces have no single answer")

	wildcard := serializeNamespacePolicy(false, "shop")
	wildcard.SourceNamespaceWildcard = true
	assert.Empty(t, wildcard.declaredNamespace(), "a wildcard cannot be proven to be one namespace")

	assert.Equal(t, "shop", serializeNamespacePolicy(false, "shop").declaredNamespace())
	assert.Empty(t, serializeNamespacePolicy(true, "shop").declaredNamespace(),
		"a folder whose documents carry their own namespace needs no attribution")
	assert.Empty(t, namespacePolicy{SourceNamespaces: []string{"shop"}}.declaredNamespace(),
		"inference is never a folder-wide claim, so unset attributes nothing")
}
