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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("GitDestination Controller", func() {
	Context("Status Condition Management", func() {
		var reconciler *GitDestinationReconciler
		var gitDestination *configbutleraiv1alpha1.GitDestination

		BeforeEach(func() {
			reconciler = &GitDestinationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "test-repo"},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
				Status: configbutleraiv1alpha1.GitDestinationStatus{
					Conditions: []metav1.Condition{},
				},
			}
		})

		It("should set initial validating condition", func() {
			reconciler.setCondition(gitDestination, metav1.ConditionUnknown,
				GitDestinationReasonValidating, "Validating...")

			Expect(gitDestination.Status.Conditions).To(HaveLen(1))
			condition := gitDestination.Status.Conditions[0]
			Expect(condition.Type).To(Equal(ConditionTypeReady))
			Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
			Expect(condition.Reason).To(Equal(GitDestinationReasonValidating))
			Expect(condition.Message).To(Equal("Validating..."))
		})

		It("should update existing condition", func() {
			// Set initial condition
			reconciler.setCondition(gitDestination, metav1.ConditionUnknown,
				GitDestinationReasonValidating, "Validating...")

			// Update condition
			reconciler.setCondition(gitDestination, metav1.ConditionTrue,
				GitDestinationReasonReady, "Ready!")

			Expect(gitDestination.Status.Conditions).To(HaveLen(1))
			condition := gitDestination.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(GitDestinationReasonReady))
			Expect(condition.Message).To(Equal("Ready!"))
		})

		It("should set different failure conditions", func() {
			testCases := []struct {
				reason  string
				message string
			}{
				{GitDestinationReasonGitRepoConfigNotFound, "GitRepoConfig not found"},
				{GitDestinationReasonBranchNotAllowed, "Branch not allowed"},
			}

			for _, tc := range testCases {
				reconciler.setCondition(gitDestination, metav1.ConditionFalse, tc.reason, tc.message)

				Expect(gitDestination.Status.Conditions).To(HaveLen(1))
				condition := gitDestination.Status.Conditions[0]
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(tc.reason))
				Expect(condition.Message).To(Equal(tc.message))
			}
		})
	})

	Context("Full Controller Integration", func() {
		var (
			ctx            context.Context
			reconciler     *GitDestinationReconciler
			gitRepoConfig  *configbutleraiv1alpha1.GitRepoConfig
			gitDestination *configbutleraiv1alpha1.GitDestination
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = &GitDestinationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		AfterEach(func() {
			// Cleanup
			if gitDestination != nil {
				_ = k8sClient.Delete(ctx, gitDestination)
			}
			if gitRepoConfig != nil {
				_ = k8sClient.Delete(ctx, gitRepoConfig)
			}
		})

		It("should fail when GitRepoConfig is not found", func() {
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "nonexistent-repo"},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(RequeueShortInterval))

			// Verify the resource was updated with failure condition
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedDest.Status.Conditions).To(HaveLen(1))
			condition := updatedDest.Status.Conditions[0]
			Expect(condition.Type).To(Equal(ConditionTypeReady))
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(GitDestinationReasonGitRepoConfigNotFound))
			Expect(condition.Message).To(ContainSubstring("Referenced GitRepoConfig"))
			Expect(condition.Message).To(ContainSubstring("not found"))
		})

		It("should fail when branch is not in allowedBranches list", func() {
			// Create GitRepoConfig with specific allowed branches
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main", "develop"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			// Create GitDestination with branch not in allowedBranches
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "test-repo"},
					Branch:     "production",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(RequeueShortInterval))

			// Verify the resource was updated with failure condition
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedDest.Status.Conditions).To(HaveLen(1))
			condition := updatedDest.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(GitDestinationReasonBranchNotAllowed))
			Expect(condition.Message).To(ContainSubstring("Branch 'production' is not in allowedBranches"))
		})

		It("should succeed when GitRepoConfig exists and branch is allowed", func() {
			// Create GitRepoConfig
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main", "develop"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			// Create GitDestination with valid branch
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "test-repo"},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(RequeueLongInterval))

			// Verify the resource was updated with success condition
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedDest.Status.Conditions).To(HaveLen(1))
			condition := updatedDest.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(GitDestinationReasonReady))
			Expect(condition.Message).To(ContainSubstring("GitDestination is ready"))
			Expect(updatedDest.Status.ObservedGeneration).To(Equal(updatedDest.Generation))
		})

		It("should handle cross-namespace GitRepoConfig reference", func() {
			// Create GitRepoConfig in a different namespace
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shared-repo",
					Namespace: "kube-system",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			// Create GitDestination referencing cross-namespace GitRepoConfig
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name:      "shared-repo",
						Namespace: "kube-system",
					},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(RequeueLongInterval))

			// Verify successful reconciliation
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedDest.Status.Conditions).To(HaveLen(1))
			condition := updatedDest.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(GitDestinationReasonReady))
		})

		It("should default to GitDestination namespace when RepoRef namespace is empty", func() {
			// Create GitRepoConfig in same namespace as GitDestination
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-repo",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			// Create GitDestination without namespace in RepoRef (should default to "default")
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef: configbutleraiv1alpha1.NamespacedName{
						Name: "local-repo",
						// Namespace intentionally left empty
					},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(RequeueLongInterval))

			// Verify successful reconciliation
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedDest.Status.Conditions).To(HaveLen(1))
			condition := updatedDest.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should handle resource deletion gracefully", func() {
			// Test reconciling a non-existent resource
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent-destination",
					Namespace: "default",
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("should update ObservedGeneration on successful reconciliation", func() {
			// Create GitRepoConfig
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-repo",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "https://github.com/test/repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			// Create GitDestination
			gitDestination = &configbutleraiv1alpha1.GitDestination{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-destination",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha1.GitDestinationSpec{
					RepoRef:    configbutleraiv1alpha1.NamespacedName{Name: "test-repo"},
					Branch:     "main",
					BaseFolder: "clusters/default",
				},
			}
			Expect(k8sClient.Create(ctx, gitDestination)).To(Succeed())

			// First reconciliation
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Get updated resource
			updatedDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				updatedDest)
			Expect(err).NotTo(HaveOccurred())

			// ObservedGeneration should match Generation
			Expect(updatedDest.Status.ObservedGeneration).To(Equal(updatedDest.Generation))

			// Update spec to trigger new generation
			updatedDest.Spec.BaseFolder = "clusters/production"
			Expect(k8sClient.Update(ctx, updatedDest)).To(Succeed())

			// Second reconciliation
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gitDestination.Name,
					Namespace: gitDestination.Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Get updated resource again
			finalDest := &configbutleraiv1alpha1.GitDestination{}
			err = k8sClient.Get(ctx,
				types.NamespacedName{Name: gitDestination.Name, Namespace: gitDestination.Namespace},
				finalDest)
			Expect(err).NotTo(HaveOccurred())

			// ObservedGeneration should still match the new Generation
			Expect(finalDest.Status.ObservedGeneration).To(Equal(finalDest.Generation))
			Expect(finalDest.Generation).To(BeNumerically(">", 1))
		})
	})
})
