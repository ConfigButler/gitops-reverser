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
	"fmt"
	"regexp"
	"strings"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ResourceMeta is passed to encryptors for context and diagnostics.
type ResourceMeta struct {
	Identifier      itypes.ResourceIdentifier
	UID             string
	ResourceVersion string
	Generation      int64
}

// Encryptor transforms plaintext bytes into encrypted bytes.
type Encryptor interface {
	Encrypt(ctx context.Context, plain []byte, meta ResourceMeta) ([]byte, error)
}

const (
	// EncryptionProviderSOPS is the only supported provider in this increment.
	EncryptionProviderSOPS = "sops"
	// defaultSOPSBinaryPath is resolved from PATH in the controller runtime image.
	defaultSOPSBinaryPath = "sops"
	// sopsAgeKeyEnvVar is the required secret key for age-based SOPS encryption.
	sopsAgeKeyEnvVar = "SOPS_AGE_KEY"
)

var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ResolvedEncryptionConfig contains runtime encryption settings resolved from GitTarget spec.
type ResolvedEncryptionConfig struct {
	Provider    string
	Environment map[string]string
}

// ResolveTargetEncryption resolves and validates GitTarget encryption configuration.
func ResolveTargetEncryption(
	ctx context.Context,
	k8sClient client.Client,
	target *v1alpha1.GitTarget,
) (*ResolvedEncryptionConfig, error) {
	if target.Spec.Encryption == nil {
		return nil, nil //nolint:nilnil // nil means encryption disabled
	}

	encryptionSpec := target.Spec.Encryption
	providerName := strings.TrimSpace(encryptionSpec.Provider)
	if providerName == "" {
		providerName = EncryptionProviderSOPS
	}
	if providerName != EncryptionProviderSOPS {
		return nil, fmt.Errorf("unsupported encryption provider %q", encryptionSpec.Provider)
	}

	secretKind := strings.TrimSpace(encryptionSpec.SecretRef.Kind)
	if secretKind != "" && secretKind != "Secret" {
		return nil, fmt.Errorf("encryption.secretRef.kind must be Secret, got %q", encryptionSpec.SecretRef.Kind)
	}

	secretName := strings.TrimSpace(encryptionSpec.SecretRef.Name)
	if secretName == "" {
		return nil, fmt.Errorf("encryption.secretRef.name must be set for provider %q", providerName)
	}

	var secret corev1.Secret
	secretKey := k8stypes.NamespacedName{
		Name:      secretName,
		Namespace: target.Namespace,
	}
	if err := k8sClient.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("failed to fetch encryption secret %s: %w", secretKey, err)
	}

	environment := toSOPSEnvironment(secret.Data)
	if len(environment) == 0 {
		return nil, fmt.Errorf(
			"encryption secret %s must contain at least one valid environment variable entry",
			secretKey,
		)
	}

	return &ResolvedEncryptionConfig{
		Provider:    providerName,
		Environment: environment,
	}, nil
}

func configureSecretEncryptionWriter(
	writer *contentWriter,
	workDir string,
	cfg *ResolvedEncryptionConfig,
) error {
	if cfg == nil {
		writer.setEncryptor(nil)
		return nil
	}

	switch cfg.Provider {
	case EncryptionProviderSOPS:
		writer.setEncryptor(NewSOPSEncryptorWithEnv(defaultSOPSBinaryPath, "", workDir, cfg.Environment))
		return nil
	default:
		return fmt.Errorf("unsupported encryption provider %q", cfg.Provider)
	}
}

func toSOPSEnvironment(secretData map[string][]byte) map[string]string {
	if len(secretData) == 0 {
		return nil
	}

	environment := make(map[string]string, len(secretData))
	for key, value := range secretData {
		if !envVarNamePattern.MatchString(key) {
			continue
		}
		environment[key] = string(value)
	}

	if len(environment) == 0 {
		return nil
	}

	return environment
}

func deriveAgeRecipientFromSOPSKey(secretValue string) (string, error) {
	lines := strings.Split(secretValue, "\n")
	identities := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		identities = append(identities, trimmed)
	}

	if len(identities) == 0 {
		return "", fmt.Errorf("%s must contain one AGE-SECRET-KEY identity", sopsAgeKeyEnvVar)
	}
	if len(identities) > 1 {
		return "", fmt.Errorf("%s must contain exactly one AGE-SECRET-KEY identity", sopsAgeKeyEnvVar)
	}
	if !strings.HasPrefix(identities[0], "AGE-SECRET-KEY-") {
		return "", fmt.Errorf("%s must contain AGE-SECRET-KEY identity", sopsAgeKeyEnvVar)
	}

	identity, err := age.ParseX25519Identity(identities[0])
	if err != nil {
		return "", fmt.Errorf("invalid %s identity: %w", sopsAgeKeyEnvVar, err)
	}

	return identity.Recipient().String(), nil
}
