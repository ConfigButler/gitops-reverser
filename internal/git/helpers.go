// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// GetCommitSigner fetches commit signing material from the specified secret.
func GetCommitSigner(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
) (gogit.Signer, error) {
	return getCommitSigner(ctx, k8sClient, provider)
}

func getCommitSigner(
	ctx context.Context,
	k8sClient client.Client,
	provider *v1alpha3.GitProvider,
) (gogit.Signer, error) {
	if provider == nil || provider.Spec.Commit == nil || provider.Spec.Commit.Signing == nil {
		return nil, nil //nolint:nilnil // absence of signing configuration is not an error
	}

	secretName := strings.TrimSpace(provider.Spec.Commit.Signing.SecretRef.Name)
	if secretName == "" {
		return nil, errors.New("commit.signing.secretRef.name must be set when signing is enabled")
	}

	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: provider.Namespace,
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("failed to get signing secret %s: %w", secretKey, err)
	}

	signer, err := LoadSSHCommitSigner(&secret)
	if err != nil {
		return nil, fmt.Errorf("failed to load signing key from secret %s: %w", secretKey, err)
	}

	return signer, nil
}
