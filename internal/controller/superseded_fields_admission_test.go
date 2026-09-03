// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// Every field this project has superseded is now DELETED outright rather than retained and
// narrowed, and these specs run against the GENERATED CRDs so they say what that actually means.
//
// Deleting a field is the silent option: CRD pruning happens on write, so once the schema drops a
// field, re-applying a legacy manifest is ACCEPTED with the value pruned away — no error anywhere.
// The first spec pins exactly that, because it is the behaviour docs/UPGRADING.md's pre-upgrade
// inventory exists to compensate for: if an apply of a legacy manifest ever started failing
// instead, the migration guidance would be wrong in the other direction.
//
// The remaining specs pin the replacement spellings, which is what stops a typo in one of them
// surfacing as a silently pruned field and a mirror behaving as if unconfigured.
var _ = Describe("Superseded source-scope fields", func() {
	It("accepts and PRUNES a legacy manifest that still sets rules[].scope", func() {
		ctx := context.Background()

		// Applied as unstructured, because the Go types no longer have the field to set. This is
		// how a manifest written for 0.42 or earlier reaches a 0.43 apiserver.
		legacy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "ClusterWatchRule",
			"metadata":   map[string]any{"name": "legacy-namespaced-scope"},
			"spec": map[string]any{
				"targetRef": map[string]any{"name": "any-target", "namespace": "default"},
				"rules": []any{map[string]any{
					"resources": []any{"configmaps"},
					"scope":     "Namespaced",
				}},
			},
		}}

		Expect(k8sClient.Create(ctx, legacy)).To(Succeed(),
			"a removed field is pruned on write, so a legacy manifest applies cleanly")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		rules, _, err := unstructured.NestedSlice(legacy.Object, "spec", "rules")
		Expect(err).NotTo(HaveOccurred())
		Expect(rules).To(HaveLen(1))
		Expect(rules[0]).NotTo(HaveKey("scope"),
			"the value is dropped silently, which is why UPGRADING.md asks for an inventory first")
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
				ProviderRef: meta.LocalObjectReference{Name: "any-provider"},
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
				TargetRef: meta.NamespacedObjectReference{
					Name: "any-target", Namespace: "default",
				},
				Rules: []configbutleraiv1alpha3.ClusterResourceRule{{
					Resources: []string{"customresourcedefinitions"},
					APIGroups: []string{"apiextensions.k8s.io"},
				}},
			},
		}

		Expect(k8sClient.Create(ctx, rule)).To(Succeed(),
			"a ClusterWatchRule names types and nothing else: which scope it watches is the kind")
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rule) })
	})

	It("accepts rules[].sourceNamespace, including the wildcard", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "per-item-source", Namespace: "default"},
			Spec: configbutleraiv1alpha3.WatchRuleSpec{
				TargetRef: meta.LocalObjectReference{Name: "any-target"},
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
				TargetRef: meta.LocalObjectReference{Name: "any-target"},
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
