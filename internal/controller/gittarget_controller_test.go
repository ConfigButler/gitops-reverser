// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"time"

	"filippo.io/age"
	meta "github.com/fluxcd/pkg/apis/meta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

var _ = Describe("GitTarget Controller Security", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When the referenced GitBranch is missing", func() {
		It("Should report a missing GitBranch through status instead of admission rejection", func() {
			ctx := context.Background()

			gitTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-missing-branch",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{Name: "missing-branch"},
					Path:         "test-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())

			key := types.NamespacedName{Name: "test-target-missing-branch", Namespace: "default"}
			Eventually(func(g Gomega) {
				var got configbutleraiv1alpha3.GitTarget
				g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				validated := findCondition(got.Status.Conditions, GitTargetConditionValidated)
				g.Expect(validated).NotTo(BeNil())
				g.Expect(validated.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(validated.Reason).To(Equal(GitTargetReasonGitBranchNotFound))
				g.Expect(validated.Message).
					To(ContainSubstring("Referenced GitBranch 'default/missing-branch' not found"))
				ready := findCondition(got.Status.Conditions, GitTargetConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(ready.Reason).To(Equal(GitTargetReadyReasonValidationFailed))
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, gitTarget)).Should(Succeed())
		})

		It("Should wait for a GitBranch that does not hold its branch, without stalling", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-two-axis",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
					SecretRef: &meta.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			gitTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-two-axis",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-two-axis", "main"),
					},
					Path: "two-axis-folder",
				},
			}
			Expect(k8sClient.Create(ctx, gitTarget)).Should(Succeed())

			key := types.NamespacedName{Name: "test-target-two-axis", Namespace: "default"}
			// No GitBranch reconciler runs in this suite, so the GitBranch never proves itself and holds
			// nothing. That clears on its own, so the target is progressing, not stalled, and the
			// retired EventStreamLive condition stays absent.
			Eventually(func(g Gomega) {
				var got configbutleraiv1alpha3.GitTarget
				g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				g.Expect(conditionIsTrue(got.Status.Conditions, GitTargetConditionReady)).To(BeFalse())
				g.Expect(conditionIsTrue(got.Status.Conditions, GitTargetConditionReconciling)).To(BeTrue())
				g.Expect(conditionIsTrue(got.Status.Conditions, GitTargetConditionStalled)).To(BeFalse())
				g.Expect(conditionIsTrue(got.Status.Conditions, GitTargetConditionStreamsRunning)).To(BeFalse())
				branchReady := findCondition(got.Status.Conditions, GitTargetConditionGitBranchReady)
				g.Expect(branchReady).NotTo(BeNil())
				g.Expect(branchReady.Reason).To(Equal(GitTargetReasonGitBranchNotReady))
				for _, c := range got.Status.Conditions {
					g.Expect(c.Type).NotTo(Equal("EventStreamLive"), "EventStreamLive condition was removed")
				}
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, gitTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})

	Context("When checking for conflicts during reconciliation loop", func() {
		It("Should detect conflicts and elect winner by creationTimestamp", func() {
			ctx := context.Background()

			// Create a GitProvider
			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main", "develop"},
					SecretRef: &meta.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			// Create first GitTarget (winner - created first)
			firstTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-target-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-conflict", "main"),
					},
					Path: "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, firstTarget)).Should(Succeed())

			// Wait for first target to reconcile
			firstTargetKey := types.NamespacedName{Name: "first-target-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, firstTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetConditionReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Kubernetes creationTimestamp has second-level precision
			// Wait at least 1 second to ensure different timestamps
			time.Sleep(1100 * time.Millisecond)

			// Create second GitTarget with same provider+branch+path (loser - created later)
			secondTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-target-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-conflict", "main"),
					},
					Path: "conflict-folder",
				},
			}
			Expect(k8sClient.Create(ctx, secondTarget)).Should(Succeed())

			// Wait for second target to reconcile
			secondTargetKey := types.NamespacedName{Name: "second-target-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, secondTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetConditionValidated &&
						condition.Reason == GitTargetReasonTargetConflict {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify second target has Conflict status
			var secondReconciledTarget configbutleraiv1alpha3.GitTarget
			Expect(k8sClient.Get(ctx, secondTargetKey, &secondReconciledTarget)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetConditionReady {
					readyCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(GitTargetReadyReasonValidationFailed))
			var validatedCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetConditionValidated {
					validatedCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}
			Expect(validatedCondition).NotTo(BeNil())
			Expect(validatedCondition.Reason).To(Equal(GitTargetReasonTargetConflict))
			Expect(validatedCondition.Message).To(ContainSubstring("first-target-conflict"))
			Expect(validatedCondition.Message).To(ContainSubstring("created later"))

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should not conflict when path is different", func() {
			ctx := context.Background()

			// Create a GitProvider
			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
					SecretRef: &meta.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			// Create first GitTarget
			firstTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-target-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-no-conflict", "main"),
					},
					Path: "folder-a",
				},
			}
			Expect(k8sClient.Create(ctx, firstTarget)).Should(Succeed())

			// Create second GitTarget with DIFFERENT path (no conflict)
			secondTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-target-no-conflict",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-no-conflict", "main"),
					},
					Path: "folder-b", // Different!
				},
			}
			Expect(k8sClient.Create(ctx, secondTarget)).Should(Succeed())

			// Wait for both to reconcile
			secondTargetKey := types.NamespacedName{Name: "second-target-no-conflict", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, secondTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetConditionReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify no conflict (reason should NOT be Conflict)
			var secondReconciledTarget configbutleraiv1alpha3.GitTarget
			Expect(k8sClient.Get(ctx, secondTargetKey, &secondReconciledTarget)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetConditionReady {
					readyCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}

			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Reason).NotTo(Equal(GitTargetReadyReasonValidationFailed),
				"Should not have conflict when path is different")

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should detect conflicts when one path nests inside another", func() {
			ctx := context.Background()

			// Create a GitProvider
			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-nested",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
					SecretRef: &meta.LocalObjectReference{
						Name: "test-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			// First GitTarget owns the parent folder "team" (winner - created first)
			firstTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "first-target-nested",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-nested", "main"),
					},
					Path: "team",
				},
			}
			Expect(k8sClient.Create(ctx, firstTarget)).Should(Succeed())

			firstTargetKey := types.NamespacedName{Name: "first-target-nested", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, firstTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetConditionReady {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Kubernetes creationTimestamp has second-level precision; ensure the
			// nested target is created strictly later so it is the loser.
			time.Sleep(1100 * time.Millisecond)

			// Second GitTarget nests under the first ("team/app") - must conflict.
			secondTarget := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "second-target-nested",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-nested", "main"),
					},
					Path: "team/app",
				},
			}
			Expect(k8sClient.Create(ctx, secondTarget)).Should(Succeed())

			secondTargetKey := types.NamespacedName{Name: "second-target-nested", Namespace: "default"}
			Eventually(func() bool {
				var target configbutleraiv1alpha3.GitTarget
				if err := k8sClient.Get(ctx, secondTargetKey, &target); err != nil {
					return false
				}
				for _, condition := range target.Status.Conditions {
					if condition.Type == GitTargetConditionValidated &&
						condition.Reason == GitTargetReasonTargetConflict {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			var secondReconciledTarget configbutleraiv1alpha3.GitTarget
			Expect(k8sClient.Get(ctx, secondTargetKey, &secondReconciledTarget)).Should(Succeed())

			var readyCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetConditionReady {
					readyCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(GitTargetReadyReasonValidationFailed))

			var validatedCondition *metav1.Condition
			for i, condition := range secondReconciledTarget.Status.Conditions {
				if condition.Type == GitTargetConditionValidated {
					validatedCondition = &secondReconciledTarget.Status.Conditions[i]
					break
				}
			}
			Expect(validatedCondition).NotTo(BeNil())
			Expect(validatedCondition.Reason).To(Equal(GitTargetReasonTargetConflict))
			Expect(validatedCondition.Message).To(ContainSubstring("overlaps"))
			Expect(validatedCondition.Message).To(ContainSubstring("first-target-nested"))
			Expect(validatedCondition.Message).To(ContainSubstring("created later"))

			// Cleanup
			Expect(k8sClient.Delete(ctx, secondTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, firstTarget)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})

	Context("When encryption secret auto-generation is configured", func() {
		It("Should create missing encryption secret with recipient annotation and warning annotation", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-generate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-generate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-generate-enc-secret", "main"),
					},
					Path: "test-path",
					Encryption: &configbutleraiv1alpha3.EncryptionSpec{
						Provider: "sops",
						SecretRef: meta.LocalObjectReference{
							Name: "generated-sops-age-key",
						},
						Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
							Enabled: true,
							Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
								ExtractFromSecret:   true,
								GenerateWhenMissing: true,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			secretKey := types.NamespacedName{Name: "generated-sops-age-key", Namespace: "default"}
			Eventually(func(g Gomega) {
				var secret corev1.Secret
				err := k8sClient.Get(ctx, secretKey, &secret)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
				ageKeyName, ageKeyValue := findFirstAgeKeyEntry(secret.Data)
				g.Expect(ageKeyName).NotTo(BeEmpty())
				g.Expect(ageKeyName).To(Equal(currentDateAgeKeySecretDataKey()))
				g.Expect(string(ageKeyValue)).To(ContainSubstring("AGE-SECRET-KEY-"))
				g.Expect(secret.Annotations).To(HaveKey(encryptionSecretRecipientAnnoKey))
				g.Expect(secret.Annotations[encryptionSecretRecipientAnnoKey]).To(HavePrefix("age1"))
				g.Expect(secret.Annotations).To(HaveKeyWithValue(
					encryptionSecretBackupWarningAnno,
					encryptionSecretBackupWarningValue,
				))
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should report invalid encryption config when secret is missing and generation is disabled", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-no-generate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-no-generate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-no-generate-enc-secret", "main"),
					},
					Path: "test-path",
					Encryption: &configbutleraiv1alpha3.EncryptionSpec{
						Provider: "sops",
						SecretRef: meta.LocalObjectReference{
							Name: "missing-sops-age-key",
						},
						Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
							Enabled: true,
							Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
								ExtractFromSecret:   true,
								GenerateWhenMissing: false,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			targetKey := types.NamespacedName{Name: "test-target-no-generate-enc-secret", Namespace: "default"}
			Eventually(func(g Gomega) {
				var got configbutleraiv1alpha3.GitTarget
				err := k8sClient.Get(ctx, targetKey, &got)
				g.Expect(err).NotTo(HaveOccurred())

				var readyCondition *metav1.Condition
				for i, condition := range got.Status.Conditions {
					if condition.Type == GitTargetConditionReady {
						readyCondition = &got.Status.Conditions[i]
						break
					}
				}
				g.Expect(readyCondition).NotTo(BeNil())
				g.Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(readyCondition.Reason).To(Equal(GitTargetReadyReasonEncryptionNotConfigured))
				var encryptionCondition *metav1.Condition
				for i, condition := range got.Status.Conditions {
					if condition.Type == GitTargetConditionEncryptionConfigured {
						encryptionCondition = &got.Status.Conditions[i]
						break
					}
				}
				g.Expect(encryptionCondition).NotTo(BeNil())
				g.Expect(encryptionCondition.Reason).To(Equal(GitTargetReasonMissingSecret))
				g.Expect(encryptionCondition.Message).To(ContainSubstring("failed to fetch encryption secret"))
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It(
			"Should report invalid encryption config when generateWhenMissing is enabled without secret extraction",
			func() {
				ctx := context.Background()

				gitProvider := &configbutleraiv1alpha3.GitProvider{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-provider-no-enc-recipients",
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha3.GitProviderSpec{
						URL:             "https://github.com/test-org/test-repo.git",
						AllowedBranches: []string{"main"},
					},
				}
				Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
				createGitBranchFor(ctx, gitProvider, "main")

				target := &configbutleraiv1alpha3.GitTarget{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-target-no-enc-recipients",
						Namespace: "default",
					},
					Spec: configbutleraiv1alpha3.GitTargetSpec{
						GitBranchRef: &meta.NamespacedObjectReference{
							Name: gitBranchNameForTest("test-provider-no-enc-recipients", "main"),
						},
						Path: "test-path",
						Encryption: &configbutleraiv1alpha3.EncryptionSpec{
							Provider: "sops",
							SecretRef: meta.LocalObjectReference{
								Name: "unused-encryption-secret",
							},
							Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
								Enabled: true,
								Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
									ExtractFromSecret:   false,
									GenerateWhenMissing: true,
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, target)).Should(Succeed())

				targetKey := types.NamespacedName{Name: "test-target-no-enc-recipients", Namespace: "default"}
				Eventually(func(g Gomega) {
					var got configbutleraiv1alpha3.GitTarget
					err := k8sClient.Get(ctx, targetKey, &got)
					g.Expect(err).NotTo(HaveOccurred())

					var validatedCondition *metav1.Condition
					var readyCondition *metav1.Condition
					var encryptionCondition *metav1.Condition
					for i, condition := range got.Status.Conditions {
						switch condition.Type {
						case GitTargetConditionValidated:
							validatedCondition = &got.Status.Conditions[i]
						case GitTargetConditionReady:
							readyCondition = &got.Status.Conditions[i]
						case GitTargetConditionEncryptionConfigured:
							encryptionCondition = &got.Status.Conditions[i]
						}
					}

					g.Expect(validatedCondition).NotTo(BeNil())
					g.Expect(validatedCondition.Status).To(Equal(metav1.ConditionTrue))
					g.Expect(readyCondition).NotTo(BeNil())
					g.Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
					g.Expect(readyCondition.Reason).To(Equal(GitTargetReadyReasonEncryptionNotConfigured))
					g.Expect(encryptionCondition).NotTo(BeNil())
					g.Expect(encryptionCondition.Status).To(Equal(metav1.ConditionFalse))
					g.Expect(encryptionCondition.Reason).To(Equal(GitTargetReasonInvalidConfig))
					g.Expect(encryptionCondition.Message).To(ContainSubstring(
						"generateWhenMissing=true requires extractFromSecret=true",
					))
				}, timeout, interval).Should(Succeed())

				Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
				Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
			},
		)

		It("Should add one .agekey entry when secret exists without any .agekey entries", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-update-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			seedSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-sops-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"OTHER_ENV": []byte("value"),
				},
			}
			Expect(k8sClient.Create(ctx, seedSecret)).Should(Succeed())

			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-update-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-update-enc-secret", "main"),
					},
					Path: "test-path",
					Encryption: &configbutleraiv1alpha3.EncryptionSpec{
						Provider: "sops",
						SecretRef: meta.LocalObjectReference{
							Name: "existing-sops-secret",
						},
						Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
							Enabled: true,
							Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
								ExtractFromSecret:   true,
								GenerateWhenMissing: true,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			secretKey := types.NamespacedName{Name: "existing-sops-secret", Namespace: "default"}
			Eventually(func(g Gomega) {
				var secret corev1.Secret
				err := k8sClient.Get(ctx, secretKey, &secret)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Data).To(HaveKey("OTHER_ENV"))
				ageKeyName, ageKeyValue := findFirstAgeKeyEntry(secret.Data)
				g.Expect(ageKeyName).NotTo(BeEmpty())
				g.Expect(ageKeyName).To(Equal(currentDateAgeKeySecretDataKey()))
				g.Expect(string(ageKeyValue)).To(ContainSubstring("AGE-SECRET-KEY-"))
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, seedSecret)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should recreate encryption secret when it is deleted while GitTarget still exists", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-recreate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-recreate-enc-secret",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-recreate-enc-secret", "main"),
					},
					Path: "test-path",
					Encryption: &configbutleraiv1alpha3.EncryptionSpec{
						Provider: "sops",
						SecretRef: meta.LocalObjectReference{
							Name: "recreated-sops-age-key",
						},
						Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
							Enabled: true,
							Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
								ExtractFromSecret:   true,
								GenerateWhenMissing: true,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			secretKey := types.NamespacedName{Name: "recreated-sops-age-key", Namespace: "default"}

			// Wait for initial secret creation.
			var firstSecret corev1.Secret
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, secretKey, &firstSecret)
				g.Expect(err).NotTo(HaveOccurred())
				ageKeyName, _ := findFirstAgeKeyEntry(firstSecret.Data)
				g.Expect(ageKeyName).NotTo(BeEmpty())
			}, timeout, interval).Should(Succeed())

			// Delete the secret to simulate accidental removal.
			Expect(k8sClient.Delete(ctx, &firstSecret)).Should(Succeed())

			// Recreation rides the PERIODIC requeue, not a watch: this controller deliberately runs
			// no control-plane Secret watch (SetupWithManager explains why), so nothing enqueues the
			// GitTarget when its age-key Secret disappears. The wait must therefore exceed a full
			// RequeueStreamSettleInterval, which the shared 10s `timeout` alone does not.
			//
			// The budget is deliberately a BOUND rather than a generous number: it is this spec's
			// implicit SLO — recreation must happen within about four ticks of the deletion — so a
			// regression that made the requeue path slow rather than broken still fails here.
			Eventually(func(g Gomega) {
				var recreated corev1.Secret
				err := k8sClient.Get(ctx, secretKey, &recreated)
				g.Expect(err).NotTo(HaveOccurred())
				ageKeyName, ageKeyValue := findFirstAgeKeyEntry(recreated.Data)
				g.Expect(ageKeyName).NotTo(BeEmpty())
				g.Expect(string(ageKeyValue)).To(ContainSubstring("AGE-SECRET-KEY-"))
				g.Expect(recreated.Annotations).To(HaveKey(encryptionSecretRecipientAnnoKey))
			}, 7*RequeueStreamSettleInterval/2+timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})

		It("Should not overwrite existing .agekey values when one already exists", func() {
			ctx := context.Background()

			gitProvider := &configbutleraiv1alpha3.GitProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-provider-existing-agekey",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitProviderSpec{
					URL:             "https://github.com/test-org/test-repo.git",
					AllowedBranches: []string{"main"},
				},
			}
			Expect(k8sClient.Create(ctx, gitProvider)).Should(Succeed())
			createGitBranchFor(ctx, gitProvider, "main")

			existingIdentity, err := age.GenerateX25519Identity()
			Expect(err).NotTo(HaveOccurred())
			existingAgeKey := existingIdentity.String()
			seedSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-agekey-secret",
					Namespace: "default",
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"20260221.agekey": []byte(existingAgeKey),
					"OTHER_ENV":       []byte("value"),
				},
			}
			Expect(k8sClient.Create(ctx, seedSecret)).Should(Succeed())

			target := &configbutleraiv1alpha3.GitTarget{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-target-existing-agekey",
					Namespace: "default",
				},
				Spec: configbutleraiv1alpha3.GitTargetSpec{
					GitBranchRef: &meta.NamespacedObjectReference{
						Name: gitBranchNameForTest("test-provider-existing-agekey", "main"),
					},
					Path: "test-path",
					Encryption: &configbutleraiv1alpha3.EncryptionSpec{
						Provider: "sops",
						SecretRef: meta.LocalObjectReference{
							Name: "existing-agekey-secret",
						},
						Age: &configbutleraiv1alpha3.AgeEncryptionSpec{
							Enabled: true,
							Recipients: configbutleraiv1alpha3.AgeRecipientsSpec{
								ExtractFromSecret:   true,
								GenerateWhenMissing: true,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, target)).Should(Succeed())

			secretKey := types.NamespacedName{Name: "existing-agekey-secret", Namespace: "default"}
			Eventually(func(g Gomega) {
				var secret corev1.Secret
				err := k8sClient.Get(ctx, secretKey, &secret)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Data).To(HaveKey("OTHER_ENV"))
				g.Expect(secret.Data).To(HaveKey("20260221.agekey"))
				g.Expect(string(secret.Data["20260221.agekey"])).To(Equal(existingAgeKey))
				g.Expect(countAgeKeyEntries(secret.Data)).To(Equal(1))
			}, timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, target)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, seedSecret)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, gitProvider)).Should(Succeed())
		})
	})
})

// createGitBranchFor creates the GitBranch "<provider>-<branch>" writing branch through provider, in
// the provider's namespace, and deletes it when the spec ends.
func createGitBranchFor(ctx context.Context, provider *configbutleraiv1alpha3.GitProvider, branch string) {
	gitBranch := &configbutleraiv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{
			Name: gitBranchNameForTest(provider.Name, branch), Namespace: provider.Namespace,
		},
		Spec: configbutleraiv1alpha3.GitBranchSpec{
			GitProviderRef: meta.LocalObjectReference{Name: provider.Name}, Branch: branch,
		},
	}
	Expect(k8sClient.Create(ctx, gitBranch)).Should(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), gitBranch) })
}

// holdGitBranchForTest creates the GitBranch "<provider>-<branch>" and makes it hold its repository
// branch, as its reconcile would once its credential is proved. No GitBranch reconciler runs in
// this suite.
func holdGitBranchForTest(ctx context.Context, provider *configbutleraiv1alpha3.GitProvider, branch string) {
	createGitBranchFor(ctx, provider, branch)
	stored := &configbutleraiv1alpha3.GitBranch{}
	key := types.NamespacedName{Namespace: provider.Namespace, Name: gitBranchNameForTest(provider.Name, branch)}
	Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
	testWorkerManager.Claims().Observe(git.BranchKeyFor(provider.Spec.URL, branch), git.BranchClaimant{
		GitBranch: key, UID: stored.UID, Created: stored.CreationTimestamp.Time,
	}, true)
	DeferCleanup(func() { testWorkerManager.Claims().Release(stored.UID) })
}

func findFirstAgeKeyEntry(data map[string][]byte) (string, []byte) {
	for key, value := range data {
		if strings.HasSuffix(key, ".agekey") {
			return key, value
		}
	}
	return "", nil
}

func countAgeKeyEntries(data map[string][]byte) int {
	count := 0
	for key := range data {
		if strings.HasSuffix(key, ".agekey") {
			count++
		}
	}
	return count
}
