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
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

func (r *GitProviderReconciler) ensureSigningKey(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha1.GitProvider,
) error {
	if gitProvider == nil || gitProvider.Spec.Commit == nil || gitProvider.Spec.Commit.Signing == nil {
		return nil
	}
	gitProvider.Status.SigningPublicKey = ""

	secretKey, err := signingSecretKey(gitProvider)
	if err != nil {
		return err
	}

	var secret corev1.Secret
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			if !gitProvider.Spec.Commit.Signing.GenerateWhenMissing {
				return fmt.Errorf("signing secret %s not found", secretKey.String())
			}
			return r.createGeneratedSigningSecret(ctx, secretKey, gitProvider)
		}
		return fmt.Errorf("failed to fetch signing secret %s: %w", secretKey.String(), err)
	}

	if len(secret.Data[gitpkg.SigningKeyDataKey]) == 0 {
		if !gitProvider.Spec.Commit.Signing.GenerateWhenMissing {
			return fmt.Errorf("signing secret %s is missing signing.key and generateWhenMissing is disabled",
				secretKey.String())
		}
		if err := r.addGeneratedSigningKeyToSecret(ctx, &secret); err != nil {
			return err
		}
	}

	publicKey, err := gitpkg.SSHAuthorizedPublicKeyFromSecret(&secret)
	if err != nil {
		return fmt.Errorf("failed to derive signing public key from secret %s: %w", secretKey.String(), err)
	}

	gitProvider.Status.SigningPublicKey = publicKey
	return nil
}

func signingSecretKey(
	gitProvider *configbutleraiv1alpha1.GitProvider,
) (k8stypes.NamespacedName, error) {
	if gitProvider == nil || gitProvider.Spec.Commit == nil || gitProvider.Spec.Commit.Signing == nil {
		return k8stypes.NamespacedName{}, errors.New("commit signing is not configured")
	}

	secretName := strings.TrimSpace(gitProvider.Spec.Commit.Signing.SecretRef.Name)
	if secretName == "" {
		return k8stypes.NamespacedName{}, errors.New(
			"commit.signing.secretRef.name must be set when signing is enabled",
		)
	}

	return k8stypes.NamespacedName{Name: secretName, Namespace: gitProvider.Namespace}, nil
}

func (r *GitProviderReconciler) createGeneratedSigningSecret(
	ctx context.Context,
	secretKey k8stypes.NamespacedName,
	gitProvider *configbutleraiv1alpha1.GitProvider,
) error {
	privateKey, publicKey, err := gitpkg.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		return fmt.Errorf("failed to generate signing key for secret %s: %w", secretKey.String(), err)
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretKey.Name,
			Namespace: secretKey.Namespace,
			// No owner reference is set intentionally: signing key material must survive
			// GitProvider deletion to avoid accidental loss of a deployed signing identity.
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			gitpkg.SigningKeyDataKey:       privateKey,
			gitpkg.SigningPublicKeyDataKey: publicKey,
		},
	}

	if err := r.Create(ctx, &secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.ensureSigningKey(ctx, gitProvider)
		}
		return fmt.Errorf("failed to create signing secret %s: %w", secretKey.String(), err)
	}

	gitProvider.Status.SigningPublicKey = string(publicKey)
	return nil
}

func (r *GitProviderReconciler) addGeneratedSigningKeyToSecret(
	ctx context.Context,
	secret *corev1.Secret,
) error {
	if secret == nil {
		return errors.New("signing secret is nil")
	}

	privateKey, publicKey, err := gitpkg.GenerateSSHSigningKeyPair(secret.Data[gitpkg.SigningPassphraseDataKey])
	if err != nil {
		return fmt.Errorf("failed to generate signing key for secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}

	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[gitpkg.SigningKeyDataKey] = privateKey
	secret.Data[gitpkg.SigningPublicKeyDataKey] = publicKey

	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("failed to update signing secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}

	return nil
}
