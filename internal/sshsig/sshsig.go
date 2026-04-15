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

package sshsig

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"golang.org/x/crypto/ssh"
)

const (
	hashAlgorithmSHA512 = "sha512"
	signatureVersion    = uint32(1)
	signatureMagic      = "SSHSIG"
)

// ParsePrivateKey loads an SSH signer from PEM-encoded private key data.
func ParsePrivateKey(privateKeyPEM, passphrase []byte) (ssh.Signer, error) {
	if len(privateKeyPEM) == 0 {
		return nil, errors.New("private key is empty")
	}

	if len(passphrase) > 0 {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(privateKeyPEM, passphrase)
		if err != nil {
			return nil, fmt.Errorf("parse passphrase-protected private key: %w", err)
		}
		return signer, nil
	}

	signer, err := ssh.ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}

// SignMessage signs message using the OpenSSH SSHSIG format for namespace.
func SignMessage(signer ssh.Signer, namespace string, message []byte) ([]byte, error) {
	if signer == nil {
		return nil, errors.New("ssh signer is nil")
	}
	if namespace == "" {
		return nil, errors.New("namespace is empty")
	}

	digest := sha512.Sum512(message)
	signedData, err := buildSignedData(namespace, nil, hashAlgorithmSHA512, digest[:])
	if err != nil {
		return nil, err
	}

	signature, err := signMessage(signer, signedData)
	if err != nil {
		return nil, fmt.Errorf("sign message: %w", err)
	}

	blob, err := encodeSignatureBlob(signer.PublicKey(), namespace, nil, hashAlgorithmSHA512, signature)
	if err != nil {
		return nil, err
	}

	return armorSignature(blob), nil
}

func buildSignedData(namespace string, reserved []byte, hashAlgorithm string, hash []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(signatureMagic)

	if err := writePacketString(&buf, []byte(namespace)); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, reserved); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, []byte(hashAlgorithm)); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, hash); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func signMessage(signer ssh.Signer, data []byte) (*ssh.Signature, error) {
	if algorithmSigner, ok := signer.(ssh.AlgorithmSigner); ok && signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		return algorithmSigner.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA512)
	}

	return signer.Sign(rand.Reader, data)
}

func encodeSignatureBlob(
	publicKey ssh.PublicKey,
	namespace string,
	reserved []byte,
	hashAlgorithm string,
	signature *ssh.Signature,
) ([]byte, error) {
	if publicKey == nil {
		return nil, errors.New("ssh signing public key is nil")
	}
	if signature == nil {
		return nil, errors.New("ssh signature is nil")
	}

	signatureBlob := ssh.Marshal(*signature)

	var buf bytes.Buffer
	buf.WriteString(signatureMagic)
	if err := binary.Write(&buf, binary.BigEndian, signatureVersion); err != nil {
		return nil, fmt.Errorf("encode ssh signature version: %w", err)
	}

	if err := writePacketString(&buf, publicKey.Marshal()); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, []byte(namespace)); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, reserved); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, []byte(hashAlgorithm)); err != nil {
		return nil, err
	}
	if err := writePacketString(&buf, signatureBlob); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func writePacketString(buf *bytes.Buffer, value []byte) error {
	if len(value) > math.MaxUint32 {
		return fmt.Errorf("ssh packet string too large: %d bytes", len(value))
	}

	//nolint:gosec // len(value) is bounded above by math.MaxUint32 just above.
	_ = binary.Write(buf, binary.BigEndian, uint32(len(value)))
	_, _ = buf.Write(value)
	return nil
}

func armorSignature(blob []byte) []byte {
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
