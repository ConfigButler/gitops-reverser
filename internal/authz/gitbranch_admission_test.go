// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// branchIn is the GitBranch namespace/main, with one spec.allowedGitTargets grant per writers entry.
func branchIn(namespace string, writers ...configv1alpha3.ConsumerNamespaces) *configv1alpha3.GitBranch {
	grants := make([]configv1alpha3.GitBranchGitTargetGrant, 0, len(writers))
	for _, w := range writers {
		grants = append(grants, configv1alpha3.GitBranchGitTargetGrant{ConsumerNamespaces: w})
	}
	return &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "main"},
		Spec: configv1alpha3.GitBranchSpec{
			GitProviderRef: meta.NamespacedObjectReference{Name: "monorepo"}, Branch: "main",
			AllowedGitTargets: grants,
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

// TestGitTargetAdmitted_GitBranchAllowedGitTargets: a GitTarget writes through a GitBranch only while
// one of that GitBranch's spec.allowedGitTargets admits its namespace, the GitBranch's own namespace
// included.
func TestGitTargetAdmitted_GitBranchAllowedGitTargets(t *testing.T) {
	// The KubeProvider gate passes in every case, so only the GitBranch's decides.
	source := providerNamed("prod-eu-1", mirrorFrom(testNS))
	same := configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame}
	tests := []struct {
		name        string
		objects     []client.Object
		target      *configv1alpha3.GitTarget
		allowed     bool
		wantMessage string
	}{
		{
			name:    "its own namespace, with no grant at all",
			objects: []client.Object{source, branchIn(testNS)},
			target:  targetWriting("", "main"),
			wantMessage: `GitBranch team-a/main admits no namespace in spec.allowedGitTargets; namespace "team-a" ` +
				"is not admitted",
		},
		{
			name:    "its own namespace, admitted by from: Same",
			objects: []client.Object{source, branchIn(testNS, same)},
			target:  targetWriting(testNS, "main"),
			allowed: true,
		},
		{
			name:        "another namespace, not admitted by from: Same",
			objects:     []client.Object{source, namespaceLabeled(nil), branchIn("platform", same)},
			target:      targetWriting("platform", "main"),
			wantMessage: `GitBranch platform/main admits platform in spec.allowedGitTargets; namespace "team-a"`,
		},
		{
			name: "another namespace, admitted by name",
			objects: []client.Object{source, namespaceLabeled(nil), branchIn("platform", same,
				configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromNames,
					Names: []configv1alpha3.GrantNamespace{testNS}})},
			target:  targetWriting("platform", "main"),
			allowed: true,
		},
		{
			name: "another namespace, admitted by its labels",
			objects: []client.Object{source, namespaceLabeled(map[string]string{"tenant": "yes"}),
				branchIn("platform", configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "yes"}}})},
			target:  targetWriting("platform", "main"),
			allowed: true,
		},
		{
			name: "a grant naming other namespaces, listed in the message",
			objects: []client.Object{source, namespaceLabeled(nil), branchIn("platform", same,
				configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromNames,
					Names: []configv1alpha3.GrantNamespace{"team-b", "team-c"}})},
			target:      targetWriting("platform", "main"),
			wantMessage: "GitBranch platform/main admits platform, team-b, team-c in spec.allowedGitTargets",
		},
		{
			name:    "a GitBranch that does not exist lends nothing, and is reported elsewhere",
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
				assert.Contains(t, decision.Message, tt.wantMessage)
			}
		})
	}
}

// sharedRepo is the GitProvider platform/shared-repo with grants as its spec.allowedGitBranches.
func sharedRepo(grants ...configv1alpha3.GitProviderGitBranchGrant) *configv1alpha3.GitProvider {
	return &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: platformNS, Name: "shared-repo"},
		Spec: configv1alpha3.GitProviderSpec{
			URL:                "https://git.example.com/platform/config.git",
			AllowedGitBranches: grants,
		},
	}
}

// claiming is a GitBranch in namespace claiming branch through platform/shared-repo.
func claiming(namespace, branch string) *configv1alpha3.GitBranch {
	return &configv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "live"},
		Spec: configv1alpha3.GitBranchSpec{
			GitProviderRef: meta.NamespacedObjectReference{Name: "shared-repo", Namespace: platformNS},
			Branch:         branch,
		},
	}
}

// TestGitBranchClaimAdmitted is the walkthrough of docs/finished/provider-access-rules.md: tenants may
// claim their own prefix, and the platform keeps main.
func TestGitBranchClaimAdmitted(t *testing.T) {
	tenant := map[string]string{"platform.example.com/tenant": "true"}
	provider := sharedRepo(
		configv1alpha3.GitProviderGitBranchGrant{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromSame},
			BranchPatterns:     []configv1alpha3.BranchPattern{"main"},
		},
		configv1alpha3.GitProviderGitBranchGrant{
			ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{
				From:     configv1alpha3.GrantFromSelector,
				Selector: &metav1.LabelSelector{MatchLabels: tenant},
			},
			BranchPatterns: []configv1alpha3.BranchPattern{"teams/{consumer.namespace}/*", "feature/*"},
		},
	)
	namespaces := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: tenant}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-c"}},
	}
	tests := []struct {
		name        string
		branch      *configv1alpha3.GitBranch
		provider    *configv1alpha3.GitProvider
		wantReason  string
		wantMessage string
	}{
		{"a tenant claims its own prefix", claiming(testNS, "teams/team-a/live"), provider, "", ""},
		{"a tenant claims a feature branch", claiming(testNS, "feature/checkout/v2"), provider, "", ""},
		{"a tenant may not claim main", claiming(testNS, "main"), provider, ReasonBranchNotAllowed,
			`namespace "team-a" may claim "feature/*", "teams/team-a/*" through GitProvider platform/shared-repo ` +
				`(spec.allowedGitBranches); requested branch "main" is not allowed`},
		{"a tenant may not claim another team's branch", claiming(testNS, "teams/team-b/live"), provider,
			ReasonBranchNotAllowed, `requested branch "teams/team-b/live" is not allowed`},
		{
			"{consumer.namespace} is a whole segment: team-a never matches team-ab",
			claiming(testNS, "teams/team-ab/live"),
			provider,
			ReasonBranchNotAllowed,
			"",
		},
		{"an unlabelled namespace is not admitted at all", claiming("team-c", "teams/team-c/live"), provider,
			ReasonNamespaceNotAuthorized, `GitProvider platform/shared-repo admits platform, namespaces labelled ` +
				`platform.example.com/tenant=true in spec.allowedGitBranches; namespace "team-c" is not admitted`},
		{"the platform claims main through from: Same", claiming(platformNS, "main"), provider, "", ""},
		{"the provider's own namespace needs a grant too", claiming(platformNS, "main"), sharedRepo(),
			ReasonNamespaceNotAuthorized, "admits no namespace in spec.allowedGitBranches"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).WithObjects(namespaces...).Build()
			decision, err := GitBranchClaimAdmitted(context.Background(), reader, tt.branch, tt.provider)
			require.NoError(t, err)
			assert.Equal(t, tt.wantReason == "", decision.Allowed, decision.Message)
			assert.Equal(t, tt.wantReason, decision.Reason)
			assert.Contains(t, decision.Message, tt.wantMessage)
		})
	}
}

// TestGitBranchClaimAdmitted_ReadErrorIsNoRevocation: a transient read keeps the caller's last
// answer; it must never come back as a denial.
func TestGitBranchClaimAdmitted_ReadErrorIsNoRevocation(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	reader := fake.NewClientBuilder().WithScheme(admissionScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()
	decision, err := GitBranchClaimAdmitted(context.Background(), reader, claiming(testNS, "main"), sharedRepo())
	require.ErrorIs(t, err, boom)
	assert.False(t, decision.Allowed)
	assert.Empty(t, decision.Reason)
}
