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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestSignMessage_ProducesVerifiableSSHSig(t *testing.T) {
	privateKeyPEM, publicKey := generateSSHSigningKeyPair(t, nil)

	signer, err := ParsePrivateKey(privateKeyPEM, nil)
	require.NoError(t, err)

	message := []byte("token-to-sign")
	signature, err := SignMessage(signer, "gitea", message)
	require.NoError(t, err)

	assert.Contains(t, string(signature), "-----BEGIN SSH SIGNATURE-----")
	assert.Contains(t, string(signature), "-----END SSH SIGNATURE-----")

	block, _ := pem.Decode(signature)
	require.NotNil(t, block)
	require.Equal(t, "SSH SIGNATURE", block.Type)

	parsed := parseSSHSigBlob(t, block.Bytes)
	parsedPublicKey, err := ssh.ParsePublicKey(parsed.publicKey)
	require.NoError(t, err)
	assert.Equal(t, string(publicKey), string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(parsedPublicKey))))
	assert.Equal(t, "gitea", parsed.namespace)
	assert.Equal(t, hashAlgorithmSHA512, parsed.hashAlgorithm)

	digest := sha512.Sum512(message)
	toVerify, err := buildSignedData(parsed.namespace, []byte(parsed.reserved), parsed.hashAlgorithm, digest[:])
	require.NoError(t, err)
	require.NoError(t, parsedPublicKey.Verify(toVerify, parsed.signature))
}

func TestSignMessage_SSHKeygenVerify(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found in PATH")
	}

	tests := []struct {
		name      string
		namespace string
		message   []byte
	}{
		{
			name:      "git namespace",
			namespace: "git",
			message:   []byte("tree deadbeef\nauthor Test <test@example.com> 1 +0000\n\nsigned commit\n"),
		},
		{
			name:      "gitea namespace",
			namespace: "gitea",
			message:   []byte("token-to-sign"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			privateKeyPEM, publicKey := generateSSHSigningKeyPair(t, nil)

			signer, err := ParsePrivateKey(privateKeyPEM, nil)
			require.NoError(t, err)

			signature, err := SignMessage(signer, tt.namespace, tt.message)
			require.NoError(t, err)

			tmpDir := t.TempDir()

			const identity = "test@example.com"
			allowedSigners := fmt.Sprintf("%s namespaces=\"%s\" %s\n", identity, tt.namespace, publicKey)
			allowedSignersFile := tmpDir + "/allowed-signers"
			require.NoError(t, os.WriteFile(allowedSignersFile, []byte(allowedSigners), 0o600))

			sigFile := tmpDir + "/message.sig"
			require.NoError(t, os.WriteFile(sigFile, signature, 0o600))

			cmd := exec.Command("ssh-keygen", "-Y", "verify",
				"-f", allowedSignersFile,
				"-I", identity,
				"-n", tt.namespace,
				"-s", sigFile,
			)
			cmd.Stdin = bytes.NewReader(tt.message)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "ssh-keygen -Y verify failed:\n%s", out)
			assert.Contains(t, string(out), "Good")
		})
	}
}

type parsedSSHSig struct {
	version       uint32
	publicKey     []byte
	namespace     string
	reserved      string
	hashAlgorithm string
	signature     *ssh.Signature
}

func parseSSHSigBlob(t *testing.T, blob []byte) parsedSSHSig {
	t.Helper()

	reader := bytes.NewReader(blob)
	magic := make([]byte, len(signatureMagic))
	_, err := io.ReadFull(reader, magic)
	require.NoError(t, err)
	require.Equal(t, signatureMagic, string(magic))

	var version uint32
	require.NoError(t, binary.Read(reader, binary.BigEndian, &version))
	require.Equal(t, signatureVersion, version)

	publicKey := readPacketStringForTest(t, reader)
	namespace := string(readPacketStringForTest(t, reader))
	reserved := string(readPacketStringForTest(t, reader))
	hashAlgorithm := string(readPacketStringForTest(t, reader))
	signatureBlob := readPacketStringForTest(t, reader)

	var signature ssh.Signature
	require.NoError(t, ssh.Unmarshal(signatureBlob, &signature))

	return parsedSSHSig{
		version:       version,
		publicKey:     publicKey,
		namespace:     namespace,
		reserved:      reserved,
		hashAlgorithm: hashAlgorithm,
		signature:     &signature,
	}
}

func readPacketStringForTest(t *testing.T, reader io.Reader) []byte {
	t.Helper()

	var length uint32
	require.NoError(t, binary.Read(reader, binary.BigEndian, &length))

	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	require.NoError(t, err)

	return data
}

func generateSSHSigningKeyPair(t *testing.T, passphrase []byte) ([]byte, []byte) {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	var block *pem.Block
	if len(passphrase) > 0 {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(privateKey, "", passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(privateKey, "")
	}
	require.NoError(t, err)

	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	require.NoError(t, err)

	return pem.EncodeToMemory(block), bytes.TrimSpace(ssh.MarshalAuthorizedKey(publicKey))
}
