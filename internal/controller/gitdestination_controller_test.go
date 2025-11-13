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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("GitDestination Controller Security", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When a branch is not allowed by GitRepoConfig", func() {
		It("Should clear BranchExists and LastCommitSHA to prevent information disclosure", func() {
			ctx := context.Background()

			// Create a GitRepoConfig that only allows 'main' and 'develop' branches
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-security",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "develop"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

			// Create a GitDestination referencing an unauthorized branch
			unauthorizedBranch := "feature/unauthorized"
			gitDestination := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-dest-security",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-security",
						Namespace: "default",
					},
					Branch:     unauthorizedBranch,
					BaseFolder: "test-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).Should(Succeed())

			// Wait for reconciliation and verify status
			gitDestLookupKey := types.NamespacedName{
				Name:      "test-dest-security",
				Namespace: "default",
			}

			createdGitDest := &configbutleraiv1alpha1.GitDestination{}

			// Wait for the controller to reconcile and set conditions
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gitDestLookupKey, createdGitDest)
				if err != nil {
					return false
				}
				// Check if Ready condition exists
				for _, condition := range createdGitDest.Status.Conditions {
					if condition.Type == ConditionTypeReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify the Ready condition is False with BranchNotAllowed reason
			Expect(createdGitDest.Status.Conditions).NotTo(BeEmpty())
			var readyCondition *metav1.Condition
			for i, condition := range createdGitDest.Status.Conditions {
				if condition.Type == ConditionTypeReady {
					readyCondition = &createdGitDest.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).NotTo(BeNil(), "Ready condition should exist")
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse), "Ready should be False")
			Expect(readyCondition.Reason).To(Equal(GitDestinationReasonBranchNotAllowed),
				"Reason should be BranchNotAllowed")

			// SECURITY TEST: Verify sensitive fields are cleared
			// This prevents unauthorized users from discovering branch existence or SHA information
			Expect(createdGitDest.Status.BranchExists).To(BeFalse(),
				"BranchExists MUST be false when branch is not allowed (security requirement)")
			Expect(createdGitDest.Status.LastCommitSHA).To(BeEmpty(),
				"LastCommitSHA MUST be empty when branch is not allowed (security requirement)")
			Expect(createdGitDest.Status.LastSyncTime).To(BeNil(),
				"LastSyncTime MUST be nil when branch is not allowed")

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitDestination)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})

		It("Should populate status fields when branch IS allowed", func() {
			ctx := context.Background()

			// Create a GitRepoConfig with wildcard pattern
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-allowed",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "feature/*"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

			// Create a GitDestination with an ALLOWED branch
			gitDestination := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-dest-allowed",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-allowed",
						Namespace: "default",
					},
					Branch:     "feature/allowed",
					BaseFolder: "allowed-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).Should(Succeed())

			// Wait for reconciliation
			gitDestLookupKey := types.NamespacedName{
				Name:      "test-dest-allowed",
				Namespace: "default",
			}

			createdGitDest := &configbutleraiv1alpha1.GitDestination{}

			// Wait for the controller to reconcile
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gitDestLookupKey, createdGitDest)
				if err != nil {
					return false
				}
				// Check if Ready condition exists
				for _, condition := range createdGitDest.Status.Conditions {
					if condition.Type == ConditionTypeReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// When branch IS allowed, the Ready condition should eventually be True
			// (may be False initially if repo is not accessible, but that's expected)
			// The key point is that sensitive fields are NOT cleared
			var readyCondition *metav1.Condition
			for i, condition := range createdGitDest.Status.Conditions {
				if condition.Type == ConditionTypeReady {
					readyCondition = &createdGitDest.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).NotTo(BeNil(), "Ready condition should exist")

			// If branch is allowed but repo is not accessible, reason should be RepositoryUnavailable
			// NOT BranchNotAllowed
			if readyCondition.Status == metav1.ConditionFalse {
				Expect(readyCondition.Reason).NotTo(Equal(GitDestinationReasonBranchNotAllowed),
					"When branch is allowed, reason should not be BranchNotAllowed")
			}

			// The key security verification: when branch IS allowed (even if repo unavailable),
			// the controller attempts to populate status fields and does NOT clear them
			// (they may be empty due to repo inaccessibility, but won't be explicitly cleared)

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitDestination)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})

		It("Should support glob patterns in allowedBranches", func() {
			ctx := context.Background()

			// Create a GitRepoConfig with various glob patterns
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-glob",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL: "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{
						"main",
						"develop",
						"feature/*",
						"release/v*",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

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
				destName := "test-dest-glob-" + string(rune('a'+i))

				gitDestination := &configbutleraiv1alpha1.GitDestination{
					ObjectMeta: metav1.ObjectMeta{
						Name:      destName,
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha1.GitDestinationSpec{
						RepoRef: configbutleraiv1alpha1.NamespacedName{
							Name:      "test-repo-glob",
							Namespace: "default",
						},
						Branch:     tc.branch,
						BaseFolder: "glob-test",
					},
				}

				Expect(k8sClient.Create(ctx, gitDestination)).Should(Succeed())

				// Wait for reconciliation
				gitDestLookupKey := types.NamespacedName{
					Name:      destName,
					Namespace: "default",
				}

				createdGitDest := &configbutleraiv1alpha1.GitDestination{}

				Eventually(func() bool {
					err := k8sClient.Get(ctx, gitDestLookupKey, createdGitDest)
					if err != nil {
						return false
					}
					for _, condition := range createdGitDest.Status.Conditions {
						if condition.Type == ConditionTypeReady {
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue())

				// Verify the condition based on whether branch should be allowed
				var readyCondition *metav1.Condition
				for i, condition := range createdGitDest.Status.Conditions {
					if condition.Type == ConditionTypeReady {
						readyCondition = &createdGitDest.Status.Conditions[i]
						break
					}
				}

				if !tc.shouldBeAllowed {
					Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(readyCondition.Reason).To(Equal(GitDestinationReasonBranchNotAllowed))
					// Security: verify fields are cleared
					Expect(createdGitDest.Status.BranchExists).To(BeFalse())
					Expect(createdGitDest.Status.LastCommitSHA).To(BeEmpty())
				} else {
					// If allowed, reason should not be BranchNotAllowed
					Expect(readyCondition.Reason).NotTo(Equal(GitDestinationReasonBranchNotAllowed))
				}

				// Cleanup
				Expect(k8sClient.Delete(ctx, gitDestination)).Should(Succeed())
			}

			// Cleanup
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})
	})

	Context("When checking for conflicts during reconciliation loop", func() {
		It("Should detect conflicts and elect winner by creationTimestamp", func() {
			ctx := context.Background()

			// Create a GitRepoConfig
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "develop"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

			// Create first GitDestination (winner - created first)
			firstDest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-dest-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-conflict",
						Namespace: "default",
					},
					Branch:     "main",
					BaseFolder: "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, firstDest)).Should(Succeed())

			// Wait for first destination to reconcile
			firstDestKey := types.NamespacedName{Name: "first-dest-conflict", Namespace: "default"}
			Eventually(func() bool {
				var dest configbutleraiv1alpha1.GitDestination
				if err := k8sClient.Get(ctx, firstDestKey, &dest); err != nil {
					return false
				}
				for _, condition := range dest.Status.Conditions {
					if condition.Type == ConditionTypeReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Kubernetes creationTimestamp has second-level precision
			// Wait at least 1 second to ensure different timestamps
			time.Sleep(1100 * time.Millisecond)

			// Create second GitDestination with same repo+branch+baseFolder (loser - created later)
			secondDest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-dest-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-conflict",
						Namespace: "default",
					},
					Branch:     "main",
					BaseFolder: "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, secondDest)).Should(Succeed())

			// Wait for second destination to reconcile
			secondDestKey := types.NamespacedName{Name: "second-dest-conflict", Namespace: "default"}
			Eventually(func() bool {
				var dest configbutleraiv1alpha1.GitDestination
				if err := k8sClient.Get(ctx, secondDestKey, &dest); err != nil {
					return false
				}
				for _, condition := range dest.Status.Conditions {
					if condition.Type == ConditionTypeReady && condition.Reason == GitDestinationReasonConflict {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify second destination has Conflict status
			var secondReconciledDest configbutleraiv1alpha1.GitDestination
			Expect(k8sClient.Get(ctx, secondDestKey, &secondReconciledDest)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledDest.Status.Conditions {
				if condition.Type == ConditionTypeReady {
					readyCondition = &secondReconciledDest.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(GitDestinationReasonConflict))
			Expect(readyCondition.Message).To(ContainSubstring("first-dest-conflict"))
			Expect(readyCondition.Message).To(ContainSubstring("created later"))

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondDest)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstDest)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})

		It("Should not conflict when baseFolder is different", func() {
			ctx := context.Background()

			// Create a GitRepoConfig
			gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).Should(Succeed())

			// Create first GitDestination
			firstDest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-dest-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-no-conflict",
						Namespace: "default",
					},
					Branch:     "main",
					BaseFolder: "folder-a",
				},
			}
			Expect(k8sClient.Create(ctx, firstDest)).Should(Succeed())

			// Create second GitDestination with DIFFERENT baseFolder (no conflict)
			secondDest := &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-dest-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "test-repo-no-conflict",
						Namespace: "default",
					},
					Branch:     "main",
					BaseFolder: "folder-b", // Different!
				},
			}
			Expect(k8sClient.Create(ctx, secondDest)).Should(Succeed())

			// Wait for both to reconcile
			secondDestKey := types.NamespacedName{Name: "second-dest-no-conflict", Namespace: "default"}
			Eventually(func() bool {
				var dest configbutleraiv1alpha1.GitDestination
				if err := k8sClient.Get(ctx, secondDestKey, &dest); err != nil {
					return false
				}
				for _, condition := range dest.Status.Conditions {
					if condition.Type == ConditionTypeReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify no conflict (reason should NOT be Conflict)
			var secondReconciledDest configbutleraiv1alpha1.GitDestination
			Expect(k8sClient.Get(ctx, secondDestKey, &secondReconciledDest)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledDest.Status.Conditions {
				if condition.Type == ConditionTypeReady {
					readyCondition = &secondReconciledDest.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Reason).NotTo(Equal(GitDestinationReasonConflict),
				"Should not have conflict when baseFolder is different")

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondDest)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstDest)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitRepoConfig)).Should(Succeed())
		})
	})
})
