// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

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

		// Applied as unstructured, because the Go types no longer have the field to set, and
		// through a client that does not ask for strict field validation. That is the QUIET half
		// of a removal and the one worth pinning: `kubectl apply` would reject this manifest by
		// naming the unknown field, but a GitOps controller reconciling it on the user's behalf
		// may not. See docs/facts/crd-upgrade-strategies.md.
		legacy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "ClusterWatchRule",
			"metadata":   map[string]any{"name": "legacy-namespaced-scope"},
			"spec": map[string]any{
				"gitTargetRef": map[string]any{"name": "any-target", "namespace": "default"},
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
		provider := &configbutleraiv1alpha3.KubeProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed-source-scope", Namespace: "default"},
			Spec: configbutleraiv1alpha3.KubeProviderSpec{
				SecretRef:  &meta.SecretKeyReference{Name: "kc"},
				AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-a"}},
				AllowCrossNamespace: &configbutleraiv1alpha3.KubeProviderCrossNamespace{
					Sources: true, Appliers: true,
				},
			},
		}
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, provider) })

		var storedProvider configbutleraiv1alpha3.KubeProvider
		Expect(k8sClient.Get(ctx, provider.Key(), &storedProvider)).To(Succeed())
		Expect(storedProvider.Spec.AccessFrom).NotTo(BeNil(), "accessFrom must round-trip, not be pruned")
		Expect(storedProvider.AllowsCrossNamespaceSources()).To(BeTrue(), "allowCrossNamespace must round-trip")
		Expect(storedProvider.AllowsCrossNamespaceAppliers()).To(BeTrue())

		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed-commit", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("any-provider", "main")},
				Path:            "clusters/prod",
				Commit: &configbutleraiv1alpha3.GitTargetCommitSpec{
					Window: &configbutleraiv1alpha3.CommitWindow{
						IdleTimeout: &metav1.Duration{Duration: 30 * time.Second},
					},
					Message: &configbutleraiv1alpha3.CommitMessageSpec{
						LiveTemplate: "chore(mirror): {{ .Count }}",
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
		Expect(storedTarget.Spec.Commit.Window.IdleTimeout.Duration).To(Equal(30 * time.Second))
		Expect(storedTarget.Spec.Commit.Message.LiveTemplate).To(Equal("chore(mirror): {{ .Count }}"))
	})

	It("accepts a ClusterWatchRule that omits scope, defaulting it to Cluster", func() {
		ctx := context.Background()

		rule := &configbutleraiv1alpha3.ClusterWatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-only-rule"},
			Spec: configbutleraiv1alpha3.ClusterWatchRuleSpec{
				GitTargetRef: meta.NamespacedObjectReference{
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
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
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
				GitTargetRef: meta.LocalObjectReference{Name: "any-target"},
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

// Collapsing our reference types onto Flux's dropped the MinLength=1 those types carried on name,
// and `{name: ""}` is not a harmless value: GitProvider.spec.secretRef treats an absent credential
// as anonymous access, so an empty name would downgrade a private repository to anonymous rather
// than fail. The constraint is back as a field-level CEL rule on every reference, and this is the
// guard that it stays there.
var _ = Describe("Reference names must not be empty", func() {
	It("refuses an empty secretRef name rather than reading it as anonymous access", func() {
		ctx := context.Background()

		provider := &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-secret-name", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitProviderSpec{
				URL:             "https://example.com/repo.git",
				AllowedBranches: []string{"main"},
				SecretRef:       &meta.LocalObjectReference{Name: ""},
			},
		}

		err := k8sClient.Create(ctx, provider)

		Expect(err).To(HaveOccurred(),
			"an empty secretRef.name must be refused, not read as anonymous access")
		Expect(err.Error()).To(ContainSubstring("must not be empty"))
	})

	It("refuses an empty name on the branch, provider and target references", func() {
		ctx := context.Background()

		target := &configbutleraiv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-branch-name", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitTargetSpec{
				KubeProviderRef: testKubeProviderRef(),
				GitBranchRef:    meta.NamespacedObjectReference{Name: ""},
				Path:            "clusters/prod",
			},
		}
		Expect(k8sClient.Create(ctx, target)).NotTo(Succeed(),
			"an empty gitBranchRef.name names nothing and must be refused at admission")

		branch := &configbutleraiv1alpha3.GitBranch{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-provider-name", Namespace: "default"},
			Spec: configbutleraiv1alpha3.GitBranchSpec{
				GitProviderRef: meta.NamespacedObjectReference{Name: ""}, Branch: "main",
			},
		}
		Expect(k8sClient.Create(ctx, branch)).NotTo(Succeed(),
			"an empty gitProviderRef.name names nothing and must be refused at admission")

		rule := &configbutleraiv1alpha3.WatchRule{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-target-name", Namespace: "default"},
			Spec: configbutleraiv1alpha3.WatchRuleSpec{
				GitTargetRef: meta.LocalObjectReference{Name: ""},
				Rules: []configbutleraiv1alpha3.ResourceRule{
					{Resources: []string{"configmaps"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rule)).NotTo(Succeed(),
			"an empty gitTargetRef.name names nothing and must be refused at admission")
	})
})

// spec.clusterProviderRef was replaced by spec.kubeProviderRef on every kind that named a source
// cluster. A pruned field would apply cleanly and leave a manifest written for the old API looking
// accepted, so the old spelling stays in each schema and a CEL rule refuses it, naming
// kubeProviderRef.
var _ = Describe("Retained and refused clusterProviderRef", func() {
	referrer := func(kind, name string, spec map[string]any) *unstructured.Unstructured {
		switch kind {
		case "GitTarget":
			spec["gitBranchRef"] = map[string]any{"name": "main"}
			spec["path"] = "apps"
		case "FluxClaim":
			spec["kustomizationRef"] = map[string]any{"name": "apps"}
		case "ArgoCDClaim":
			spec["applicationRef"] = map[string]any{"name": "apps"}
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	for _, kind := range []string{"GitTarget", "FluxClaim", "ArgoCDClaim"} {
		It("refuses spec.clusterProviderRef on a "+kind+", naming spec.kubeProviderRef", func() {
			ctx := context.Background()
			obj := referrer(kind, "legacy-cluster-provider", map[string]any{
				"kubeProviderRef":    map[string]any{"name": "local"},
				"clusterProviderRef": map[string]any{"name": "default"},
			})
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "the old field must not apply cleanly")
			Expect(err.Error()).To(ContainSubstring("spec.clusterProviderRef was replaced by spec.kubeProviderRef"))
		})

		It("requires spec.kubeProviderRef on a "+kind+", with no default", func() {
			ctx := context.Background()
			err := k8sClient.Create(ctx, referrer(kind, "no-kube-provider-ref", map[string]any{}))
			Expect(err).To(HaveOccurred(), "an omitted reference must not fall back to any provider")
			Expect(err.Error()).To(ContainSubstring("kubeProviderRef"))
		})
	}
})

// GitTarget.spec.gitProviderRef, spec.branch and spec.parentBranch moved to a GitBranch. A pruned
// field would apply cleanly and leave the GitTarget naming no branch, so the old spellings stay in
// the schema and a CEL rule refuses them, naming gitBranchRef.
var _ = Describe("Retained and refused GitTarget branch fields", func() {
	legacyTarget := func(name string, spec map[string]any) *unstructured.Unstructured {
		spec["gitBranchRef"] = map[string]any{"name": "main"}
		spec["kubeProviderRef"] = map[string]any{"name": "local"}
		spec["path"] = "apps"
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitTarget",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	It("refuses spec.gitProviderRef, spec.branch and spec.parentBranch, naming spec.gitBranchRef", func() {
		ctx := context.Background()

		for name, spec := range map[string]map[string]any{
			"legacy-provider": {"gitProviderRef": map[string]any{"name": "platform"}},
			"legacy-branch":   {"branch": "main"},
			"legacy-parent":   {"parentBranch": "develop"},
		} {
			err := k8sClient.Create(ctx, legacyTarget(name, spec))
			Expect(err).To(HaveOccurred(), "%s: the old field must not apply cleanly", name)
			Expect(err.Error()).To(ContainSubstring("moved to a GitBranch; set spec.gitBranchRef"), name)
		}
	})

	It("requires spec.gitBranchRef", func() {
		ctx := context.Background()

		target := legacyTarget("no-branch-ref", map[string]any{})
		Expect(unstructured.SetNestedField(target.Object, nil, "spec", "gitBranchRef")).To(Succeed())
		delete(target.Object["spec"].(map[string]any), "gitBranchRef")
		err := k8sClient.Create(ctx, target)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("gitBranchRef"))
	})
})

// GitProvider.spec.receiver became the GitReceiver kind. A pruned field would apply cleanly and leave
// a provider that used to be notified silently unnotified, so the old spelling stays in the schema
// and a CEL rule refuses it, naming the replacement.
var _ = Describe("Retained and refused GitProvider receiver field", func() {
	legacyProvider := func(name string, spec map[string]any) *unstructured.Unstructured {
		spec["url"] = "https://github.com/acme/platform.git"
		spec["allowedBranches"] = []any{"main"}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       "GitProvider",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}
	}

	It("refuses spec.receiver in its old shape, or any other, naming the GitReceiver that replaces it", func() {
		ctx := context.Background()

		for name, receiver := range map[string]any{
			"legacy-receiver": map[string]any{"type": "github", "secretRef": map[string]any{"name": "token"}},
			"empty-receiver":  map[string]any{},
		} {
			err := k8sClient.Create(ctx, legacyProvider(name, map[string]any{"receiver": receiver}))
			Expect(err).To(HaveOccurred(), "%s: the old field must not apply cleanly", name)
			Expect(err.Error()).To(ContainSubstring("spec.receiver was replaced by a GitReceiver"), name)
			Expect(err.Error()).To(ContainSubstring("whose spec.url they name"), name)
		}
	})

	It("requires a GitReceiver's type and secretRef, and refuses an unknown type", func() {
		ctx := context.Background()

		receiver := func(name string, spec map[string]any) *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "configbutler.ai/v1alpha3",
				"kind":       "GitReceiver",
				"metadata":   map[string]any{"name": name, "namespace": "default"},
				"spec":       spec,
			}}
		}
		valid := receiver("valid-receiver", map[string]any{
			"type": "gitea", "secretRef": map[string]any{"name": "token"},
		})
		Expect(k8sClient.Create(ctx, valid)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, valid) })

		for name, tc := range map[string]struct {
			spec map[string]any
			want string
		}{
			"no-type":      {map[string]any{"secretRef": map[string]any{"name": "token"}}, "type"},
			"no-secret":    {map[string]any{"type": "github"}, "secretRef"},
			"empty-secret": {map[string]any{"type": "github", "secretRef": map[string]any{"name": ""}}, "secretRef.name"},
			"bitbucket":    {map[string]any{"type": "bitbucket", "secretRef": map[string]any{"name": "token"}}, "type"},
		} {
			err := k8sClient.Create(ctx, receiver(name, tc.spec))
			Expect(err).To(HaveOccurred(), name)
			Expect(err.Error()).To(ContainSubstring(tc.want), name)
		}
	})
})
