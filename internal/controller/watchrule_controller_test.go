// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

var _ = Describe("WatchRule Controller", func() {
	Context("When CRD validation runs", func() {
		It("should reject subresource entries in resources", func() {
			ctx := context.Background()
			watchRule := &configbutleraiv1alpha3.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-subresource-rule",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.WatchRuleSpec{
					TargetRef: configbutleraiv1alpha3.LocalTargetReference{
						Kind: "GitTarget",
						Name: "target",
					},
					Rules: []configbutleraiv1alpha3.ResourceRule{{
						Resources: []string{"pods/log"},
					}},
				},
			}

			err := k8sClient.Create(ctx, watchRule)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("^[^/]*$"))
		})
	})

	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		watchrule := &configbutleraiv1alpha3.WatchRule{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind GitProvider")
			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test/repo.git",
					AllowedBranches: []string{"*"},
					SecretRef: &configbutleraiv1alpha3.LocalSecretReference{
						Name: "git-credentials",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).To(Succeed())

			By("creating a GitTarget referencing the GitProvider")
			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					ProviderRef: configbutleraiv1alpha3.GitProviderReference{
						Name: "test-provider",
					},
					Branch: "main",
					Path:   "default/test",
				},
			}
			Expect(k8sClient.Create(ctx, target)).To(Succeed())

			By("creating the custom resource for the Kind WatchRule")
			err := k8sClient.Get(ctx, typeNamespacedName, watchrule)
			if err != nil && errors.IsNotFound(err) {
				resource := &configbutleraiv1alpha3.WatchRule{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha3.WatchRuleSpec{
						TargetRef: configbutleraiv1alpha3.LocalTargetReference{
							Kind: "GitTarget",
							Name: "test-target",
						},
						Rules: []configbutleraiv1alpha3.ResourceRule{
							{
								Resources: []string{"Pod"},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &configbutleraiv1alpha3.WatchRule{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance WatchRule")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			target := &configbutleraiv1alpha3.GitTarget{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{Name: "test-target", Namespace: "default"},
				target,
			)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance GitTarget")
			Expect(k8sClient.Delete(ctx, target)).To(Succeed())

			gitProvider := &configbutleraiv1alpha3.GitProvider{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{Name: "test-provider", Namespace: "default"},
				gitProvider,
			)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance GitProvider")
			Expect(k8sClient.Delete(ctx, gitProvider)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &WatchRuleReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RuleStore: rulestore.NewStore(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When testing access policy validation with direct reconcile calls", func() {
		var (
			ctx        context.Context
			reconciler *WatchRuleReconciler
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = &WatchRuleReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RuleStore: testRuleStore,
			}
		})

		It("should work with same namespace (default behavior)", func() {
			By("Creating GitProvider")
			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-provider",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/octocat/Hello-World",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			// No wait on GitProvider readiness: reconcileWatchRuleViaTarget only requires the
			// GitProvider to exist (it is resolved by name), not to be Ready. The GitTarget
			// readiness wait below is the dependency gate this spec actually needs.

			By("Creating GitTarget in same namespace referencing the GitProvider")
			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-target",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					ProviderRef: configbutleraiv1alpha3.GitProviderReference{
						Name: "local-provider",
					},
					Branch: "main",
					Path:   "ns/default",
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			By("Creating WatchRule in same namespace referencing Target")
			watchRule := &configbutleraiv1alpha3.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-rule",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.WatchRuleSpec{
					TargetRef: configbutleraiv1alpha3.LocalTargetReference{
						Kind: "GitTarget",
						Name: "local-target",
					},
					Rules: []configbutleraiv1alpha3.ResourceRule{
						{
							Resources: []string{"pods"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, watchRule)).Should(Succeed())

			By("Waiting for the GitTarget controller to publish its not-yet-Ready status")
			// The WatchRule mirrors the referenced GitTarget's Ready condition. Without this
			// wait the direct reconcile below races the async GitTargetReconciler: if the
			// GitTarget has not published any condition yet, gitTargetReadyCondition falls back
			// to Unknown instead of the expected False/Progressing, which flaked in CI. Block
			// until the GitTarget settles on its deterministic Ready=False/Progressing state
			// (no streams run in envtest) so the WatchRule reconcile observes a stable dependency.
			gitTarget := &configbutleraiv1alpha3.GitTarget{}
			eventuallyConditionStatusReason(
				ctx,
				types.NamespacedName{Name: "local-target", Namespace: "default"},
				gitTarget,
				func() []metav1.Condition { return gitTarget.Status.Conditions },
				ConditionTypeReady,
				metav1.ConditionFalse,
				ReasonProgressing,
			)

			By("Reconciling the WatchRule directly")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "local-rule",
					Namespace: "default",
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying WatchRule is Ready")
			updatedRule := &configbutleraiv1alpha3.WatchRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "local-rule",
				Namespace: "default",
			}, updatedRule)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying WatchRule is reconciling until the GitTarget and streams are ready")
			Expect(updatedRule.Status.Conditions).To(HaveLen(6))
			var condition, streamsRunning, gitTargetReady, reconciling, stalled metav1.Condition
			var sourceAuthorized metav1.Condition
			for _, c := range updatedRule.Status.Conditions {
				if c.Type == ConditionTypeSourceNamespaceAuthorized {
					sourceAuthorized = c
				}
			}

			By("Verifying the legacy own-namespace rule is authorized without any policy or flag")
			Expect(sourceAuthorized.Status).To(Equal(metav1.ConditionTrue))
			Expect(sourceAuthorized.Reason).To(Equal(WatchRuleReasonLegacySourceNamespace))

			for _, c := range updatedRule.Status.Conditions {
				if c.Type == ConditionTypeReady {
					condition = c
				}
				if c.Type == ConditionTypeStreamsRunning {
					streamsRunning = c
				}
				if c.Type == ConditionTypeGitTargetReady {
					gitTargetReady = c
				}
				if c.Type == ConditionTypeReconciling {
					reconciling = c
				}
				if c.Type == ConditionTypeStalled {
					stalled = c
				}
			}
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(ReasonProgressing))
			Expect(streamsRunning.Status).To(Equal(metav1.ConditionFalse))
			Expect(streamsRunning.Reason).To(Equal(watch.StreamReasonNoResolvedTypes))
			Expect(gitTargetReady.Status).To(Equal(metav1.ConditionFalse))
			Expect(gitTargetReady.Reason).To(Equal(ReasonProgressing))
			Expect(reconciling.Status).To(Equal(metav1.ConditionTrue))
			Expect(stalled.Status).To(Equal(metav1.ConditionFalse))

			// Cleanup
			Expect(k8sClient.Delete(ctx, watchRule)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})
})
