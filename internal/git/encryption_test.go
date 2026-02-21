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

package git

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestResolveTargetEncryption(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	t.Run("returns nil when encryption is not configured", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		assert.Nil(t, resolved)
	})

	t.Run("returns nil when age is disabled", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
				},
			},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		assert.Nil(t, resolved)
	})

	t.Run("fails when provider is unsupported", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: "kms",
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported encryption provider")
	})

	t.Run("fails when public key is invalid", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							PublicKeys: []string{"invalid-recipient"},
						},
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid age recipient")
	})

	t.Run("fails when no recipient resolves", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires at least one resolved recipient")
	})

	t.Run("resolves public key only mode without secret", func(t *testing.T) {
		identity, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							PublicKeys: []string{identity.Recipient().String()},
						},
					},
				},
			},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, []string{identity.Recipient().String()}, resolved.AgeRecipients)
		assert.Nil(t, resolved.Environment)
	})

	t.Run("fails when extractFromSecret is enabled and secret name is empty", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							ExtractFromSecret: true,
						},
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "encryption.secretRef.name must be set")
	})

	t.Run("fails when extracted secret is missing", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							ExtractFromSecret: true,
						},
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to fetch encryption secret")
	})

	t.Run("fails when extracted agekey data is invalid", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "enc-secret", Namespace: "default"},
			Data: map[string][]byte{
				"identity.agekey": []byte("invalid"),
			},
		}).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							ExtractFromSecret: true,
						},
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identity.agekey must contain AGE-SECRET-KEY identity")
	})

	t.Run("resolves recipients from public keys and secret entries", func(t *testing.T) {
		firstIdentity, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		secondIdentity, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		thirdIdentity, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "enc-secret", Namespace: "default"},
			Data: map[string][]byte{
				"identity.agekey": []byte(firstIdentity.String()),
				"backup.agekey":   []byte(secondIdentity.String()),
				"SOPS_KMS_ARN":    []byte("kms-arn"),
			},
		}).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha1.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha1.AgeRecipientsSpec{
							PublicKeys: []string{
								thirdIdentity.Recipient().String(),
								firstIdentity.Recipient().String(),
							},
							ExtractFromSecret: true,
						},
					},
				},
			},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, EncryptionProviderSOPS, resolved.Provider)

		expectedRecipients := []string{
			firstIdentity.Recipient().String(),
			secondIdentity.Recipient().String(),
			thirdIdentity.Recipient().String(),
		}
		sort.Strings(expectedRecipients)
		assert.Equal(t, expectedRecipients, resolved.AgeRecipients)
		assert.Equal(t, "kms-arn", resolved.Environment["SOPS_KMS_ARN"])

		expectedIdentities := []string{
			firstIdentity.String(),
			secondIdentity.String(),
		}
		sort.Strings(expectedIdentities)
		assert.Equal(t, expectedIdentities, resolved.AgeIdentities)
	})
}

func TestBuildSOPSEnvironment(t *testing.T) {
	t.Run("returns base environment when no age identities exist", func(t *testing.T) {
		cfg := &ResolvedEncryptionConfig{
			Environment: map[string]string{
				"SOPS_KMS_ARN": "kms-arn",
			},
		}

		env, err := buildSOPSEnvironment(t.TempDir(), cfg)
		require.NoError(t, err)
		assert.Equal(t, "kms-arn", env["SOPS_KMS_ARN"])
		_, hasKeyFile := env[sopsAgeKeyFileEnvVar]
		assert.False(t, hasKeyFile)
	})

	t.Run("writes age identities to temp file and sets SOPS age key file env", func(t *testing.T) {
		firstIdentity, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		secondIdentity, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		workDir := t.TempDir()
		cfg := &ResolvedEncryptionConfig{
			Environment: map[string]string{
				"SOPS_KMS_ARN": "kms-arn",
			},
			AgeIdentities: []string{firstIdentity.String(), secondIdentity.String()},
		}

		env, err := buildSOPSEnvironment(workDir, cfg)
		require.NoError(t, err)
		keyFilePath := env[sopsAgeKeyFileEnvVar]
		require.NotEmpty(t, keyFilePath)
		assert.Equal(t, ageIdentityFileDir, filepath.Base(filepath.Dir(keyFilePath)))

		content, err := os.ReadFile(keyFilePath)
		require.NoError(t, err)
		assert.Contains(t, string(content), firstIdentity.String())
		assert.Contains(t, string(content), secondIdentity.String())
		assert.Equal(t, "kms-arn", env["SOPS_KMS_ARN"])
	})
}

func TestDeriveAgeRecipientFromSecretEntry(t *testing.T) {
	t.Run("returns recipient for single valid identity", func(t *testing.T) {
		identity, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		recipient, parsedIdentity, err := deriveAgeRecipientFromSecretEntry("identity.agekey", identity.String())
		require.NoError(t, err)
		assert.Equal(t, identity.Recipient().String(), recipient)
		assert.Equal(t, identity.String(), parsedIdentity)
	})

	t.Run("fails when identity is missing", func(t *testing.T) {
		_, _, err := deriveAgeRecipientFromSecretEntry("identity.agekey", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identity.agekey must contain one AGE-SECRET-KEY identity")
	})

	t.Run("fails when identity is invalid", func(t *testing.T) {
		_, _, err := deriveAgeRecipientFromSecretEntry("identity.agekey", "AGE-SECRET-KEY-1invalid")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid identity.agekey identity")
	})

	t.Run("fails when multiple identities are present", func(t *testing.T) {
		first, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		second, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		combined := first.String() + "\n" + second.String()
		_, _, err = deriveAgeRecipientFromSecretEntry("identity.agekey", combined)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must contain exactly one AGE-SECRET-KEY identity")
	})
}
