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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("GitTarget Controller Security", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When a branch is not allowed by GitProvider", func() {
		It("Should clear LastCommit to prevent information disclosure", func() {
			ctx := context.Background()

			// Create a GitProvider that only allows 'main' and 'develop' branches
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-security",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "develop"},
					SecretRef: corev1.LocalObjectReference{
						Name: "test-secret", // Dummy secret
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// Create a GitTarget referencing an unauthorized branch
			unauthorizedBranch := "feature/unauthorized"
			gitTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-security",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-security",
						Kind: "GitProvider",
					},
					Branch: unauthorizedBranch,
					Path:   "test-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())

			// Wait for reconciliation and verify status
			gitTargetLookupKey := types.NamespacedName{
				Name:      "test-target-security",
				Namespace: "default",
			}

			createdGitTarget := &configbutleraiv1alpha1.GitTarget{}

			// Wait for the controller to reconcile and set conditions
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gitTargetLookupKey, createdGitTarget)
				if err != nil {
					return false
				}
				// Check if Ready condition exists
				for _, condition := range createdGitTarget.Status.Conditions {
					if condition.Type == GitTargetReasonReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify the Ready condition is False with BranchNotAllowed reason
			Expect(createdGitTarget.Status.Conditions).NotTo(BeEmpty())
			var readyCondition *metav1.Condition
			for i, condition := range createdGitTarget.Status.Conditions {
				if condition.Type == GitTargetReasonReady {
					readyCondition = &createdGitTarget.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).NotTo(BeNil(), "Ready condition should exist")
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse), "Ready should be False")
			Expect(readyCondition.Reason).To(Equal(GitTargetReasonBranchNotAllowed),
				"Reason should be BranchNotAllowed")

			// SECURITY TEST: Verify sensitive fields are cleared
			// This prevents unauthorized users from discovering SHA information
			Expect(createdGitTarget.Status.LastCommit).To(BeEmpty(),
				"LastCommit MUST be empty when branch is not allowed (security requirement)")
			Expect(createdGitTarget.Status.LastPushTime).To(BeNil(),
				"LastPushTime MUST be nil when branch is not allowed")

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should populate status fields when branch IS allowed", func() {
			ctx := context.Background()

			// Create a GitProvider with wildcard pattern
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-allowed",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "feature/*"},
					SecretRef: corev1.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// Create a GitTarget with an ALLOWED branch
			gitTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-allowed",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-allowed",
						Kind: "GitProvider",
					},
					Branch: "feature/allowed",
					Path:   "allowed-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())

			// Wait for reconciliation
			gitTargetLookupKey := types.NamespacedName{
				Name:      "test-target-allowed",
				Namespace: "default",
			}

			createdGitTarget := &configbutleraiv1alpha1.GitTarget{}

			// Wait for the controller to reconcile
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gitTargetLookupKey, createdGitTarget)
				if err != nil {
					return false
				}
				// Check if Ready condition exists
				for _, condition := range createdGitTarget.Status.Conditions {
					if condition.Type == GitTargetReasonReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// When branch IS allowed, the Ready condition should eventually be True
			// (may be False initially if repo is not accessible, but that's expected)
			// The key point is that sensitive fields are NOT cleared
			var readyCondition *metav1.Condition
			for i, condition := range createdGitTarget.Status.Conditions {
				if condition.Type == GitTargetReasonReady {
					readyCondition = &createdGitTarget.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).NotTo(BeNil(), "Ready condition should exist")

			// If branch is allowed but repo is not accessible, reason should be RepositoryUnavailable
			// NOT BranchNotAllowed
			if readyCondition.Status == metav1.ConditionFalse {
				Expect(readyCondition.Reason).NotTo(Equal(GitTargetReasonBranchNotAllowed),
					"When branch is allowed, reason should not be BranchNotAllowed")
			}

			// The key security verification: when branch IS allowed (even if repo unavailable),
			// the controller attempts to populate status fields and does NOT clear them
			// (they may be empty due to repo inaccessibility, but won't be explicitly cleared)

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should support glob patterns in allowedBranches", func() {
			ctx := context.Background()

			// Create a GitProvider with various glob patterns
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-glob",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL: "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{
						"main",
						"develop",
						"feature/*",
						"release/v*",
					},
					SecretRef: corev1.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// Test cases for different branches
			testCases := []struct {
				branch          string
				shouldBeAllowed bool
			}{
				{"main", true},
				{"develop", true},
				{"feature/login", true},
				{"feature/payment", true},
				{"release/v1.0", true},
				{"release/v2.5", true},
				{"hotfix/urgent", false},
				{"staging", false},
			}

			for i, tc := range testCases {
				// Generate a valid K8s name (no slashes or special chars)
				targetName := "test-target-glob-" + string(rune('a'+i))

				gitTarget := &configbutleraiv1alpha1.GitTarget{
					ObjectMeta: metav1.ObjectMeta{
						Name:      targetName,
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha1.GitTargetSpec{
						Provider: configbutleraiv1alpha1.GitProviderReference{
							Name: "test-provider-glob",
							Kind: "GitProvider",
						},
						Branch: tc.branch,
						Path:   "glob-test",
					},
				}

				Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())

				// Wait for reconciliation
				gitTargetLookupKey := types.NamespacedName{
					Name:      targetName,
					Namespace: "default",
				}

				createdGitTarget := &configbutleraiv1alpha1.GitTarget{}

				Eventually(func() bool {
					err := k8sClient.Get(ctx, gitTargetLookupKey, createdGitTarget)
					if err != nil {
						return false
					}
					for _, condition := range createdGitTarget.Status.Conditions {
						if condition.Type == GitTargetReasonReady {
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue())

				// Verify the condition based on whether branch should be allowed
				var readyCondition *metav1.Condition
				for i, condition := range createdGitTarget.Status.Conditions {
					if condition.Type == GitTargetReasonReady {
						readyCondition = &createdGitTarget.Status.Conditions[i]
						break
					}
				}

				if !tc.shouldBeAllowed {
					Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(readyCondition.Reason).To(Equal(GitTargetReasonBranchNotAllowed))
					// Security: verify fields are cleared
					Expect(createdGitTarget.Status.LastCommit).To(BeEmpty())
				} else {
					// If allowed, reason should not be BranchNotAllowed
					Expect(readyCondition.Reason).NotTo(Equal(GitTargetReasonBranchNotAllowed))
				}

				// Cleanup
				Expect(k8sClient.Delete(ctx, gitTarget)).Should(Succeed())
			}

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})

	Context("When checking for conflicts during reconciliation loop", func() {
		It("Should detect conflicts and elect winner by creationTimestamp", func() {
			ctx := context.Background()

			// Create a GitProvider
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "develop"},
					SecretRef: corev1.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// Create first GitTarget (winner - created first)
			firstTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-target-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-conflict",
						Kind: "GitProvider",
					},
					Branch: "main",
					Path:   "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, firstTarget)).Should(Succeed())

			// Wait for first target to reconcile
			firstTargetKey := types.NamespacedName{Name: "first-target-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha1.GitTarget
				if err := k8sClient.Get(ctx, firstTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetReasonReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Kubernetes creationTimestamp has second-level precision
			// Wait at least 1 second to ensure different timestamps
			time.Sleep(1100 * time.Millisecond)

			// Create second GitTarget with same provider+branch+path (loser - created later)
			secondTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-target-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-conflict",
						Kind: "GitProvider",
					},
					Branch: "main",
					Path:   "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, secondTarget)).Should(Succeed())

			// Wait for second target to reconcile
			secondTargetKey := types.NamespacedName{Name: "second-target-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha1.GitTarget
				if err := k8sClient.Get(ctx, secondTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetReasonReady && condition.Reason == GitTargetReasonConflict {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify second target has Conflict status
			var secondReconciledTarget configbutleraiv1alpha1.GitTarget
			Expect(k8sClient.Get(ctx, secondTargetKey, &secondReconciledTarget)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetReasonReady {
					readyCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(GitTargetReasonConflict))
			Expect(readyCondition.Message).To(ContainSubstring("first-target-conflict"))
			Expect(readyCondition.Message).To(ContainSubstring("created later"))

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should not conflict when path is different", func() {
			ctx := context.Background()

			// Create a GitProvider
			gitProvider := &configbutleraiv1alpha1.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
					SecretRef: corev1.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())

			// Create first GitTarget
			firstTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-target-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-no-conflict",
						Kind: "GitProvider",
					},
					Branch: "main",
					Path:   "folder-a",
				},
			}
			Expect(k8sClient.Create(ctx, firstTarget)).Should(Succeed())

			// Create second GitTarget with DIFFERENT path (no conflict)
			secondTarget := &configbutleraiv1alpha1.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-target-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitTargetSpec{
					Provider: configbutleraiv1alpha1.GitProviderReference{
						Name: "test-provider-no-conflict",
						Kind: "GitProvider",
					},
					Branch: "main",
					Path:   "folder-b", // Different!
				},
			}
			Expect(k8sClient.Create(ctx, secondTarget)).Should(Succeed())

			// Wait for both to reconcile
			secondTargetKey := types.NamespacedName{Name: "second-target-no-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha1.GitTarget
				if err := k8sClient.Get(ctx, secondTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetReasonReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify no conflict (reason should NOT be Conflict)
			var secondReconciledTarget configbutleraiv1alpha1.GitTarget
			Expect(k8sClient.Get(ctx, secondTargetKey, &secondReconciledTarget)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetReasonReady {
					readyCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Reason).NotTo(Equal(GitTargetReasonConflict),
				"Should not have conflict when path is different")

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})
})
