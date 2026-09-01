// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// spec.placement.useKustomize is the only thing in this operator that writes a file nobody asked
// for by name, so what it does NOT do matters as much as what it does. The corpus
// (docs/layout/shapes/5-kustomize-single-folder) pins the bytes of the one commit it produces;
// these pin its boundaries.

func useKustomizePolicy() *manifestanalyzer.PlacementPolicy {
	return &manifestanalyzer.PlacementPolicy{UseKustomize: true}
}

func flushWithPlacement(
	t *testing.T,
	worktree *gogit.Worktree,
	policy *manifestanalyzer.PlacementPolicy,
	namespaces namespacePolicy,
	events ...Event,
) error {
	t.Helper()
	w := &BranchWorker{contentWriter: newContentWriter(types.SensitiveResourcePolicy{}), mapper: namespaceProbeMapper()}
	_, err := w.flushEventsToWorktree(t.Context(), worktree, "", events, policy, namespaces, v1alpha3.PruneOnEvent)
	return err
}

func TestUseKustomize_CreatesTheRootAndRegistersTheDocument(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	root := readWorktreeFile(t, worktree, "kustomization.yaml")
	assert.Equal(t, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
		"namespace: shop\nresources:\n  - checkout-config.yaml\n", root)
	assert.NotContains(t, readWorktreeFile(t, worktree, "checkout-config.yaml"), "namespace:",
		"the operator owns the root that supplies the namespace, which is what makes the omission provable")
}

// The second document in the same flush joins the root the first one created. The store was built
// before the batch, so nothing in it knows that file exists.
func TestUseKustomize_SecondDocumentJoinsTheRootTheFirstCreated(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green"),
		namespaceProbeEvent("shop", "checkout-flags", "blue")))

	root := readWorktreeFile(t, worktree, "kustomization.yaml")
	assert.Contains(t, root, "- checkout-config.yaml")
	assert.Contains(t, root, "- checkout-flags.yaml")
	assert.Equal(t, 1, strings.Count(root, "kind: Kustomization"), "one root, not one per document")
}

// The flag's ONE job is the empty case. A folder that already has a root is registered into
// either way, which is #319's invariant and not this setting.
func TestUseKustomize_LeavesAnExistingRootAlone(t *testing.T) {
	worktree := newWorktreeForTest(t)
	existing := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
		"namespace: shop\nresources:\n  - web.yaml\n"
	seedFile(t, worktree.Filesystem().Root(), "kustomization.yaml", existing)
	seedFile(t, worktree.Filesystem().Root(), "web.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n")

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	root := readWorktreeFile(t, worktree, "kustomization.yaml")
	assert.Contains(t, root, "- web.yaml", "the root the user wrote keeps its own entries")
	assert.Contains(t, root, "- checkout-config.yaml")
	assert.Equal(t, 1, strings.Count(root, "kind: Kustomization"))
}

// Without the flag nothing is created, and the document lands at the canonical path. That is the
// default and it is the whole difference between adopting a kustomize folder and creating one.
func TestUseKustomize_UnsetWritesNoRoot(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithPlacement(t, worktree, nil, namespacePolicy{},
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.NoFileExists(t, worktree.Filesystem().Root()+"/kustomization.yaml")
	assert.FileExists(t, worktree.Filesystem().Root()+"/shop/configmaps/checkout-config.yaml")
}

// The created root carries namespace: only when the folder has ONE. With two source namespaces
// there is nothing truthful to write there — and such a target is refused before it reaches here
// when it declared serializeNamespace: false, which is the pairing the model relies on.
func TestUseKustomize_CreatedRootCarriesNamespaceOnlyWhenTheFolderHasOne(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		namespacePolicy{SourceNamespaces: []string{"billing", "shop"}},
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.NotContains(t, readWorktreeFile(t, worktree, "kustomization.yaml"), "namespace:",
		"a root that named one of two namespaces would mislabel every document under it")
}

// A declared template still decides the path; useKustomize only decides whether the folder gets a
// root. The created root lists the document wherever the template put it, which is what makes a
// subdirectory template renderable at all.
func TestUseKustomize_RegistersADeclaredSubdirectoryPath(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := &manifestanalyzer.PlacementPolicy{Default: "configmaps/{name}.yaml", UseKustomize: true}

	require.NoError(t, flushWithPlacement(t, worktree, policy,
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.Contains(t, readWorktreeFile(t, worktree, "kustomization.yaml"),
		"- configmaps/checkout-config.yaml")
	assert.FileExists(t, worktree.Filesystem().Root()+"/configmaps/checkout-config.yaml")
}
