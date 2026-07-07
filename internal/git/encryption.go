// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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
	// ageSecretKeySuffix identifies Flux-compatible age private-key entries. The write path
	// reads these only to derive the public recipient; the private identity is never kept.
	ageSecretKeySuffix = ".agekey"
)

// ResolvedEncryptionConfig contains runtime encryption settings resolved from GitTarget spec.
//
// It carries public age recipients only. The write path encrypts, it never decrypts, so no
// private age identity is retained, written to disk, or passed to the sops process. See
// docs/future/secret-value-retention-plan.md.
type ResolvedEncryptionConfig struct {
	Provider      string
	AgeRecipients []string
}

// ResolveTargetEncryption resolves and validates GitTarget encryption configuration.
func ResolveTargetEncryption(
	ctx context.Context,
	k8sClient client.Client,
	target *v1alpha3.GitTarget,
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
	secretRecipients, err := resolveSecretRecipients(ctx, k8sClient, target, encryptionSpec)
	if err != nil {
		return nil, err
	}

	resolvedRecipients := dedupeAndSortRecipients(append(publicRecipients, secretRecipients...))
	if len(resolvedRecipients) == 0 {
		return nil, errors.New(
			"encryption.age.enabled=true requires at least one resolved recipient from age.recipients.publicKeys or secret *.agekey entries",
		)
	}

	return &ResolvedEncryptionConfig{
		Provider:      providerName,
		AgeRecipients: resolvedRecipients,
	}, nil
}

func resolveEncryptionProvider(encryptionSpec *v1alpha3.EncryptionSpec) (string, error) {
	providerName := strings.TrimSpace(encryptionSpec.Provider)
	if providerName == "" {
		providerName = EncryptionProviderSOPS
	}
	if providerName != EncryptionProviderSOPS {
		return "", fmt.Errorf("unsupported encryption provider %q", encryptionSpec.Provider)
	}
	return providerName, nil
}

// resolveSecretRecipients reads the referenced age-key Secret and derives the public age
// recipients from its *.agekey entries. It intentionally derives recipients only: the private
// identities and the rest of the Secret data are never returned to the caller, so they cannot
// reach the sops process or disk.
func resolveSecretRecipients(
	ctx context.Context,
	k8sClient client.Client,
	target *v1alpha3.GitTarget,
	encryptionSpec *v1alpha3.EncryptionSpec,
) ([]string, error) {
	if encryptionSpec.Age == nil || !encryptionSpec.Age.Recipients.ExtractFromSecret {
		return nil, nil
	}

	secretKind := strings.TrimSpace(encryptionSpec.SecretRef.Kind)
	if secretKind != "" && secretKind != "Secret" {
		return nil, fmt.Errorf(
			"encryption.secretRef.kind must be Secret, got %q",
			encryptionSpec.SecretRef.Kind,
		)
	}

	secretName := strings.TrimSpace(encryptionSpec.SecretRef.Name)
	if secretName == "" {
		return nil, errors.New(
			"encryption.secretRef.name must be set when age.recipients.extractFromSecret=true",
		)
	}

	secret, secretKey, err := getEncryptionSecret(ctx, k8sClient, target.Namespace, secretName)
	if err != nil {
		return nil, err
	}

	secretRecipients, err := resolveAgeRecipientsFromSecret(secret.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve recipients from encryption secret %s: %w", secretKey, err)
	}

	return secretRecipients, nil
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

func configureSecretEncryptionWriter(
	writer *contentWriter,
	workDir string,
	cfg *ResolvedEncryptionConfig,
) error {
	if cfg == nil {
		writer.setEncryptor(nil, "")
		return nil
	}

	switch cfg.Provider {
	case EncryptionProviderSOPS:
		// No environment is passed to sops: age recipients are published to the target's
		// .sops.yaml at bootstrap, and the write path only encrypts, so it needs neither a
		// private-key file (SOPS_AGE_KEY_FILE) nor blanket Secret data in the process env.
		scope := secretEncryptionCacheScope(workDir, cfg)
		writer.setEncryptor(NewSOPSEncryptorWithEnv(defaultSOPSBinaryPath, "", workDir, nil), scope)
		return nil
	default:
		return fmt.Errorf("unsupported encryption provider %q", cfg.Provider)
	}
}

func secretEncryptionCacheScope(workDir string, cfg *ResolvedEncryptionConfig) string {
	if cfg == nil {
		return ""
	}

	hasher := sha256.New()
	hasher.Write([]byte(strings.TrimSpace(cfg.Provider)))
	hasher.Write([]byte{0})
	hasher.Write([]byte(strings.TrimSpace(workDir)))
	hasher.Write([]byte{0})

	for _, recipient := range cfg.AgeRecipients {
		hasher.Write([]byte(strings.TrimSpace(recipient)))
		hasher.Write([]byte{0})
	}

	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func resolveAgeRecipientsFromSecret(secretData map[string][]byte) ([]string, error) {
	if len(secretData) == 0 {
		return nil, nil
	}

	recipients := make([]string, 0, len(secretData))
	for key, value := range secretData {
		if !strings.HasSuffix(key, ageSecretKeySuffix) {
			continue
		}

		recipient, err := deriveAgeRecipientFromSecretEntry(key, string(value))
		if err != nil {
			return nil, err
		}
		recipients = append(recipients, recipient)
	}

	return dedupeAndSortRecipients(recipients), nil
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

// deriveAgeRecipientFromSecretEntry parses a single AGE-SECRET-KEY identity from a *.agekey
// Secret entry and returns only its public recipient. The parsed private identity is used
// solely to derive the recipient and is never returned, stored, or written to disk.
func deriveAgeRecipientFromSecretEntry(secretKey, secretValue string) (string, error) {
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
		return "", fmt.Errorf("%s must contain one AGE-SECRET-KEY identity", secretKey)
	}
	if len(identities) > 1 {
		return "", fmt.Errorf("%s must contain exactly one AGE-SECRET-KEY identity", secretKey)
	}
	if !strings.HasPrefix(identities[0], "AGE-SECRET-KEY-") {
		return "", fmt.Errorf("%s must contain AGE-SECRET-KEY identity", secretKey)
	}

	identity, err := age.ParseX25519Identity(identities[0])
	if err != nil {
		return "", fmt.Errorf("invalid %s identity: %w", secretKey, err)
	}

	return identity.Recipient().String(), nil
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
