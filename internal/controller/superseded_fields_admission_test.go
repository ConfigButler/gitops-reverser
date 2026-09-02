// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ClusterWatchRule.spec.rules[].scope was NARROWED rather than deleted, and this is the guard that
// it still is. It runs against the GENERATED CRDs, so it fails the moment somebody "cleans up" the
// field out of the Go types.
//
// Deleting a field is the silent option: CRD pruning happens on write, so once the schema drops a
// field, re-applying a legacy manifest is ACCEPTED with the value pruned away — no error anywhere.
// A retained-but-narrowed field turns that into an apply-time rejection an operator cannot miss.
//
// The GitTarget/GitProvider/ClusterProvider fields this release removed took the OTHER option
// deliberately: they are deleted outright, so an old manifest is accepted and pruned, and
// docs/UPGRADING.md carries a pre-upgrade inventory instead. That trade is priced there, and the
// last spec below is what stops the replacement spellings regressing unnoticed.
var _ = Describe("Superseded source-scope fields", func() {
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

	It("accepts the replacement spellings on every kind the wave touched", func() {
		ctx := context.Background()

		// With the removed fields gone there is nothing left to reject, so the only thing worth
		// pinning is that what REPLACED them is really served. A typo in one of these names would
		// otherwise surface as a silently pruned field and a mirror behaving as if unconfigured.
		provider := &configbutleraiv1alpha3.ClusterProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed-source-scope"},
			Spec: configbutleraiv1alpha3.ClusterProviderSpec{
				AccessFrom:              &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
				AllowAnySourceNamespace: true,
			},
		}
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, provider) })

		var storedProvider configbutleraiv1alpha3.ClusterProvider
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: provider.Name}, &storedProvider)).To(Succeed())
		Expect(storedProvider.Spec.AccessFrom).NotTo(BeNil(), "accessFrom must round-trip, not be pruned")
		Expect(storedProvider.Spec.AllowAnySourceNamespace).To(BeTrue())

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

		var storedTarget configbutleraiv1alpha3.GitTarget
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: target.Name, Namespace: target.Namespace}, &storedTarget)).To(Succeed())
		Expect(storedTarget.Spec.Commit).NotTo(BeNil(), "spec.commit must round-trip, not be pruned")
		Expect(*storedTarget.Spec.Commit.Window).To(Equal("30s"))
		Expect(storedTarget.Spec.Commit.Message.GroupTemplate).To(Equal("chore(mirror): {{ .Count }}"))
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
