// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

// A STORED GitProvider carrying either relocated field is refused rather than half-honoured.
// Admission rejects both on write, so only an object written by an earlier release reaches this,
// and neither of the alternatives is acceptable: honouring the values keeps the folder's cadence
// and wording coming from the connection after the API says they come from the folder, and
// ignoring them changes both without telling anyone.
func TestRefuseRelocatedCommitFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    configbutleraiv1alpha3.GitProviderSpec
		refused bool
	}{
		{
			name:    "clean provider",
			spec:    configbutleraiv1alpha3.GitProviderSpec{URL: "git@example.com:o/r.git"},
			refused: false,
		},
		{
			name: "committer and signing are untouched by the move",
			spec: configbutleraiv1alpha3.GitProviderSpec{
				Commit: &configbutleraiv1alpha3.CommitSpec{
					Committer: &configbutleraiv1alpha3.CommitterSpec{Name: "Bot"},
				},
			},
			refused: false,
		},
		{
			name: "stored spec.push",
			spec: configbutleraiv1alpha3.GitProviderSpec{
				//nolint:staticcheck // setting the removed field is the point.
				Push: &configbutleraiv1alpha3.PushStrategy{CommitWindow: ptr.To("30s")},
			},
			refused: true,
		},
		{
			name: "stored spec.commit.message",
			spec: configbutleraiv1alpha3.GitProviderSpec{
				Commit: &configbutleraiv1alpha3.CommitSpec{
					//nolint:staticcheck // setting the removed field is the point.
					Message: &configbutleraiv1alpha3.CommitMessageSpec{EventTemplate: "x"},
				},
			},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseRelocatedCommitFields(&configbutleraiv1alpha3.GitProvider{Spec: tc.spec})
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrRelocatedCommitFields)
			assert.Contains(t, err.Error(), "GitTarget.spec.commit.window",
				"the refusal must name where the value went, not merely that it is gone")
			assert.Contains(t, err.Error(), "GitTarget.spec.commit.message")
		})
	}
}

func TestValidateCommitConfiguration_SigningEnabled(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha3.GitProvider{
		Status: configbutleraiv1alpha3.GitProviderStatus{
			SigningPublicKey: "ssh-ed25519 AAAA old",
		},
		Spec: configbutleraiv1alpha3.GitProviderSpec{
			Commit: &configbutleraiv1alpha3.CommitSpec{
				Signing: &configbutleraiv1alpha3.CommitSigningSpec{
					SecretRef: configbutleraiv1alpha3.LocalSecretReference{
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
					SecretRef:           configbutleraiv1alpha3.LocalSecretReference{Name: "signing-secret"},
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
					SecretRef: configbutleraiv1alpha3.LocalSecretReference{Name: "signing-secret"},
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
					SecretRef: configbutleraiv1alpha3.LocalSecretReference{Name: "signing-secret"},
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
