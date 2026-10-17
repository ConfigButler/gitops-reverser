// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

func TestValidateCommitConfiguration_SigningEnabled(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha3.GitProvider{
		Status: configbutleraiv1alpha3.GitProviderStatus{
			SigningPublicKey: "ssh-ed25519 AAAA old",
		},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Commit: &configbutleraiv1alpha3.CommitSpec{
				Signing: &configbutleraiv1alpha3.CommitSigningSpec{
					SecretRef: meta.LocalObjectReference{
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
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Commit: &configbutleraiv1alpha3.CommitSpec{
				Signing: &configbutleraiv1alpha3.CommitSigningSpec{
					SecretRef:           meta.LocalObjectReference{Name: "signing-secret"},
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
	assert.NotEmpty(t, secret.Data[gitpkg.SigningKeyDataKey])
	assert.NotEmpty(t, secret.Data[gitpkg.SigningPublicKeyDataKey])
	assert.Equal(t, provider.Status.SigningPublicKey, string(secret.Data[gitpkg.SigningPublicKeyDataKey]))
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
			gitpkg.SigningKeyDataKey: privateKey,
		},
	}

	reconciler := &GitProviderReconciler{Client: newGitProviderTestClient(t, secret)}
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Commit: &configbutleraiv1alpha3.CommitSpec{
				Signing: &configbutleraiv1alpha3.CommitSigningSpec{
					SecretRef: meta.LocalObjectReference{Name: "signing-secret"},
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
	_, hasPublicKey := updatedSecret.Data[gitpkg.SigningPublicKeyDataKey]
	assert.False(t, hasPublicKey)
}

func TestEnsureSigningKey_MissingSecretWithoutGeneration(t *testing.T) {
	ctx := context.Background()
	reconciler := &GitProviderReconciler{Client: newGitProviderTestClient(t)}
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider",
			Namespace: "default",
		},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Commit: &configbutleraiv1alpha3.CommitSpec{
				Signing: &configbutleraiv1alpha3.CommitSigningSpec{
					SecretRef: meta.LocalObjectReference{Name: "signing-secret"},
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
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()
}

// TestPublishWebhookPath_FollowsSpecReceiverAndToken pins status.webhookPath to spec.receiver and
// its token: derived from the token while both exist, moved when the token rotates, and cleared when
// either goes, so a stale path never points a Git host at a route that has stopped serving.
func TestPublishWebhookPath_FollowsSpecReceiverAndToken(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver-token", Namespace: "team-a"},
		Data:       map[string][]byte{configbutleraiv1alpha3.ReceiverTokenKey: []byte("first")},
	}
	c := newGitProviderTestClient(t, secret)
	reconciler := &GitProviderReconciler{Client: c}
	provider := &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-repo", Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Receiver: &configbutleraiv1alpha3.GitProviderReceiver{
				Type:      configbutleraiv1alpha3.ReceiverTypeGitHub,
				SecretRef: meta.LocalObjectReference{Name: "receiver-token"},
			},
		},
	}
	log := logr.Discard()

	reconciler.publishWebhookPath(ctx, log, provider)
	first := provider.Status.WebhookPath
	assert.Equal(t, configbutleraiv1alpha3.ReceiverPath([]byte("first"), "team-a", "platform-repo"), first)
	assert.NotContains(t, first, "team-a", "the path must not name the namespace")
	assert.NotContains(t, first, "platform-repo", "the path must not name the provider")

	secret.Data[configbutleraiv1alpha3.ReceiverTokenKey] = []byte("second")
	require.NoError(t, c.Update(ctx, secret))
	reconciler.publishWebhookPath(ctx, log, provider)
	assert.NotEqual(t, first, provider.Status.WebhookPath, "a rotated token moves the path")

	secret.Data = map[string][]byte{"password": []byte("second")}
	require.NoError(t, c.Update(ctx, secret))
	reconciler.publishWebhookPath(ctx, log, provider)
	assert.Empty(t, provider.Status.WebhookPath, "no token, no path")

	provider.Status.WebhookPath = "/git-push/stale"
	require.NoError(t, c.Delete(ctx, secret))
	reconciler.publishWebhookPath(ctx, log, provider)
	assert.Empty(t, provider.Status.WebhookPath, "no Secret, no path")

	provider.Status.WebhookPath = "/git-push/stale"
	provider.Spec.Receiver = nil
	reconciler.publishWebhookPath(ctx, log, provider)
	assert.Empty(t, provider.Status.WebhookPath, "no receiver, no path")
}
