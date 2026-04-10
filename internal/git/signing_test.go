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
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"golang.org/x/crypto/ssh"
)

func TestLoadSSHCommitSigner_ProducesVerifiableSSHSig(t *testing.T) {
	privateKey, publicKey, err := GenerateSSHSigningKeyPair(nil)
	require.NoError(t, err)

	secret := &corev1.Secret{
		Data: map[string][]byte{
			signingKeyDataKey: privateKey,
		},
	}

	signer, err := LoadSSHCommitSigner(secret)
	require.NoError(t, err)

	message := []byte("tree deadbeef\nauthor Test <test@example.com> 1 +0000\n\nsigned commit\n")
	signature, err := signer.Sign(bytes.NewReader(message))
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
	assert.Equal(t, sshSignatureNamespace, parsed.namespace)
	assert.Equal(t, sshSignatureHashAlg, parsed.hashAlgorithm)

	digest := sha512.Sum512(message)
	toVerify := ssh.Marshal(sshsigSignedData{
		Magic:         []byte(sshSignatureMagic),
		Namespace:     parsed.namespace,
		Reserved:      parsed.reserved,
		HashAlgorithm: parsed.hashAlgorithm,
		Hash:          digest[:],
	})
	require.NoError(t, parsedPublicKey.Verify(toVerify, parsed.signature))
}

func TestLoadSSHCommitSigner_InvalidPrivateKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-signing-key",
			Namespace: "default",
		},
		Data: map[string][]byte{
			signingKeyDataKey: []byte("not-a-private-key"),
		},
	}

	_, err := LoadSSHCommitSigner(secret)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse signing key")
}

func TestLoadSSHCommitSigner_PassphraseProtectedKey(t *testing.T) {
	passphrase := []byte("top-secret")
	privateKey, _, err := GenerateSSHSigningKeyPair(passphrase)
	require.NoError(t, err)

	secret := &corev1.Secret{
		Data: map[string][]byte{
			signingKeyDataKey:        privateKey,
			signingPassphraseDataKey: passphrase,
		},
	}

	signer, err := LoadSSHCommitSigner(secret)
	require.NoError(t, err)

	signature, err := signer.Sign(bytes.NewReader([]byte("example commit payload")))
	require.NoError(t, err)
	assert.Contains(t, string(signature), "BEGIN SSH SIGNATURE")

	publicKey, err := SSHAuthorizedPublicKeyFromSecret(secret)
	require.NoError(t, err)
	assert.Contains(t, publicKey, "ssh-ed25519 ")
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
	magic := make([]byte, len(sshSignatureMagic))
	_, err := io.ReadFull(reader, magic)
	require.NoError(t, err)
	require.Equal(t, sshSignatureMagic, string(magic))

	var version uint32
	require.NoError(t, binary.Read(reader, binary.BigEndian, &version))
	require.Equal(t, sshSignatureVersion, version)

	publicKey := readSSHPacketStringForTest(t, reader)
	namespace := string(readSSHPacketStringForTest(t, reader))
	reserved := string(readSSHPacketStringForTest(t, reader))
	hashAlgorithm := string(readSSHPacketStringForTest(t, reader))
	signatureBlob := readSSHPacketStringForTest(t, reader)

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

func readSSHPacketStringForTest(t *testing.T, reader io.Reader) []byte {
	t.Helper()

	var length uint32
	require.NoError(t, binary.Read(reader, binary.BigEndian, &length))

	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	require.NoError(t, err)

	return data
}
