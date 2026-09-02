// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// The gate has to consider the GitTarget's OWN stored fields AND those of the GitProvider it writes
// through. The provider half is the one that is easy to get wrong: a GitProvider reporting Stalled
// does not stop a GitTarget wiring a worker and declaring, so without this the provider's stored
// commit settings would be ignored and the folder would keep committing at the default cadence and
// wording — the silent reinterpretation the whole retained-and-refused pattern exists to prevent.
func TestGitTargetSupersededFieldRefusal(t *testing.T) {
	cleanTarget := func() *configbutleraiv1alpha3.GitTarget {
		return &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "p"},
				Branch:      "main",
				Path:        "clusters/prod",
			},
		}
	}
	cleanProvider := func() *configbutleraiv1alpha3.GitProvider {
		return &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Spec: configbutleraiv1alpha3.GitProviderSpec{
				URL:             "git@example.com:o/r.git",
				AllowedBranches: []string{"main"},
			},
		}
	}

	t.Run("clean target and clean provider", func(t *testing.T) {
		r := &GitTargetReconciler{Client: fake.NewClientBuilder().
			WithScheme(scScheme(t)).WithObjects(cleanProvider()).Build()}

		assert.Empty(t, r.supersededFieldRefusal(context.Background(), cleanTarget(), "ns"))
	})

	t.Run("the target's own stored field", func(t *testing.T) {
		target := cleanTarget()
		//nolint:staticcheck // setting the removed field is the point.
		target.Spec.AllowedSourceNamespaces = &configbutleraiv1alpha3.NamespaceMatcher{
			Names: []string{"repo-config"},
		}
		r := &GitTargetReconciler{Client: fake.NewClientBuilder().
			WithScheme(scScheme(t)).WithObjects(cleanProvider()).Build()}

		refusal := r.supersededFieldRefusal(context.Background(), target, "ns")

		require.NotEmpty(t, refusal)
		assert.Contains(t, refusal, "spec.allowedSourceNamespaces")
	})

	t.Run("the referenced provider's stored field", func(t *testing.T) {
		provider := cleanProvider()
		//nolint:staticcheck // setting the relocated field is the point.
		provider.Spec.Push = &configbutleraiv1alpha3.PushStrategy{CommitWindow: ptr.To("30s")}
		r := &GitTargetReconciler{Client: fake.NewClientBuilder().
			WithScheme(scScheme(t)).WithObjects(provider).Build()}

		refusal := r.supersededFieldRefusal(context.Background(), cleanTarget(), "ns")

		require.NotEmpty(t, refusal,
			"a provider's stored commit fields must refuse the TARGET, since the target is what "+
				"wires the worker that would otherwise write on settings nobody chose")
		assert.Contains(t, refusal, "spec.push")
	})

	t.Run("an unreadable provider is not this gate's fault to report", func(t *testing.T) {
		// validateProviderAndBranch owns the missing-provider case and reports it with its own
		// reason; duplicating it here would give one fault two vocabularies.
		r := &GitTargetReconciler{Client: fake.NewClientBuilder().WithScheme(scScheme(t)).Build()}

		assert.Empty(t, r.supersededFieldRefusal(context.Background(), cleanTarget(), "ns"))
	})
}
