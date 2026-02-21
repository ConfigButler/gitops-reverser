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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	// sopsAgeKeyFileEnvVar points SOPS to a file containing age private identities.
	sopsAgeKeyFileEnvVar = "SOPS" + "_AGE_KEY_FILE"
	// ageSecretKeySuffix identifies Flux-compatible age private-key entries.
	ageSecretKeySuffix = ".agekey"
	// ageIdentityFileDir is the temp directory used for SOPS age identity files.
	ageIdentityFileDir = "gitops-reverser-age-identities"
)

var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ResolvedEncryptionConfig contains runtime encryption settings resolved from GitTarget spec.
type ResolvedEncryptionConfig struct {
	Provider      string
	Environment   map[string]string
	AgeRecipients []string
	AgeIdentities []string
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
	providerName, err := resolveEncryptionProvider(encryptionSpec)
	if err != nil {
		return nil, err
	}
	ageSpec := encryptionSpec.Age
	if ageSpec == nil || !ageSpec.Enabled {
		return nil, nil //nolint:nilnil // nil means encryption disabled for current provider implementation
	}

	publicRecipients, err := normalizePublicAgeRecipients(ageSpec.Recipients.PublicKeys)
	if err != nil {
		return nil, err
	}
	secretRecipients, secretIdentities, environment, err := resolveSecretRecipientsAndEnvironment(
		ctx,
		k8sClient,
		target,
		encryptionSpec,
	)
	if err != nil {
		return nil, err
	}

	resolvedRecipients := dedupeAndSortRecipients(append(publicRecipients, secretRecipients...))
	if len(resolvedRecipients) == 0 {
		return nil, errors.New(
			"encryption.age.enabled=true requires at least one resolved recipient from age.recipients.publicKeys or secret *.agekey entries",
		)
	}

	environment = normalizeEnvironment(environment)
	return &ResolvedEncryptionConfig{
		Provider:      providerName,
		Environment:   environment,
		AgeRecipients: resolvedRecipients,
		AgeIdentities: secretIdentities,
	}, nil
}

func resolveEncryptionProvider(encryptionSpec *v1alpha1.EncryptionSpec) (string, error) {
	providerName := strings.TrimSpace(encryptionSpec.Provider)
	if providerName == "" {
		providerName = EncryptionProviderSOPS
	}
	if providerName != EncryptionProviderSOPS {
		return "", fmt.Errorf("unsupported encryption provider %q", encryptionSpec.Provider)
	}
	return providerName, nil
}

func resolveSecretRecipientsAndEnvironment(
	ctx context.Context,
	k8sClient client.Client,
	target *v1alpha1.GitTarget,
	encryptionSpec *v1alpha1.EncryptionSpec,
) ([]string, []string, map[string]string, error) {
	if encryptionSpec.Age == nil || !encryptionSpec.Age.Recipients.ExtractFromSecret {
		return nil, nil, nil, nil
	}

	secretKind := strings.TrimSpace(encryptionSpec.SecretRef.Kind)
	if secretKind != "" && secretKind != "Secret" {
		return nil, nil, nil, fmt.Errorf(
			"encryption.secretRef.kind must be Secret, got %q",
			encryptionSpec.SecretRef.Kind,
		)
	}

	secretName := strings.TrimSpace(encryptionSpec.SecretRef.Name)
	if secretName == "" {
		return nil, nil, nil, errors.New(
			"encryption.secretRef.name must be set when age.recipients.extractFromSecret=true",
		)
	}

	secret, secretKey, err := getEncryptionSecret(ctx, k8sClient, target.Namespace, secretName)
	if err != nil {
		return nil, nil, nil, err
	}

	secretRecipients, secretIdentities, err := resolveAgeRecipientsFromSecret(secret.Data)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to resolve recipients from encryption secret %s: %w", secretKey, err)
	}

	environment := toSOPSEnvironment(secret.Data)
	return secretRecipients, secretIdentities, environment, nil
}

func getEncryptionSecret(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	secretName string,
) (*corev1.Secret, k8stypes.NamespacedName, error) {
	secretKey := k8stypes.NamespacedName{
		Name:      secretName,
		Namespace: namespace,
	}
	var secret corev1.Secret
	if err := k8sClient.Get(ctx, secretKey, &secret); err != nil {
		return nil, secretKey, fmt.Errorf("failed to fetch encryption secret %s: %w", secretKey, err)
	}

	return &secret, secretKey, nil
}

func normalizeEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return nil
	}
	return environment
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
		environment, err := buildSOPSEnvironment(workDir, cfg)
		if err != nil {
			return err
		}
		writer.setEncryptor(NewSOPSEncryptorWithEnv(defaultSOPSBinaryPath, "", workDir, environment))
		return nil
	default:
		return fmt.Errorf("unsupported encryption provider %q", cfg.Provider)
	}
}

func buildSOPSEnvironment(workDir string, cfg *ResolvedEncryptionConfig) (map[string]string, error) {
	environment := cloneEnvironment(cfg.Environment)
	if len(cfg.AgeIdentities) == 0 {
		return environment, nil
	}

	ageKeyFilePath, err := writeAgeIdentityFile(workDir, cfg.AgeIdentities)
	if err != nil {
		return nil, fmt.Errorf("failed to write SOPS age identity file: %w", err)
	}
	if environment == nil {
		environment = make(map[string]string, 1)
	}
	environment[sopsAgeKeyFileEnvVar] = ageKeyFilePath
	return environment, nil
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func writeAgeIdentityFile(workDir string, identities []string) (string, error) {
	if len(identities) == 0 {
		return "", errors.New("no age identities provided")
	}

	dirPath := filepath.Join(os.TempDir(), ageIdentityFileDir)
	if err := os.MkdirAll(dirPath, 0700); err != nil {
		return "", fmt.Errorf("create age identity directory: %w", err)
	}

	keyHash := sha256.Sum256([]byte(strings.TrimSpace(workDir)))
	fileName := hex.EncodeToString(keyHash[:8]) + ageSecretKeySuffix
	filePath := filepath.Join(dirPath, fileName)
	fileContent := strings.Join(identities, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(fileContent), 0600); err != nil {
		return "", fmt.Errorf("write age identity file: %w", err)
	}

	return filePath, nil
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

func resolveAgeRecipientsFromSecret(secretData map[string][]byte) ([]string, []string, error) {
	if len(secretData) == 0 {
		return nil, nil, nil
	}

	recipients := make([]string, 0, len(secretData))
	identities := make([]string, 0, len(secretData))
	for key, value := range secretData {
		if !strings.HasSuffix(key, ageSecretKeySuffix) {
			continue
		}

		recipient, identity, err := deriveAgeRecipientFromSecretEntry(key, string(value))
		if err != nil {
			return nil, nil, err
		}
		recipients = append(recipients, recipient)
		identities = append(identities, identity)
	}

	return dedupeAndSortRecipients(recipients), dedupeAndSortIdentities(identities), nil
}

func normalizePublicAgeRecipients(publicKeys []string) ([]string, error) {
	if len(publicKeys) == 0 {
		return nil, nil
	}

	recipients := make([]string, 0, len(publicKeys))
	for i, publicKey := range publicKeys {
		trimmed := strings.TrimSpace(publicKey)
		if trimmed == "" {
			continue
		}

		recipient, err := age.ParseX25519Recipient(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid age recipient in age.recipients.publicKeys[%d]: %w", i, err)
		}
		recipients = append(recipients, recipient.String())
	}

	return dedupeAndSortRecipients(recipients), nil
}

func deriveAgeRecipientFromSecretEntry(secretKey, secretValue string) (string, string, error) {
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
		return "", "", fmt.Errorf("%s must contain one AGE-SECRET-KEY identity", secretKey)
	}
	if len(identities) > 1 {
		return "", "", fmt.Errorf("%s must contain exactly one AGE-SECRET-KEY identity", secretKey)
	}
	if !strings.HasPrefix(identities[0], "AGE-SECRET-KEY-") {
		return "", "", fmt.Errorf("%s must contain AGE-SECRET-KEY identity", secretKey)
	}

	identity, err := age.ParseX25519Identity(identities[0])
	if err != nil {
		return "", "", fmt.Errorf("invalid %s identity: %w", secretKey, err)
	}

	return identity.Recipient().String(), identity.String(), nil
}

func dedupeAndSortRecipients(recipients []string) []string {
	if len(recipients) == 0 {
		return nil
	}

	uniq := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		trimmed := strings.TrimSpace(recipient)
		if trimmed == "" {
			continue
		}
		uniq[trimmed] = struct{}{}
	}

	if len(uniq) == 0 {
		return nil
	}

	result := make([]string, 0, len(uniq))
	for recipient := range uniq {
		result = append(result, recipient)
	}
	sort.Strings(result)
	return result
}

func dedupeAndSortIdentities(identities []string) []string {
	if len(identities) == 0 {
		return nil
	}

	uniq := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		trimmed := strings.TrimSpace(identity)
		if trimmed == "" {
			continue
		}
		uniq[trimmed] = struct{}{}
	}

	if len(uniq) == 0 {
		return nil
	}

	result := make([]string, 0, len(uniq))
	for identity := range uniq {
		result = append(result, identity)
	}
	sort.Strings(result)
	return result
}
