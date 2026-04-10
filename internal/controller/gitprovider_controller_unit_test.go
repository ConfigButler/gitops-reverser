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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

func TestValidateCommitConfiguration_InvalidTemplate(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha1.GitProvider{
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Message: &configbutleraiv1alpha1.CommitMessageSpec{
					Template: "{{.Operation",
				},
			},
		},
	}

	err := reconciler.validateCommitConfiguration(provider)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid commit configuration")
	assert.Empty(t, provider.Status.SigningPublicKey)
}

func TestValidateCommitConfiguration_SigningEnabled(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha1.GitProvider{
		Status: configbutleraiv1alpha1.GitProviderStatus{
			SigningPublicKey: "ssh-ed25519 AAAA old",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Signing: &configbutleraiv1alpha1.CommitSigningSpec{
					SecretRef: configbutleraiv1alpha1.LocalSecretReference{
						Name: "signing-secret",
					},
				},
			},
		},
	}

	err := reconciler.validateCommitConfiguration(provider)
	require.NoError(t, err)
	assert.Empty(t, provider.Status.SigningPublicKey)
}

func TestEnsureSigningKey_GeneratesMissingSecret(t *testing.T) {
	ctx := context.Background()
	reconciler := &GitProviderReconciler{Client: newGitProviderTestClient(t)}
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Signing: &configbutleraiv1alpha1.CommitSigningSpec{
					SecretRef:           configbutleraiv1alpha1.LocalSecretReference{Name: "signing-secret"},
					GenerateWhenMissing: true,
				},
			},
		},
	}

	err := reconciler.ensureSigningKey(ctx, provider)
	require.NoError(t, err)
	assert.Contains(t, provider.Status.SigningPublicKey, "ssh-ed25519 ")

	var secret corev1.Secret
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "signing-secret", Namespace: "default"}, &secret))
	assert.NotEmpty(t, secret.Data["signing.key"])
	assert.NotEmpty(t, secret.Data["signing.pub"])
	assert.Equal(t, provider.Status.SigningPublicKey, string(secret.Data["signing.pub"]))
}

func TestEnsureSigningKey_UsesExistingKey(t *testing.T) {
	ctx := context.Background()
	privateKey, publicKey, err := gitpkg.GenerateSSHSigningKeyPair(nil)
	require.NoError(t, err)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "signing-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"signing.key": privateKey,
		},
	}

	reconciler := &GitProviderReconciler{Client: newGitProviderTestClient(t, secret)}
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Signing: &configbutleraiv1alpha1.CommitSigningSpec{
					SecretRef: configbutleraiv1alpha1.LocalSecretReference{Name: "signing-secret"},
				},
			},
		},
	}

	err = reconciler.ensureSigningKey(ctx, provider)
	require.NoError(t, err)
	assert.Equal(t, string(publicKey), provider.Status.SigningPublicKey)

	var updatedSecret corev1.Secret
	require.NoError(
		t,
		reconciler.Get(ctx, types.NamespacedName{Name: "signing-secret", Namespace: "default"}, &updatedSecret),
	)
	_, hasPublicKey := updatedSecret.Data["signing.pub"]
	assert.False(t, hasPublicKey)
}

func TestEnsureSigningKey_MissingSecretWithoutGeneration(t *testing.T) {
	ctx := context.Background()
	reconciler := &GitProviderReconciler{Client: newGitProviderTestClient(t)}
	provider := &configbutleraiv1alpha1.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Signing: &configbutleraiv1alpha1.CommitSigningSpec{
					SecretRef: configbutleraiv1alpha1.LocalSecretReference{Name: "signing-secret"},
				},
			},
		},
	}

	err := reconciler.ensureSigningKey(ctx, provider)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, provider.Status.SigningPublicKey)
}

func newGitProviderTestClient(t *testing.T, objects ...runtime.Object) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()
}
