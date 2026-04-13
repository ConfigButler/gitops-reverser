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
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	corev1 "k8s.io/api/core/v1"

	"golang.org/x/crypto/ssh"
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

	sshSignatureNamespace = "git"
	sshSignatureHashAlg   = "sha512"
	sshSignatureVersion   = uint32(1)
	sshSignatureMagic     = "SSHSIG"
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
		signer, err := ssh.ParsePrivateKeyWithPassphrase(privateKey, passphrase)
		if err != nil {
			return nil, fmt.Errorf("parse passphrase-protected signing key from secret %s/%s: %w",
				secret.Namespace, secret.Name, err)
		}
		return signer, nil
	}

	signer, err := ssh.ParsePrivateKey(privateKey)
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

	digest := sha512.Sum512(payload)

	// Build the signed data blob per the actual OpenSSH sshsig implementation.
	// Despite PROTOCOL.sshsig stating that SIG_VERSION appears in the signed
	// data, OpenSSH 9.x does NOT include it — version appears only in the blob
	// header. The signed data is:
	//   "SSHSIG" (6 raw bytes) || string(namespace)
	//   || string(reserved) || string(hash_algorithm) || string(hash)
	// The magic must be written as raw bytes — NOT as an SSH wire-format
	// length-prefixed string — so ssh.Marshal must not be used here.
	var sigData bytes.Buffer
	sigData.WriteString(sshSignatureMagic)
	_ = writeSSHPacketString(&sigData, []byte(sshSignatureNamespace))
	_ = writeSSHPacketString(&sigData, nil) // reserved
	_ = writeSSHPacketString(&sigData, []byte(sshSignatureHashAlg))
	_ = writeSSHPacketString(&sigData, digest[:])

	signature, err := signSSHMessage(s.signer, sigData.Bytes())
	if err != nil {
		return nil, fmt.Errorf("sign commit payload: %w", err)
	}

	blob, err := encodeSSHSignatureBlob(s.signer.PublicKey(), signature)
	if err != nil {
		return nil, err
	}

	return armorSSHSignature(blob), nil
}

func signSSHMessage(signer ssh.Signer, data []byte) (*ssh.Signature, error) {
	if algorithmSigner, ok := signer.(ssh.AlgorithmSigner); ok && signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		return algorithmSigner.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA512)
	}

	return signer.Sign(rand.Reader, data)
}

func encodeSSHSignatureBlob(publicKey ssh.PublicKey, signature *ssh.Signature) ([]byte, error) {
	if publicKey == nil {
		return nil, errors.New("ssh signing public key is nil")
	}
	if signature == nil {
		return nil, errors.New("ssh signature is nil")
	}

	signatureBlob := ssh.Marshal(*signature)

	var buf bytes.Buffer
	buf.WriteString(sshSignatureMagic)
	if err := binary.Write(&buf, binary.BigEndian, sshSignatureVersion); err != nil {
		return nil, fmt.Errorf("encode ssh signature version: %w", err)
	}

	if err := writeSSHPacketString(&buf, publicKey.Marshal()); err != nil {
		return nil, err
	}
	if err := writeSSHPacketString(&buf, []byte(sshSignatureNamespace)); err != nil {
		return nil, err
	}
	if err := writeSSHPacketString(&buf, nil); err != nil {
		return nil, err
	}
	if err := writeSSHPacketString(&buf, []byte(sshSignatureHashAlg)); err != nil {
		return nil, err
	}
	if err := writeSSHPacketString(&buf, signatureBlob); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func writeSSHPacketString(buf *bytes.Buffer, value []byte) error {
	if len(value) > math.MaxUint32 {
		return fmt.Errorf("ssh packet string too large: %d bytes", len(value))
	}

	//nolint:gosec // len(value) is bounded above by math.MaxUint32 just above.
	_ = binary.Write(buf, binary.BigEndian, uint32(len(value)))
	_, _ = buf.Write(value)
	return nil
}

func armorSSHSignature(blob []byte) []byte {
	var buf bytes.Buffer
	_, _ = buf.WriteString("-----BEGIN SSH SIGNATURE-----\n")

	encoded := base64.StdEncoding.EncodeToString(blob)
	for len(encoded) > 70 {
		_, _ = buf.WriteString(encoded[:70])
		_ = buf.WriteByte('\n')
		encoded = encoded[70:]
	}
	if encoded != "" {
		_, _ = buf.WriteString(encoded)
		_ = buf.WriteByte('\n')
	}

	_, _ = buf.WriteString("-----END SSH SIGNATURE-----")
	return buf.Bytes()
}
