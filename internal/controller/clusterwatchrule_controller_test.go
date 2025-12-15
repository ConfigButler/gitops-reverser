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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("ClusterWatchRule Controller", func() {
	Context("When reconciling a ClusterWatchRule", func() {
		var (
			ctx        context.Context
			reconciler *ClusterWatchRuleReconciler
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = &ClusterWatchRuleReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RuleStore: testRuleStore,
			}
		})

		It("should handle non-existent ClusterWatchRule gracefully", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "non-existent"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should fail when GitTarget not found", func() {
			By("Creating a ClusterWatchRule referencing non-existent GitTarget")
			clusterRule := &configbutleraiv1alpha1.ClusterWatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name: "missing-target-rule",
				},
				Spec: configbutleraiv1alpha1.ClusterWatchRuleSpec{
					TargetRef: configbutleraiv1alpha1.NamespacedTargetReference{
						Kind:      "GitTarget",
						Name:      "nonexistent-target",
						Namespace: "default",
					},
					Rules: []configbutleraiv1alpha1.ClusterResourceRule{
						{
							Scope:     configbutleraiv1alpha1.ResourceScopeCluster,
							Resources: []string{"nodes"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, clusterRule)).Should(Succeed())

			By("Reconciling the ClusterWatchRule")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "missing-target-rule"},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the ClusterWatchRule has GitDestinationNotFound condition")
			updatedRule := &configbutleraiv1alpha1.ClusterWatchRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "missing-target-rule"}, updatedRule)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRule.Status.Conditions).To(HaveLen(1))
			condition := updatedRule.Status.Conditions[0]
			Expect(condition.Type).To(Equal(ConditionTypeReady))
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(ClusterWatchRuleReasonGitDestinationNotFound))

			// Cleanup
			Expect(k8sClient.Delete(ctx, clusterRule)).Should(Succeed())
		})

		// Note: Full integration tests with Ready GitRepoConfig are in E2E tests
		// Unit tests cannot properly test this because GitRepoConfig controller
		// keeps reconciling and failing Git validation in test environment
		PIt("should successfully reconcile with valid GitRepoConfig (tested in E2E)", func() {
			Skip("Requires real Git repository - tested in E2E tests")
		})

		PIt("should fail when GitRepoConfig does not allow cluster rules (tested in E2E)", func() {
			Skip("Requires real Git repository - tested in E2E tests")
		})

		PIt("should handle Namespaced scope with namespace selector (tested in E2E)", func() {
			Skip("Requires real Git repository - tested in E2E tests")
		})
	})
})
