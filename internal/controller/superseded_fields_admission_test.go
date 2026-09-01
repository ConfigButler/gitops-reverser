// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Both superseded fields were NARROWED rather than deleted, and this is the guard that they still
// are. It runs against the GENERATED CRDs, so it fails the moment somebody "cleans up" either field
// out of the Go types.
//
// Deleting a field is the silent option, which is why it was rejected: CRD pruning happens on WRITE,
// so once the schema drops a field, re-applying a legacy manifest is ACCEPTED with the value pruned
// away — no error anywhere — and the rule quietly changes what it mirrors. A retained-but-narrowed
// field turns that into an apply-time rejection an operator cannot miss.
var _ = Describe("Superseded source-scope fields are rejected, not pruned", func() {
	It("rejects ClusterWatchRule scope: Namespaced at admission", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-namespaced-scope"},
			Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
				TargetRef: configbutleraiv1alpha3.NamespacedTargetReference{
					Name: "any-target", Namespace: "default",
				},
				Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
					Resources: []string{"configmaps"},
					Scope:     configbutleraiv1alpha3.ResourceScopeNamespaced,
				}},
			},
		}

		err := k8sClient.Create(ctx, rule)

		Expect(err).To(HaveOccurred(),
			"a legacy namespaced ClusterWatchRule must FAIL to apply, never be silently pruned")
		Expect(err.Error()).To(ContainSubstring("scope"))
	})

	It("rejects every superseded source-scope field, naming its replacement", func() {
		ctx := context.Background()

		// Each of these is retained in the schema and refused, rather than deleted. A deleted field
		// is PRUNED on write with no error at all, and for allowSourceNamespaceOverride that would
		// silently revoke a delegation: every cross-namespace WatchRule through the provider would
		// stall, while the manifest still appeared to grant it.
		By("rejecting GitTarget.spec.allowedSourceNamespaces")
		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-source-scope", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "any-provider"},
				Branch:      "main",
				Path:        "clusters/prod",
				//nolint:staticcheck // setting the removed field is the point: it must be rejected.
				AllowedSourceNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{
					Names: []string{"repo-config"},
				},
			},
		}
		err := k8sClient.Create(ctx, target)
		Expect(err).To(HaveOccurred(),
			"a stored allowedSourceNamespaces must FAIL to re-apply, never be silently pruned")
		Expect(err.Error()).To(ContainSubstring("allowAnySourceNamespace"),
			"the refusal must name the replacement grant")

		By("rejecting ClusterProvider.spec.allowedNamespaces")
		renamedPolicy := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-allowed-namespaces"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				//nolint:staticcheck // setting the renamed field is the point.
				AllowedNamespaces: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
			},
		}
		err = k8sClient.Create(ctx, renamedPolicy)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.accessFrom"))

		By("rejecting ClusterProvider.spec.allowSourceNamespaceOverride")
		renamedFlag := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-override-flag"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
				//nolint:staticcheck // setting the renamed field is the point.
				AllowSourceNamespaceOverride: ptr.To(true),
			},
		}
		err = k8sClient.Create(ctx, renamedFlag)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.allowAnySourceNamespace"))

		By("rejecting GitProvider.spec.push")
		relocatedWindow := &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-push", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitProviderSpec{
				URL:             "git@github.com:example/repo.git",
				AllowedBranches: []string{"main"},
				//nolint:staticcheck // setting the relocated field is the point.
				Push: &configbutleraiv1alpha3.PushStrategy{CommitWindow: ptr.To("30s")},
			},
		}
		err = k8sClient.Create(ctx, relocatedWindow)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("GitTarget.spec.commit.window"))
	})

	It("accepts the replacement spellings", func() {
		ctx := context.Background()

		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed-source-scope"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AccessFrom:              &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
				AllowAnySourceNamespace: true,
			},
		}
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, provider) })

		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed-commit", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				ProviderRef: configbutleraiv1alpha3.GitProviderReference{Name: "any-provider"},
				Branch:      "main",
				Path:        "clusters/prod",
				Commit: &configbutleraiv1alpha3.GitTargetCommitSpec{
					Window: ptr.To("30s"),
					Message: &configbutleraiv1alpha3.CommitMessageSpec{
						GroupTemplate: "chore(mirror): {{ .Count }}",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
	})

	It("accepts a ClusterWatchRule that omits scope, defaulting it to Cluster", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-only-rule"},
			Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
				TargetRef: configbutleraiv1alpha3.NamespacedTargetReference{
					Name: "any-target", Namespace: "default",
				},
				Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
					Resources: []string{"customresourcedefinitions"},
					APIGroups: []string{"apiextensions.k8s.io"},
				}},
			},
		}

		Expect(k8sClient.Create(ctx, rule)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rule) })

		//nolint:staticcheck // reading the deprecated field is the point: it must still default.
		Expect(rule.Spec.Rules[0].Scope).To(Equal(configbutleraiv1alpha3.ResourceScopeCluster),
			"the field is omittable and defaults to Cluster, so a converted manifest need not set it")
	})

	It("accepts rules[].sourceNamespace, including the wildcard", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "per-item-source", Namespace: "default"},
			Spec: configbutleraiv1alpha3.WatchRuleSpec{
				TargetRef: configbutleraiv1alpha3.LocalTargetReference{Name: "any-target"},
				Rules: []configbutleraiv1alpha3.ResourceRule{
					{Resources: []string{"configmaps"}},
					{Resources: []string{"secrets"}, SourceNamespace: "repo-config"},
					{Resources: []string{"deployments"}, SourceNamespace: "*"},
				},
			},
		}

		Expect(k8sClient.Create(ctx, rule)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rule) })
	})

	It("rejects a structurally invalid rules[].sourceNamespace", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "malformed-source", Namespace: "default"},
			Spec: configbutleraiv1alpha3.WatchRuleSpec{
				TargetRef: configbutleraiv1alpha3.LocalTargetReference{Name: "any-target"},
				Rules: []configbutleraiv1alpha3.ResourceRule{{
					Resources: []string{"configmaps"}, SourceNamespace: "Not A Namespace",
				}},
			},
		}

		err := k8sClient.Create(ctx, rule)

		Expect(err).To(HaveOccurred(),
			"a malformed namespace must be rejected at admission rather than resolving to nothing "+
				"at compile time")
	})
})
