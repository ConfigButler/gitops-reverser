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
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"

	"github.com/ConfigButler/gitops-reverser/internal/sshsig"
)

const (
	// SigningKeyDataKey is the Secret data key for the PEM-encoded SSH private signing key.
	SigningKeyDataKey = "signing.key"
	// SigningPublicKeyDataKey is the Secret data key for the authorized_keys-format public key.
	SigningPublicKeyDataKey = "signing.pub"
	// SigningPassphraseDataKey is the Secret data key for an optional key passphrase.
	SigningPassphraseDataKey = "passphrase"

	// Unexported aliases for internal use within this package.
	signingKeyDataKey        = SigningKeyDataKey
	signingPublicKeyDataKey  = SigningPublicKeyDataKey
	signingPassphraseDataKey = SigningPassphraseDataKey
	sshSignatureNamespace    = "git"
)

type sshCommitSigner struct {
	signer ssh.Signer
}

// LoadSSHCommitSigner loads a git-compatible SSH signer from the provided Secret.
func LoadSSHCommitSigner(secret *corev1.Secret) (gogit.Signer, error) {
	signer, err := loadSSHSigner(secret)
	if err != nil {
		return nil, err
	}

	return &sshCommitSigner{signer: signer}, nil
}

// SSHAuthorizedPublicKeyFromSecret derives the authorized_keys-form public key from a signing Secret.
func SSHAuthorizedPublicKeyFromSecret(secret *corev1.Secret) (string, error) {
	signer, err := loadSSHSigner(secret)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// GenerateSSHSigningKeyPair creates an ed25519 SSH signing keypair.
func GenerateSSHSigningKeyPair(passphrase []byte) ([]byte, []byte, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 signing key: %w", err)
	}

	var block *pem.Block
	if len(passphrase) > 0 {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(privateKey, "", passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(privateKey, "")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("marshal signing private key: %w", err)
	}

	privateKeyPEM := pem.EncodeToMemory(block)
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		return nil, nil, fmt.Errorf("derive signing public key: %w", err)
	}

	return privateKeyPEM, bytes.TrimSpace(ssh.MarshalAuthorizedKey(publicKey)), nil
}

func loadSSHSigner(secret *corev1.Secret) (ssh.Signer, error) {
	if secret == nil {
		return nil, errors.New("signing secret is nil")
	}

	privateKey, ok := secret.Data[signingKeyDataKey]
	if !ok || len(privateKey) == 0 {
		return nil, fmt.Errorf("signing secret %s/%s is missing %q", secret.Namespace, secret.Name, signingKeyDataKey)
	}

	if passphrase := secret.Data[signingPassphraseDataKey]; len(passphrase) > 0 {
		signer, err := sshsig.ParsePrivateKey(privateKey, passphrase)
		if err != nil {
			return nil, fmt.Errorf("parse passphrase-protected signing key from secret %s/%s: %w",
				secret.Namespace, secret.Name, err)
		}
		return signer, nil
	}

	signer, err := sshsig.ParsePrivateKey(privateKey, nil)
	if err != nil {
		return nil, fmt.Errorf("parse signing key from secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return signer, nil
}

func (s *sshCommitSigner) Sign(message io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(message)
	if err != nil {
		return nil, fmt.Errorf("read commit payload: %w", err)
	}

	signature, err := sshsig.SignMessage(s.signer, sshSignatureNamespace, payload)
	if err != nil {
		return nil, fmt.Errorf("sign commit payload: %w", err)
	}

	return signature, nil
}
