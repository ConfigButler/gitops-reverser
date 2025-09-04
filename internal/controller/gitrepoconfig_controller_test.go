/*
Copyright 2025.

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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

var _ = Describe("GitRepoConfig Controller", func() {
	Context("Credential Extraction", func() {
		var reconciler *GitRepoConfigReconciler

		BeforeEach(func() {
			reconciler = &GitRepoConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		Describe("SSH Key Authentication", func() {
			It("should extract SSH credentials without passphrase", func() {
				privateKey, err := generateTestSSHKey()
				Expect(err).NotTo(HaveOccurred())

				secret := &corev1.Secret{
					Data: map[string][]byte{
						"ssh-privatekey": privateKey,
					},
				}

				auth, err := reconciler.extractCredentials(secret)
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).To(BeAssignableToTypeOf(&ssh.PublicKeys{}))

				sshAuth := auth.(*ssh.PublicKeys)
				Expect(sshAuth.User).To(Equal("git"))
			})

			It("should extract SSH credentials with passphrase", func() {
				// For testing, use a simple unencrypted key with passphrase field
				privateKey, err := generateTestSSHKey()
				Expect(err).NotTo(HaveOccurred())

				secret := &corev1.Secret{
					Data: map[string][]byte{
						"ssh-privatekey": privateKey,
						"ssh-passphrase": []byte("testpass"),
					},
				}

				auth, err := reconciler.extractCredentials(secret)
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).To(BeAssignableToTypeOf(&ssh.PublicKeys{}))
			})

			It("should fail with invalid SSH key", func() {
				secret := &corev1.Secret{
					Data: map[string][]byte{
						"ssh-privatekey": []byte("invalid-key"),
					},
				}

				_, err := reconciler.extractCredentials(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to parse SSH private key"))
			})

			It("should handle empty passphrase gracefully", func() {
				privateKey, err := generateTestSSHKey()
				Expect(err).NotTo(HaveOccurred())

				secret := &corev1.Secret{
					Data: map[string][]byte{
						"ssh-privatekey": privateKey,
						"ssh-passphrase": []byte(""), // Empty passphrase
					},
				}

				auth, err := reconciler.extractCredentials(secret)
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).To(BeAssignableToTypeOf(&ssh.PublicKeys{}))
			})
		})

		Describe("Username/Password Authentication", func() {
			It("should extract username/password credentials", func() {
				secret := &corev1.Secret{
					Data: map[string][]byte{
						"username": []byte("testuser"),
						"password": []byte("testpass"),
					},
				}

				auth, err := reconciler.extractCredentials(secret)
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).To(BeAssignableToTypeOf(&http.BasicAuth{}))

				httpAuth := auth.(*http.BasicAuth)
				Expect(httpAuth.Username).To(Equal("testuser"))
				Expect(httpAuth.Password).To(Equal("testpass"))
			})

			It("should fail with username but no password", func() {
				secret := &corev1.Secret{
					Data: map[string][]byte{
						"username": []byte("testuser"),
					},
				}

				_, err := reconciler.extractCredentials(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("secret contains username but missing password"))
			})
		})

		Describe("Invalid Secrets", func() {
			It("should fail with empty secret", func() {
				secret := &corev1.Secret{
					Data: map[string][]byte{},
				}

				_, err := reconciler.extractCredentials(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("secret must contain either 'ssh-privatekey' or both 'username' and 'password'"))
			})

			It("should fail with irrelevant data", func() {
				secret := &corev1.Secret{
					Data: map[string][]byte{
						"random-key": []byte("random-value"),
					},
				}

				_, err := reconciler.extractCredentials(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("secret must contain either 'ssh-privatekey' or both 'username' and 'password'"))
			})
		})
	})

	Context("Status Condition Management", func() {
		var reconciler *GitRepoConfigReconciler
		var gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig

		BeforeEach(func() {
			reconciler = &GitRepoConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Status: configbutleraiv1alpha1.GitRepoConfigStatus{
					Conditions: []metav1.Condition{},
				},
			}
		})

		It("should set initial checking condition", func() {
			reconciler.setCondition(gitRepoConfig, metav1.ConditionUnknown, ReasonChecking, "Validating...")

			Expect(gitRepoConfig.Status.Conditions).To(HaveLen(1))
			condition := gitRepoConfig.Status.Conditions[0]
			Expect(condition.Type).To(Equal("Ready"))
			Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
			Expect(condition.Reason).To(Equal(ReasonChecking))
			Expect(condition.Message).To(Equal("Validating..."))
		})

		It("should update existing condition", func() {
			// Set initial condition
			reconciler.setCondition(gitRepoConfig, metav1.ConditionUnknown, ReasonChecking, "Checking...")

			// Update condition
			reconciler.setCondition(gitRepoConfig, metav1.ConditionTrue, ReasonBranchFound, "Success!")

			Expect(gitRepoConfig.Status.Conditions).To(HaveLen(1))
			condition := gitRepoConfig.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(ReasonBranchFound))
			Expect(condition.Message).To(Equal("Success!"))
		})

		It("should set different failure conditions", func() {
			testCases := []struct {
				reason  string
				message string
			}{
				{ReasonSecretNotFound, "Secret not found"},
				{ReasonSecretMalformed, "Secret malformed"},
				{ReasonConnectionFailed, "Connection failed"},
				{ReasonBranchNotFound, "Branch not found"},
			}

			for _, tc := range testCases {
				reconciler.setCondition(gitRepoConfig, metav1.ConditionFalse, tc.reason, tc.message)

				Expect(gitRepoConfig.Status.Conditions).To(HaveLen(1))
				condition := gitRepoConfig.Status.Conditions[0]
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal(tc.reason))
				Expect(condition.Message).To(Equal(tc.message))
			}
		})
	})

	Context("Full Controller Integration", func() {
		var (
			ctx           context.Context
			reconciler    *GitRepoConfigReconciler
			gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig
			testSecret    *corev1.Secret
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = &GitRepoConfigReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create a test secret with SSH key
			privateKey, err := generateTestSSHKey()
			Expect(err).NotTo(HaveOccurred())

			testSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "git-credentials",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"ssh-privatekey": privateKey,
				},
			}
			Expect(k8sClient.Create(ctx, testSecret)).To(Succeed())
		})

		AfterEach(func() {
			// Cleanup
			if gitRepoConfig != nil {
				_ = k8sClient.Delete(ctx, gitRepoConfig)
			}
			if testSecret != nil {
				_ = k8sClient.Delete(ctx, testSecret)
			}
		})

		It("should fail when secret is not found", func() {
			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-config",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "git@github.com:test/repo.git",
					Branch:          "main",
					SecretName:      "nonexistent-secret",
					SecretNamespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: gitRepoConfig.Name,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute * 5))

			// Verify the resource was updated with failure condition
			updatedConfig := &configbutleraiv1alpha1.GitRepoConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: gitRepoConfig.Name}, updatedConfig)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedConfig.Status.Conditions).To(HaveLen(1))
			condition := updatedConfig.Status.Conditions[0]
			Expect(condition.Type).To(Equal("Ready"))
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(ReasonSecretNotFound))
			Expect(condition.Message).To(ContainSubstring("Secret 'nonexistent-secret' not found"))
		})

		It("should fail when secret is malformed", func() {
			// Create malformed secret
			malformedSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "malformed-secret",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"invalid-key": []byte("invalid-data"),
				},
			}
			Expect(k8sClient.Create(ctx, malformedSecret)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, malformedSecret) }()

			gitRepoConfig = &configbutleraiv1alpha1.GitRepoConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-config",
				},
				Spec: configbutleraiv1alpha1.GitRepoConfigSpec{
					RepoURL:         "git@github.com:test/repo.git",
					Branch:          "main",
					SecretName:      "malformed-secret",
					SecretNamespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, gitRepoConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: gitRepoConfig.Name,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute * 5))

			// Verify the resource was updated with failure condition
			updatedConfig := &configbutleraiv1alpha1.GitRepoConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: gitRepoConfig.Name}, updatedConfig)
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedConfig.Status.Conditions).To(HaveLen(1))
			condition := updatedConfig.Status.Conditions[0]
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(ReasonSecretMalformed))
			Expect(condition.Message).To(ContainSubstring("Secret 'malformed-secret' malformed"))
		})

		It("should handle resource deletion gracefully", func() {
			// Test reconciling a non-existent resource
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: "nonexistent-config",
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})
	})
})

// Helper functions for generating test SSH keys
func generateTestSSHKey() ([]byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	return privateKeyPEM, nil
}
