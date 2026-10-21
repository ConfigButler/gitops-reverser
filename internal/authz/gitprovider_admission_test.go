// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// sharedGitProvider is the GitProvider platform/monorepo, with policy as its spec.accessFrom.
func sharedGitProvider(policy *configv1alpha3.NamespaceMatcher) *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "monorepo"},
		Spec:       configv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform", AccessFrom: policy},
	}
}

// targetWriting is a GitTarget in testNS writing through the GitProvider namespace/name; an empty
// namespace means its own.
func targetWriting(namespace, name string) *configv1alpha3.GitTarget {
	target := targetIn("")
	target.Spec.GitProviderRef.Name = name
	target.Spec.GitProviderRef.Namespace = namespace
	return target
}

// TestGitTargetAdmitted_GitProviderAccessFrom: a GitTarget writes with its GitProvider's credential,
// so one in another namespace is used only while its spec.accessFrom admits the GitTarget's
// namespace. A GitProvider in the GitTarget's own namespace needs no policy.
func TestGitTargetAdmitted_GitProviderAccessFrom(t *testing.T) {
	// The ClusterProvider gate passes in every case, so only the GitProvider's decides.
	source := providerNamed(configv1alpha3.DefaultClusterProviderName,
		&configv1alpha3.NamespaceMatcher{Names: []string{testNS}})
	tests := []struct {
		name    string
		objects []client.Object
		target  *configv1alpha3.GitTarget
		allowed bool
	}{
		{
			name:    "its own namespace, with no policy at all",
			objects: []client.Object{source},
			target:  targetWriting("", "repo"),
			allowed: true,
		},
		{
			name:    "its own namespace, named explicitly",
			objects: []client.Object{source},
			target:  targetWriting(testNS, "repo"),
			allowed: true,
		},
		{
			name:    "another namespace, with no policy",
			objects: []client.Object{source, sharedGitProvider(nil), namespaceLabeled(nil)},
			target:  targetWriting("platform", "monorepo"),
		},
		{
			name: "another namespace, admitted by name",
			objects: []client.Object{source, namespaceLabeled(nil),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{Names: []string{testNS}})},
			target:  targetWriting("platform", "monorepo"),
			allowed: true,
		},
		{
			name: "another namespace, admitted by its labels",
			objects: []client.Object{source, namespaceLabeled(map[string]string{"tenant": "yes"}),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "yes"}},
				})},
			target:  targetWriting("platform", "monorepo"),
			allowed: true,
		},
		{
			name: "another namespace, not admitted",
			objects: []client.Object{source, namespaceLabeled(nil),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{Names: []string{"team-b"}})},
			target: targetWriting("platform", "monorepo"),
		},
		{
			name:    "another namespace's GitProvider that does not exist wields nothing",
			objects: []client.Object{source},
			target:  targetWriting("platform", "missing"),
			allowed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(tt.objects...).Build()

			decision, err := GitTargetAdmitted(context.Background(), reader, tt.target)

			require.NoError(t, err)
			assert.Equal(t, tt.allowed, decision.Allowed, decision.Message)
			if !tt.allowed {
				assert.Equal(t, ReasonNamespaceNotAuthorized, decision.Reason)
				assert.Contains(t, decision.Message, "GitProvider platform/monorepo")
			}
		})
	}
}
