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
			By("creating the custom resource for the Kind GitRepoConfig")
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-config",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main"},
					SecretRef: &configbutleraiv1alpha1.LocalObjectReference{
						Name: "git-credentials",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			By("creating a GitDestination referencing the GitRepoConfig")
			dest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "test-repo-config"},
					Branch:     "main",
					BaseFolder: "default/test",
				},
			}
			Expect(k8sClient.Create(ctx, dest)).To(Succeed())

			By("creating the custom resource for the Kind WatchRule")
			err := k8sClient.Get(ctx, typeNamespacedName, watchrule)
			if err != nil && errors.IsNotFound(err) {
				resource := &configbutleraiv1alpha1.WatchRule{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha1.WatchRuleSpec{
						DestinationRef: &configbutleraiv1alpha1.NamespacedName{Name: "test-destination"},
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

			dest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{Name: "test-destination", Namespace: "default"},
				dest,
			)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance GitDestination")
			Expect(k8sClient.Delete(ctx, dest)).To(Succeed())

			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{}
			err = k8sClient.Get(
				ctx,
				types.NamespacedName{Name: "test-repo-config", Namespace: "default"},
				gitRepoConfig,
			)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance GitRepoConfig")
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).To(Succeed())
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
			By("Creating GitRepoConfig with no access policy")
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-config",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/octocat/Hello-World",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

			// Wait for GitRepoConfig to be ready (it should become ready with the real GitHub repo)
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "local-config", Namespace: "default"}, gitRepoConfig)
				if err != nil {
					return false
				}
				for _, condition := range gitRepoConfig.Status.Conditions {
					if condition.Type == ConditionTypeReady && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, "60s", "2s").Should(BeTrue())

			By("Creating GitDestination in same namespace referencing the GitRepoConfig")
			dest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-dest",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "local-config"},
					Branch:     "main",
					BaseFolder: "ns/default",
				},
			}
			Expect(k8sClient.Create(ctx, dest)).Should(Succeed())

			By("Creating WatchRule in same namespace referencing DestinationRef")
			watchRule := &configbutleraiv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-rule",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.WatchRuleSpec{
					DestinationRef: &configbutleraiv1alpha1.NamespacedName{Name: "local-dest"},
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
			Expect(k8sClient.Delete(ctx, dest)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})
	})
})
