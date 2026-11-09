// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// spec.serializeNamespace overrides the inference at every site that decides whether
// metadata.namespace is in the bytes. The corpus (shapes/2-flat-namespace-free and
// 4-tree-namespace-free under test/fixtures/layout-corpus) pins the FIRST write of a
// namespace-free folder; what it cannot pin is everything after it: an update, a second write
// of the same object, and the folder that already supplies the namespace being overridden the
// other way. Those are here.

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

// TestSerializeNamespace_SecondSourceNamespaceIsRefused is the fence around "one namespace", and
// the reason it refuses where the rest of the model reports: two namespaces reaching a
// namespace-free folder do not collide, they MATCH. shop/checkout-config and
// billing/checkout-config both resolve to bytes carrying no namespace, so one document ends up
// flipping between two live objects and Git holds no record that it happened.
//
// test/fixtures/layout-corpus/shapes/2-flat-namespace-free asserts the same refusal end to end, against the
// condition a user reads. This asserts the halves that fixture cannot: that the refusal is a
// precondition (nothing is written, whatever the events were), and that a wildcard is refused
// without enumerating anything.
func TestSerializeNamespace_SecondSourceNamespaceIsRefused(t *testing.T) {
	t.Run("a second namespace refuses before a byte moves", func(t *testing.T) {
		worktree := newWorktreeForTest(t)

		err := flushWithNamespacePolicy(t, worktree,
			serializeNamespacePolicy(false, "billing", "shop"),
			namespaceProbeEvent("shop", "checkout-config", "green"))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "admits exactly one source namespace")
		assert.NoFileExists(t, filepath.Join(worktree.Filesystem().Root(), "shop/configmaps/checkout-config.yaml"),
			"a refused flush writes nothing")
	})

	t.Run("a wildcard is refused without enumerating anything", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		policy := serializeNamespacePolicy(false, "shop")
		policy.SourceNamespaceWildcard = true

		err := flushWithNamespacePolicy(t, worktree, policy,
			namespaceProbeEvent("shop", "checkout-config", "green"))

		require.Error(t, err)
		assert.Contains(t, err.Error(), `sourceNamespace "*"`)
	})

	t.Run("one namespace writes", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		require.NoError(t, flushWithNamespacePolicy(t, worktree,
			serializeNamespacePolicy(false, "shop"),
			namespaceProbeEvent("shop", "checkout-config", "green")))
	})

	t.Run("inference is never fenced", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		require.NoError(t, flushWithNamespacePolicy(t, worktree,
			namespacePolicy{SourceNamespaces: []string{"billing", "shop"}},
			namespaceProbeEvent("shop", "checkout-config", "green")),
			"a folder that declared nothing resolves each document against the root governing it")
	})

	t.Run("serializeNamespace: true is not fenced either", func(t *testing.T) {
		worktree := newWorktreeForTest(t)
		require.NoError(t, flushWithNamespacePolicy(t, worktree,
			serializeNamespacePolicy(true, "billing", "shop"),
			namespaceProbeEvent("shop", "checkout-config", "green")),
			"every document carries its own namespace, so two of them are still two documents")
	})
}

// TestSerializeNamespace_RefusalPublishesItsOwnReason pins the reason the refusal reaches status
// under. The message is asserted by the corpus fixture; this is the mapping, which is what a
// consumer alerts on and what Stalled carries.
func TestSerializeNamespace_RefusalPublishesItsOwnReason(t *testing.T) {
	worktree := newWorktreeForTest(t)

	err := flushWithNamespacePolicy(t, worktree,
		serializeNamespacePolicy(false, "billing", "shop"),
		namespaceProbeEvent("shop", "checkout-config", "green"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "MultipleSourceNamespaces", manifestanalyzer.GitPathRefusalReason(refused))
}

// clearMetadataEncryptor stands in for sops under the encrypted_regex a target's .sops.yaml uses:
// metadata stays in the clear, so whatever namespace it is handed ends up in the file.
type clearMetadataEncryptor struct{}

func (clearMetadataEncryptor) Encrypt(_ context.Context, plain []byte, _ ResourceMeta) ([]byte, error) {
	return append(slices.Clone(plain), "sops:\n  version: 3.9.0\n  mac: stub\n"...), nil
}

func flushSecretWithNamespacePolicy(t *testing.T, worktree *gogit.Worktree, policy namespacePolicy, value string) {
	t.Helper()
	writer := newContentWriter(types.SensitiveResourcePolicy{})
	writer.setEncryptor(clearMetadataEncryptor{}, "test-scope")
	w := &BranchWorker{
		contentWriter: writer,
		mapper: typeset.NewSnapshotRegistry(typeset.Snapshot{Entries: []typeset.Entry{{
			GVK:        schema.GroupVersionKind{Version: "v1", Kind: "Secret"},
			GVR:        schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
			Namespaced: true,
			Allowed:    true,
		}}}),
	}
	event := Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "app", "namespace": "shop"},
			"data":       map[string]interface{}{"apiKey": value},
		}},
		Identifier: types.ResourceIdentifier{Version: "v1", Resource: "secrets", Namespace: "shop", Name: "app"},
		Operation:  "UPDATE",
	}
	_, err := w.flushEventsToWorktree(t.Context(), worktree, "", []Event{event}, nil, policy, v1alpha3.PruneOnEvent)
	require.NoError(t, err)
}

// TestSerializeNamespace_FalseUpdatesTheSecretItAlreadyWrote is the sensitive half of
// TestSerializeNamespace_FalseUpdatesTheDocumentItAlreadyWrote. An encrypted file is never patched:
// every update re-encrypts the whole object, so it has its own place to leave the namespace out.
func TestSerializeNamespace_FalseUpdatesTheSecretItAlreadyWrote(t *testing.T) {
	worktree := newWorktreeForTest(t)
	policy := serializeNamespacePolicy(false, "shop")

	flushSecretWithNamespacePolicy(t, worktree, policy, "Z3JlZW4=")
	flushSecretWithNamespacePolicy(t, worktree, policy, "Ymx1ZQ==")

	body := readWorktreeFile(t, worktree, "shop/secrets/app.sops.yaml")
	assert.Contains(t, body, "apiKey: Ymx1ZQ==", "the edit must land")
	assert.NotContains(t, body, "namespace:", "and it must still carry no namespace")
}

// TestSensitiveUpdate_NamespaceFollowsThePolicy pins that re-encrypting an existing file decides the
// namespace the way patching a plaintext one does: spec.serializeNamespace when it is set, and what
// the document's own bytes do when it is not.
func TestSensitiveUpdate_NamespaceFollowsThePolicy(t *testing.T) {
	const (
		namespaced    = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: shop\n"
		namespaceless = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n"
		encryptedBody = "data:\n  apiKey: ENC[AES256,data:OLD,iv:aa,tag:bb]\nsops:\n  version: 3.9.0\n  mac: OLD\n"
		suppliesShop  = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: shop\n" +
			"resources:\n  - app.sops.yaml\n"
		suppliesNothing = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n  - app.sops.yaml\n"
	)

	tests := []struct {
		name          string
		document      string
		kustomization string
		policy        namespacePolicy
		wantNamespace bool
	}{
		{
			name:          "unset: a kustomization supplies it, so the file keeps leaving it out",
			document:      namespaceless,
			kustomization: suppliesShop,
			wantNamespace: false,
		},
		{
			name:          "unset: a file that carries its own namespace keeps it",
			document:      namespaced,
			kustomization: suppliesNothing,
			wantNamespace: true,
		},
		{
			name:          "true writes it where the file left it out",
			document:      namespaceless,
			kustomization: suppliesShop,
			policy:        serializeNamespacePolicy(true, "shop"),
			wantNamespace: true,
		},
		{
			name:          "false removes it where the file carried it",
			document:      namespaced,
			kustomization: suppliesShop,
			policy:        serializeNamespacePolicy(false, "shop"),
			wantNamespace: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktree := newWorktreeForTest(t)
			root := worktree.Filesystem().Root()
			seedFile(t, root, "kustomization.yaml", tt.kustomization)
			seedFile(t, root, "app.sops.yaml", tt.document+encryptedBody)

			flushSecretWithNamespacePolicy(t, worktree, tt.policy, "Ymx1ZQ==")

			body := readWorktreeFile(t, worktree, "app.sops.yaml")
			assert.Contains(t, body, "apiKey: Ymx1ZQ==", "the update must re-encrypt the file it found")
			assert.Equal(t, tt.wantNamespace, strings.Contains(body, "namespace: shop"), body)
		})
	}
}
