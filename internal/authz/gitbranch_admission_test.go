// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
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

// sharedGitBranch is the GitBranch platform/main, written with the GitProvider platform/provider.
func sharedGitBranch(provider string) *configv1alpha3.GitBranch {
	return &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "main"},
		Spec: configv1alpha3.GitBranchSpec{
			GitProviderRef: meta.LocalObjectReference{Name: provider}, Branch: "main",
		},
	}
}

// targetWriting is a GitTarget in testNS writing through the GitBranch namespace/name; an empty
// namespace means its own.
func targetWriting(namespace, name string) *configv1alpha3.GitTarget {
	target := targetIn("prod-eu-1")
	target.Spec.GitBranchRef = meta.NamespacedObjectReference{Name: name, Namespace: namespace}
	return target
}

// TestGitTargetAdmitted_GitBranchsProviderAccessFrom: a GitTarget writes with its GitBranch's
// GitProvider credential, so a GitBranch in another namespace is used only while that GitProvider's
// spec.accessFrom admits the GitTarget's namespace. A GitBranch in the GitTarget's own namespace
// needs no policy.
func TestGitTargetAdmitted_GitBranchsProviderAccessFrom(t *testing.T) {
	// The KubeProvider gate passes in every case, so only the GitProvider's decides.
	source := providerNamed("prod-eu-1", &configv1alpha3.NamespaceMatcher{Names: []string{testNS}})
	tests := []struct {
		name    string
		objects []client.Object
		target  *configv1alpha3.GitTarget
		allowed bool
	}{
		{
			name:    "its own namespace, with no policy at all",
			objects: []client.Object{source},
			target:  targetWriting("", "main"),
			allowed: true,
		},
		{
			name:    "its own namespace, named explicitly",
			objects: []client.Object{source},
			target:  targetWriting(testNS, "main"),
			allowed: true,
		},
		{
			name: "another namespace, with no policy",
			objects: []client.Object{
				source,
				sharedGitProvider(nil),
				sharedGitBranch("monorepo"),
				namespaceLabeled(nil),
			},
			target: targetWriting("platform", "main"),
		},
		{
			name: "another namespace, admitted by name",
			objects: []client.Object{source, namespaceLabeled(nil), sharedGitBranch("monorepo"),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{Names: []string{testNS}})},
			target:  targetWriting("platform", "main"),
			allowed: true,
		},
		{
			name: "another namespace, admitted by its labels",
			objects: []client.Object{
				source,
				namespaceLabeled(map[string]string{"tenant": "yes"}),
				sharedGitBranch("monorepo"),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "yes"}},
				}),
			},
			target:  targetWriting("platform", "main"),
			allowed: true,
		},
		{
			name: "another namespace, not admitted",
			objects: []client.Object{source, namespaceLabeled(nil), sharedGitBranch("monorepo"),
				sharedGitProvider(&configv1alpha3.NamespaceMatcher{Names: []string{"team-b"}})},
			target: targetWriting("platform", "main"),
		},
		{
			name:    "another namespace's GitBranch that does not exist wields nothing",
			objects: []client.Object{source},
			target:  targetWriting("platform", "missing"),
			allowed: true,
		},
		{
			name:    "another namespace's GitBranch whose GitProvider does not exist wields nothing",
			objects: []client.Object{source, sharedGitBranch("missing")},
			target:  targetWriting("platform", "main"),
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
				assert.Contains(t, decision.Message, "GitBranch platform/main")
				assert.Contains(t, decision.Message, "GitProvider platform/monorepo")
			}
		})
	}
}
