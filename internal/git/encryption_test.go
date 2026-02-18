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
	"strings"
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

	t.Run("fails when provider is unsupported", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: "kms",
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported encryption provider")
	})

	t.Run("fails when secret name is missing", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider:  EncryptionProviderSOPS,
					SecretRef: v1alpha1.LocalSecretReference{},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "encryption.secretRef.name must be set")
	})

	t.Run("fails when secret is missing", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					Provider: EncryptionProviderSOPS,
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to fetch encryption secret")
	})

	t.Run("fails when secret has no valid environment keys", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "enc-secret", Namespace: "default"},
			Data: map[string][]byte{
				"invalid.key": []byte("value"),
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
				},
			},
		}

		_, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must contain at least one valid environment variable entry")
	})

	t.Run("returns resolved environment for valid sops config", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "enc-secret", Namespace: "default"},
			Data: map[string][]byte{
				"SOPS_AGE_KEY": []byte("AGE-SECRET-KEY-1example"),
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
				},
			},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, EncryptionProviderSOPS, resolved.Provider)
		assert.Equal(t, "AGE-SECRET-KEY-1example", resolved.Environment["SOPS_AGE_KEY"])
	})

	t.Run("defaults provider to sops when omitted", func(t *testing.T) {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "enc-secret", Namespace: "default"},
			Data: map[string][]byte{
				"SOPS_AGE_KEY": []byte("AGE-SECRET-KEY-1example"),
			},
		}).Build()
		target := &v1alpha1.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
			Spec: v1alpha1.GitTargetSpec{
				Encryption: &v1alpha1.EncryptionSpec{
					SecretRef: v1alpha1.LocalSecretReference{
						Name: "enc-secret",
					},
				},
			},
		}

		resolved, err := ResolveTargetEncryption(context.Background(), k8sClient, target)
		require.NoError(t, err)
		require.NotNil(t, resolved)
		assert.Equal(t, EncryptionProviderSOPS, resolved.Provider)
	})
}

func TestDeriveAgeRecipientFromSOPSKey(t *testing.T) {
	t.Run("returns recipient for single valid identity", func(t *testing.T) {
		identity, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		recipient, err := deriveAgeRecipientFromSOPSKey(identity.String())
		require.NoError(t, err)
		assert.Equal(t, identity.Recipient().String(), recipient)
	})

	t.Run("fails when identity is missing", func(t *testing.T) {
		_, err := deriveAgeRecipientFromSOPSKey("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must contain one AGE-SECRET-KEY identity")
	})

	t.Run("fails when identity is invalid", func(t *testing.T) {
		_, err := deriveAgeRecipientFromSOPSKey("AGE-SECRET-KEY-1invalid")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid SOPS_AGE_KEY identity")
	})

	t.Run("fails when multiple identities are present", func(t *testing.T) {
		first, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		second, err := age.GenerateX25519Identity()
		require.NoError(t, err)

		combined := strings.Join([]string{first.String(), second.String()}, "\n")
		_, err = deriveAgeRecipientFromSOPSKey(combined)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must contain exactly one AGE-SECRET-KEY identity")
	})
}
