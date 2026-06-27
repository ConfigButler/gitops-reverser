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

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

var _ = Describe("ClusterWatchRule Controller", func() {
	Context("When CRD validation runs", func() {
		It("should reject subresource entries in resources", func() {
			ctx := context.Background()
			clusterRule := &configbutleraiv1alpha2.ClusterWatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name: "invalid-subresource-cluster-rule",
				},
				Spec: configbutleraiv1alpha2.ClusterWatchRuleSpec{
					TargetRef: configbutleraiv1alpha2.NamespacedTargetReference{
						Kind:      "GitTarget",
						Name:      "target",
						Namespace: "default",
					},
					Rules: []configbutleraiv1alpha2.ClusterResourceRule{{
						Scope:     configbutleraiv1alpha2.ResourceScopeNamespaced,
						Resources: []string{"pods/*"},
					}},
				},
			}

			err := k8sClient.Create(ctx, clusterRule)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("^[^/]*$"))
		})
	})

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
			clusterRule := &configbutleraiv1alpha2.ClusterWatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name: "missing-target-rule",
				},
				Spec: configbutleraiv1alpha2.ClusterWatchRuleSpec{
					TargetRef: configbutleraiv1alpha2.NamespacedTargetReference{
						Kind:      "GitTarget",
						Name:      "nonexistent-target",
						Namespace: "default",
					},
					Rules: []configbutleraiv1alpha2.ClusterResourceRule{
						{
							Scope:     configbutleraiv1alpha2.ResourceScopeCluster,
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

			By("Verifying the ClusterWatchRule has GitTargetNotFound condition")
			updatedRule := &configbutleraiv1alpha2.ClusterWatchRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "missing-target-rule"}, updatedRule)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedRule.Status.Conditions).To(HaveLen(5))
			var condition, streamsRunning, gitTargetReady, reconciling, stalled metav1.Condition
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
			Expect(condition.Reason).To(Equal(ClusterWatchRuleReasonGitTargetNotFound))
			Expect(streamsRunning.Status).To(Equal(metav1.ConditionUnknown))
			Expect(streamsRunning.Reason).To(Equal(GitTargetStreamsRunningReasonNotReady))
			Expect(gitTargetReady.Status).To(Equal(metav1.ConditionFalse))
			Expect(gitTargetReady.Reason).To(Equal(ClusterWatchRuleReasonGitTargetNotFound))
			Expect(reconciling.Status).To(Equal(metav1.ConditionFalse))
			Expect(stalled.Status).To(Equal(metav1.ConditionTrue))
			Expect(stalled.Reason).To(Equal(ClusterWatchRuleReasonGitTargetNotFound))

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
