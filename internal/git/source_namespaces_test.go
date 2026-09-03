// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func sourceNamespaceRule(name, targetName, sourceNamespace string) *configv1alpha3.WatchRule {
	return &configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: configv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: targetName},
			Rules: []configv1alpha3.ResourceRule{{
				Resources:       []string{"configmaps"},
				SourceNamespace: sourceNamespace,
			}},
		},
	}
}

func sourceNamespaceClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// TestResolveSourceNamespaces is the set the one-source-namespace rule is decided on. Every case
// here is a claim docs/layout/model.md makes about it, and the whole set is answerable from the
// config cluster — no scan, no repository state.
func TestResolveSourceNamespaces(t *testing.T) {
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-artifact", Namespace: "shop"},
	}

	t.Run("the target's own namespace is always in the set", func(t *testing.T) {
		c := sourceNamespaceClient(t)
		namespaces, wildcard, err := resolveSourceNamespaces(t.Context(), c, target)
		require.NoError(t, err)
		assert.Equal(t, []string{"shop"}, namespaces)
		assert.False(t, wildcard)
	})

	t.Run("a rule that names no sourceNamespace watches its own", func(t *testing.T) {
		c := sourceNamespaceClient(t, sourceNamespaceRule("content", "checkout-artifact", ""))
		namespaces, _, err := resolveSourceNamespaces(t.Context(), c, target)
		require.NoError(t, err)
		assert.Equal(t, []string{"shop"}, namespaces)
	})

	t.Run("an explicit sourceNamespace is a second namespace", func(t *testing.T) {
		c := sourceNamespaceClient(t,
			sourceNamespaceRule("content", "checkout-artifact", ""),
			sourceNamespaceRule("content-billing", "checkout-artifact", "billing"))
		namespaces, _, err := resolveSourceNamespaces(t.Context(), c, target)
		require.NoError(t, err)
		assert.Equal(t, []string{"billing", "shop"}, namespaces, "sorted, so the message a user reads is stable")
	})

	t.Run("a rule pointing at another target contributes nothing", func(t *testing.T) {
		c := sourceNamespaceClient(t, sourceNamespaceRule("elsewhere", "other-artifact", "billing"))
		namespaces, _, err := resolveSourceNamespaces(t.Context(), c, target)
		require.NoError(t, err)
		assert.Equal(t, []string{"shop"}, namespaces)
	})

	t.Run("a wildcard is reported, never enumerated", func(t *testing.T) {
		c := sourceNamespaceClient(t,
			sourceNamespaceRule("everything", "checkout-artifact", configv1alpha3.SourceNamespaceWildcard))
		namespaces, wildcard, err := resolveSourceNamespaces(t.Context(), c, target)
		require.NoError(t, err)
		assert.True(t, wildcard, "a wildcard cannot be proven to be one namespace from the spec")
		assert.Equal(t, []string{"shop"}, namespaces, "and it adds no name of its own")
	})
}
