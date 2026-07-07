// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
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

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func TestResolveTargetEncryption(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))

	t.Run("returns nil when encryption is not configured", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		assert.Nil(t, resolved)
	})

	t.Run("returns nil when age is disabled", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: "kms",
					Age: &v1alpha3.AgeEncryptionSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha3.AgeEncryptionSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
	})

	t.Run("fails when extractFromSecret is enabled and secret name is empty", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					SecretRef: v1alpha3.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					SecretRef: v1alpha3.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
				// A non-agekey entry that also happens to be a valid env var name must be
				// ignored entirely: it is neither a recipient source nor passed to sops.
				"SOPS_KMS_ARN": []byte("kms-arn"),
			},
		}).Build()
		target := &v1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha3.GitTargetSpec{
				Encryption: &v1alpha3.EncryptionSpec{
					SecretRef: v1alpha3.LocalSecretReference{
						Name: "enc-secret",
					},
					Age: &v1alpha3.AgeEncryptionSpec{
						Enabled: true,
						Recipients: v1alpha3.AgeRecipientsSpec{
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
	})
}

// TestConfigureSecretEncryptionWriter_NoPrivateAgeMaterial proves the SOPS write path never
// receives private age identity material: no SOPS_AGE_KEY_FILE, and no environment carrying
// AGE-SECRET-KEY content. Recipients reach sops through the target's .sops.yaml, not the env.
func TestConfigureSecretEncryptionWriter_NoPrivateAgeMaterial(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	writer := &contentWriter{}
	cfg := &ResolvedEncryptionConfig{
		Provider:      EncryptionProviderSOPS,
		AgeRecipients: []string{identity.Recipient().String()},
	}

	require.NoError(t, configureSecretEncryptionWriter(writer, t.TempDir(), cfg))

	encryptor, ok := writer.encryptor.(*SOPSEncryptor)
	require.True(t, ok, "expected a *SOPSEncryptor to be configured")
	assert.Empty(t, encryptor.env, "SOPS env must be empty; no private key file or secret data")
	_, hasKeyFile := encryptor.env["SOPS_AGE_KEY_FILE"]
	assert.False(t, hasKeyFile, "the age private-key file env var must never be set")
	for key, value := range encryptor.env {
		assert.NotContains(t, value, "AGE-SECRET-KEY", "env var %q must not carry a private age key", key)
	}
}

func TestDeriveAgeRecipientFromSecretEntry(t *testing.T) {
	t.Run("returns recipient for single valid identity", func(t *testing.T) {
		identity, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		recipient, err := deriveAgeRecipientFromSecretEntry("identity.agekey", identity.String())
		require.NoError(t, err)
		assert.Equal(t, identity.Recipient().String(), recipient)
	})

	t.Run("fails when identity is missing", func(t *testing.T) {
		_, err := deriveAgeRecipientFromSecretEntry("identity.agekey", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "identity.agekey must contain one AGE-SECRET-KEY identity")
	})

	t.Run("fails when identity is invalid", func(t *testing.T) {
		_, err := deriveAgeRecipientFromSecretEntry("identity.agekey", "AGE-SECRET-KEY-1invalid")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid identity.agekey identity")
	})

	t.Run("fails when multiple identities are present", func(t *testing.T) {
		first, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		second, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		combined := first.String() + "\n" + second.String()
		_, err = deriveAgeRecipientFromSecretEntry("identity.agekey", combined)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must contain exactly one AGE-SECRET-KEY identity")
	})
}
