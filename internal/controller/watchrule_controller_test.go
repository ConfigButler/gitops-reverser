/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

var _ = Describe("WatchRule Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		watchrule := &configbutleraiv1alpha1.WatchRule{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind GitProvider")
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL: "https://github.com/test/repo.git",
					SecretRef: corev1.LocalObjectReference{
						Name: "git-credentials",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).To(Succeed())

			By("creating a GitTarget referencing the GitProvider")
			target := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
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
				resource := &configbutleraiv1alpha1.WatchRule{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha1.WatchRuleSpec{
						Target: configbutleraiv1alpha1.LocalTargetReference{Name: "test-target"},
						Rules: []configbutleraiv1alpha1.ResourceRule{
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
			resource := &configbutleraiv1alpha1.WatchRule{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance WatchRule")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			target := &configbutleraiv1alpha1.GitTarget{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{Name: "test-target", Namespace: "default"},
				target,
			)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance GitTarget")
			Expect(k8sClient.Delete(ctx, target)).To(Succeed())

			gitProvider := &configbutleraiv1alpha1.GitProvider{}
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
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-provider",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL: "https://github.com/octocat/Hello-World",
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// TODO: Wait for GitProvider to be ready (if we implement status update for it)

			By("Creating GitTarget in same namespace referencing the GitProvider")
			target := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-target",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "local-provider",
					},
					Branch: "main",
					Path:   "ns/default",
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			By("Creating WatchRule in same namespace referencing Target")
			watchRule := &configbutleraiv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-rule",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.WatchRuleSpec{
					Target: configbutleraiv1alpha1.LocalTargetReference{Name: "local-target"},
					Rules: []configbutleraiv1alpha1.ResourceRule{
						{
							Resources: []string{"pods"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, watchRule)).Should(Succeed())

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
			updatedRule := &configbutleraiv1alpha1.WatchRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "local-rule",
				Namespace: "default",
			}, updatedRule)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying WatchRule is Ready")
			Expect(updatedRule.Status.Conditions).To(HaveLen(1))
			condition := updatedRule.Status.Conditions[0]
			Expect(condition.Type).To(Equal(ConditionTypeReady))
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(WatchRuleReasonReady))

			// Cleanup
			Expect(k8sClient.Delete(ctx, watchRule)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})
})
