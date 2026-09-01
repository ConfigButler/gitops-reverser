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

// The finding this test exists for: enabling kustomize on a folder that ALREADY holds manifests
// and then writing a root that lists only the new document would unrender every other file. They
// stay in Git, they look mirrored, and the first `kustomize build` drops them from the output.
//
// So the created root adopts the folder. Nothing is rewritten, moved or re-encoded: the existing
// files are named in resources: exactly where they already are.
func TestUseKustomize_CreatedRootAdoptsTheFilesAlreadyInTheFolder(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedFile(t, root, "web.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n  namespace: shop\n")
	seedFile(t, root, "configmaps/cache.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cache\n  namespace: shop\n")

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.Equal(t, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
		"namespace: shop\nresources:\n"+
		"  - checkout-config.yaml\n  - configmaps/cache.yaml\n  - web.yaml\n",
		readWorktreeFile(t, worktree, "kustomization.yaml"),
		"every managed document in the folder is listed, at the path it already lives at")
	assert.Contains(t, readWorktreeFile(t, worktree, "web.yaml"), "namespace: shop",
		"adopting a file names it in resources:; it does not rewrite its bytes")
}

// A folder that already has a render root never gains a SECOND one, even when a declared template
// puts the new document outside it. Two render roots is an Ambiguous folder, and an ambiguous
// folder stops accepting new documents altogether — a far larger fault than the one unregistered
// file this leaves behind.
func TestUseKustomize_WritesNoSecondRootBesideAnExistingOne(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedFile(t, root, "media/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: shop\nresources:\n  - web.yaml\n")
	seedFile(t, root, "media/web.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n")
	policy := &manifestanalyzer.PlacementPolicy{Default: "flags/{name}.yaml", UseKustomize: true}

	require.NoError(t, flushWithPlacement(t, worktree, policy, namespacePolicy{},
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.NoFileExists(t, root+"/kustomization.yaml",
		"a second render root would make the folder ambiguous and stop every later placement")
	assert.FileExists(t, root+"/flags/checkout-config.yaml", "the document is still written")
	assert.Contains(t, readWorktreeFile(t, worktree, "media/kustomization.yaml"), "- web.yaml",
		"and the root that was already there is untouched")
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
