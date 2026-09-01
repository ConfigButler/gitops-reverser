// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The render gate compares what a folder renders with the live object it mirrors, and
// metadata.namespace is the one field that is allowed not to match. This file is the whole
// boundary, one test per row of the table in docs/design/created-root-namespace.md:
//
//	serializeNamespace   governing root        metadata.namespace in the comparison
//	unset or true        any                   checked
//	false                sets namespace:       checked
//	false                sets none / no root   ignored, every other field still compared
//
// The relaxation is scoped by the RENDER, not by the setting alone. A folder that declares
// namespace: makes a concrete claim and has to keep it; a folder that declares none has said the
// namespace comes from the installer, and comparing it against the source namespace would be
// comparing something the folder deliberately does not express.

func namespacelessRoot() string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - web.yaml\n"
}

func rootWithNamespace(namespace string) string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
		"namespace: " + namespace + "\nresources:\n  - web.yaml\n"
}

func seedKustomizeFolder(t *testing.T, worktree *gogit.Worktree, root string) {
	t.Helper()
	dir := worktree.Filesystem().Root()
	seedFile(t, dir, "kustomization.yaml", root)
	seedFile(t, dir, "web.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n")
}

// Row 3, the created root. The operator wrote the root itself and wrote no namespace into it, so
// the document renders with none while the live object is in shop. That difference is the shape
// working as declared.
func TestNamespaceFidelity_CreatedRootSuppliesNoNamespaceAndTheWriteIsAccepted(t *testing.T) {
	worktree := newWorktreeForTest(t)

	require.NoError(t, flushWithPlacement(t, worktree, useKustomizePolicy(),
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	root := readWorktreeFile(t, worktree, "kustomization.yaml")
	assert.NotContains(t, root, "namespace:",
		"serializeNamespace: false means the artifact does not encode its deployment namespace")
	assert.NotContains(t, readWorktreeFile(t, worktree, "checkout-config.yaml"), "namespace:")
}

// Row 3 again, and the point of scoping the rule by the render rather than by who wrote the root:
// a namespace-less root the USER wrote behaves exactly like one the operator created.
func TestNamespaceFidelity_ExistingNamespacelessRootBehavesIdentically(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seedKustomizeFolder(t, worktree, namespacelessRoot())

	require.NoError(t, flushWithPlacement(t, worktree, nil,
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green")))

	assert.NotContains(t, readWorktreeFile(t, worktree, "checkout-config.yaml"), "namespace:")
	assert.Contains(t, readWorktreeFile(t, worktree, "kustomization.yaml"), "- checkout-config.yaml")
}

// Row 2, and the refinement that keeps the relaxation honest: a root declaring namespace: shop has
// a concrete render contract, so a live billing object written under it really is being relocated.
// Relaxing there would hide exactly the failure this gate exists to catch.
func TestNamespaceFidelity_RootThatDeclaresANamespaceStillRejectsAnotherNamespacesObject(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seedKustomizeFolder(t, worktree, rootWithNamespace("shop"))

	err := flushWithPlacement(t, worktree, nil,
		serializeNamespacePolicy(false, "billing"),
		namespaceProbeEvent("billing", "checkout-config", "green"))

	require.Error(t, err, "the folder renders this document into shop, and the object lives in billing")
	assert.Contains(t, err.Error(), "does not render to the live object")
}

// Row 1. Neither unset nor true relaxes anything: the namespace stays part of the comparison, so a
// transformer that would move the object is still refused.
func TestNamespaceFidelity_TrueAndUnsetStayStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy namespacePolicy
	}{
		{"unset infers per document", namespacePolicy{SourceNamespaces: []string{"shop"}}},
		{"true always writes the namespace", serializeNamespacePolicy(true, "shop")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := newWorktreeForTest(t)
			seedKustomizeFolder(t, worktree, rootWithNamespace("billing"))

			err := flushWithPlacement(t, worktree, nil, tc.policy,
				namespaceProbeEvent("shop", "checkout-config", "green"))

			require.Error(t, err, "the root's transformer renders this shop object into billing")
			assert.Contains(t, err.Error(), "does not render to the live object")
		})
	}
}

// The relaxation is namespace-shaped and nothing wider: every other field is still compared, so a
// folder that cannot express the object it mirrors is still refused under serializeNamespace: false.
func TestNamespaceFidelity_FalseStillComparesEveryOtherField(t *testing.T) {
	worktree := newWorktreeForTest(t)
	seedFile(t, worktree.Filesystem().Root(), "kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"resources:\n  - web.yaml\ncommonLabels:\n  team: platform\n")
	seedFile(t, worktree.Filesystem().Root(), "web.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web\n")

	err := flushWithPlacement(t, worktree, nil,
		serializeNamespacePolicy(false, "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green"))

	require.Error(t, err, "a label transformer the live object does not carry is still a divergence")
	assert.True(t,
		strings.Contains(err.Error(), "does not render to the live object") ||
			strings.Contains(err.Error(), "Git path refused"),
		"unexpected refusal: %v", err)
}
