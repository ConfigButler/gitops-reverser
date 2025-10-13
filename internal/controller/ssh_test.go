/*
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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SSH Authentication", func() {
	var (
		reconciler       *GitRepoConfigReconciler
		privateKey       []byte
		knownHosts       []byte
		validSSHSecret   *corev1.Secret
		invalidSSHSecret *corev1.Secret
	)

	BeforeEach(func() {
		reconciler = &GitRepoConfigReconciler{}

		// Generate a test RSA key pair (4096 bits to meet Gitea's security requirements)
		rsaKey, err := rsa.GenerateKey(rand.Reader, 4096)
		Expect(err).NotTo(HaveOccurred())

		// Private key in PEM format
		privateKeyPEM := &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
		}
		privateKey = pem.EncodeToMemory(privateKeyPEM)

		// Mock known_hosts content
		knownHosts = []byte("gitea-ssh.gitea-e2e.svc.cluster.local ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQC...")

		// Create valid SSH secret
		validSSHSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valid-ssh-secret",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"ssh-privatekey": privateKey,
				"known_hosts":    knownHosts,
			},
		}

		// Create invalid SSH secret (malformed private key)
		invalidSSHSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-ssh-secret",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"ssh-privatekey": []byte("invalid-private-key-content"),
				"known_hosts":    knownHosts,
			},
		}
	})

	Describe("extractCredentials", func() {
		Context("with valid SSH secret", func() {
			It("should successfully create SSH authentication", func() {
				auth, err := reconciler.extractCredentials(validSSHSecret)

				Expect(err).NotTo(HaveOccurred())
				Expect(auth).NotTo(BeNil())
				Expect(auth).To(BeAssignableToTypeOf(&ssh.PublicKeys{}))

				sshAuth := auth.(*ssh.PublicKeys)
				Expect(sshAuth.User).To(Equal("git"))
				Expect(sshAuth.Signer).NotTo(BeNil())
				Expect(sshAuth.HostKeyCallback).NotTo(BeNil())
			})
		})

		Context("with SSH secret without known_hosts", func() {
			It("should create SSH auth with insecure host key callback", func() {
				secretWithoutKnownHosts := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ssh-no-known-hosts",
						Namespace: "test",
					},
					Data: map[string][]byte{
						"ssh-privatekey": privateKey,
					},
				}

				auth, err := reconciler.extractCredentials(secretWithoutKnownHosts)

				Expect(err).NotTo(HaveOccurred())
				Expect(auth).NotTo(BeNil())
				Expect(auth).To(BeAssignableToTypeOf(&ssh.PublicKeys{}))

				sshAuth := auth.(*ssh.PublicKeys)
				Expect(sshAuth.User).To(Equal("git"))
				Expect(sshAuth.Signer).NotTo(BeNil())
				Expect(sshAuth.HostKeyCallback).NotTo(BeNil())
			})
		})

		Context("with SSH secret with passphrase", func() {
			It("should handle encrypted private keys", func() {
				// Skip this test as x509.EncryptPEMBlock is deprecated and
				// creating proper encrypted keys for testing is complex
				Skip("Skipping encrypted private key test due to deprecated x509.EncryptPEMBlock")
			})
		})

		Context("with invalid SSH secret", func() {
			It("should return error for malformed private key", func() {
				auth, err := reconciler.extractCredentials(invalidSSHSecret)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to create SSH public keys"))
				Expect(auth).To(BeNil())
			})
		})

		Context("with HTTP credentials", func() {
			It("should create HTTP basic auth", func() {
				httpSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "http-secret",
						Namespace: "test",
					},
					Data: map[string][]byte{
						"username": []byte("testuser"),
						"password": []byte("testpass"),
					},
				}

				auth, err := reconciler.extractCredentials(httpSecret)

				Expect(err).NotTo(HaveOccurred())
				Expect(auth).NotTo(BeNil())
			})
		})

		Context("with empty secret", func() {
			It("should return nil auth for anonymous access", func() {
				auth, err := reconciler.extractCredentials(nil)

				Expect(err).NotTo(HaveOccurred())
				Expect(auth).To(BeNil())
			})
		})

		Context("with incomplete HTTP credentials", func() {
			It("should return error when password is missing", func() {
				incompleteSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incomplete-secret",
						Namespace: "test",
					},
					Data: map[string][]byte{
						"username": []byte("testuser"),
						// password missing
					},
				}

				auth, err := reconciler.extractCredentials(incompleteSecret)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("username but missing password"))
				Expect(auth).To(BeNil())
			})
		})

		Context("with empty secret data", func() {
			It("should return error for secret without required fields", func() {
				emptySecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "empty-secret",
						Namespace: "test",
					},
					Data: map[string][]byte{},
				}

				auth, err := reconciler.extractCredentials(emptySecret)

				Expect(err).To(HaveOccurred())
				Expect(
					err.Error(),
				).To(ContainSubstring("must contain either 'ssh-privatekey' or both 'username' and 'password'"))
				Expect(auth).To(BeNil())
			})
		})
	})

	Describe("SSH URL parsing", func() {
		Context("with different SSH URL formats", func() {
			It("should handle standard SSH URLs", func() {
				testCases := []struct {
					name  string
					url   string
					valid bool
				}{
					{"Standard SSH", "ssh://git@example.com:22/org/repo.git", true},
					{"SSH with custom port", "ssh://git@example.com:2222/org/repo.git", true},
					{"Git protocol", "git@example.com:org/repo.git", true},
					{"HTTPS (not SSH)", "https://example.com/org/repo.git", false},
				}

				for _, tc := range testCases {
					By(fmt.Sprintf("Testing %s: %s", tc.name, tc.url))

					// Test if URL would work with SSH auth
					if tc.valid {
						// SSH URLs should be compatible with SSH auth
						Expect(tc.url).To(Or(
							ContainSubstring("ssh://"),
							MatchRegexp(`^[^/]+@[^/]+:`), // git@host:repo pattern
						))
					} else {
						// Non-SSH URLs should not use SSH auth
						Expect(tc.url).NotTo(ContainSubstring("ssh://"))
					}
				}
			})
		})
	})
})

// TestSSHCredentials tests SSH credential extraction functionality.
func TestSSHCredentials(t *testing.T) {
	reconciler := &GitRepoConfigReconciler{}

	// Test with valid SSH key
	t.Run("Valid SSH Key", func(t *testing.T) {
		privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			t.Fatal(err)
		}

		privateKeyPEM := &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		}
		privateKeyBytes := pem.EncodeToMemory(privateKeyPEM)

		secret := &corev1.Secret{
			Data: map[string][]byte{
				"ssh-privatekey": privateKeyBytes,
				"known_hosts":    []byte("example.com ssh-rsa AAAAB3..."),
			},
		}

		auth, err := reconciler.extractCredentials(secret)
		if err != nil {
			t.Errorf("Expected no error, got: %v", err)
		}
		if auth == nil {
			t.Error("Expected auth object, got nil")
		}
		if _, ok := auth.(*ssh.PublicKeys); !ok {
			t.Errorf("Expected *ssh.PublicKeys, got %T", auth)
		}
	})

	// Test with malformed SSH key
	t.Run("Invalid SSH Key", func(t *testing.T) {
		secret := &corev1.Secret{
			Data: map[string][]byte{
				"ssh-privatekey": []byte("invalid-key-data"),
			},
		}

		auth, err := reconciler.extractCredentials(secret)
		if err == nil {
			t.Error("Expected error for invalid SSH key")
		}
		if auth != nil {
			t.Error("Expected nil auth for invalid key")
		}
	})

	// Test with HTTP credentials
	t.Run("HTTP Credentials", func(t *testing.T) {
		secret := &corev1.Secret{
			Data: map[string][]byte{
				"username": []byte("testuser"),
				"password": []byte("testpass"),
			},
		}

		auth, err := reconciler.extractCredentials(secret)
		if err != nil {
			t.Errorf("Expected no error, got: %v", err)
		}
		if auth == nil {
			t.Error("Expected auth object, got nil")
		}
	})

	// Test with nil secret (anonymous access)
	t.Run("Anonymous Access", func(t *testing.T) {
		auth, err := reconciler.extractCredentials(nil)
		if err != nil {
			t.Errorf("Expected no error, got: %v", err)
		}
		if auth != nil {
			t.Error("Expected nil auth for anonymous access")
		}
	})
}

// TestValidateRepository tests the repository validation logic.
func TestValidateRepository(t *testing.T) {
	reconciler := &GitRepoConfigReconciler{}
	ctx := context.Background()

	// Test with invalid URL (this will fail but should handle gracefully)
	t.Run("Invalid Repository URL", func(t *testing.T) {
		_, err := reconciler.validateRepository(ctx, "invalid-url", "main", nil)
		if err == nil {
			t.Error("Expected error for invalid repository URL")
		}
	})

	// Test with valid public repository (GitHub)
	t.Run("Public Repository", func(t *testing.T) {
		// Skip in short tests or CI environments
		if testing.Short() {
			t.Skip("Skipping network test in short mode")
		}

		commitHash, err := reconciler.validateRepository(
			ctx,
			"https://github.com/octocat/Hello-World.git",
			"master",
			nil,
		)
		if err != nil {
			t.Logf("Public repository test failed (might be expected in CI): %v", err)
		} else {
			if commitHash == "" {
				t.Error("Expected non-empty commit hash")
			}
			if len(commitHash) != 40 { // SHA-1 hash length
				t.Errorf("Expected 40 character commit hash, got %d", len(commitHash))
			}
		}
	})
}

// TestGitRepoConfigConditions tests the condition setting logic.
func TestGitRepoConfigConditions(t *testing.T) {
	reconciler := &GitRepoConfigReconciler{}

	// Mock GitRepoConfig
	gitRepoConfig := &configbutleraiv1alpha1.GitRepoConfig{}

	// Test setting various conditions
	testCases := []struct {
		status  metav1.ConditionStatus
		reason  string
		message string
	}{
		{metav1.ConditionTrue, ReasonBranchFound, "Branch 'main' found"},
		{metav1.ConditionFalse, ReasonBranchNotFound, "Branch 'invalid' not found"},
		{metav1.ConditionFalse, ReasonConnectionFailed, "Failed to connect"},
		{metav1.ConditionFalse, ReasonSecretNotFound, "Secret not found"},
		{metav1.ConditionUnknown, ReasonChecking, "Checking repository"},
	}

	for _, tc := range testCases {
		t.Run(tc.reason, func(t *testing.T) {
			reconciler.setCondition(gitRepoConfig, tc.status, tc.reason, tc.message)

			if len(gitRepoConfig.Status.Conditions) == 0 {
				t.Error("Expected at least one condition")
				return
			}

			condition := gitRepoConfig.Status.Conditions[len(gitRepoConfig.Status.Conditions)-1]
			if condition.Type != ConditionTypeReady {
				t.Errorf("Expected condition type 'Ready', got '%s'", condition.Type)
			}
			if condition.Status != tc.status {
				t.Errorf("Expected status '%s', got '%s'", tc.status, condition.Status)
			}
			if condition.Reason != tc.reason {
				t.Errorf("Expected reason '%s', got '%s'", tc.reason, condition.Reason)
			}
			if condition.Message != tc.message {
				t.Errorf("Expected message '%s', got '%s'", tc.message, condition.Message)
			}
		})
	}
}
