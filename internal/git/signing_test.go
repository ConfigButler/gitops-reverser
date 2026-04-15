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
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"golang.org/x/crypto/ssh"
)

const (
	testSSHSigMagic         = "SSHSIG"
	testSSHSigVersion       = uint32(1)
	testSSHSigHashAlgSHA512 = "sha512"
)

// buildSSHSigSignedData constructs the signed data blob as OpenSSH actually
// implements it (verified against OpenSSH 9.x sshsig.c). Despite PROTOCOL.sshsig
// listing SIG_VERSION in the signed-data section, the real implementation omits
// it — version appears only in the outer blob header. Signed data is:
//
//	"SSHSIG" (6 raw bytes) || string(namespace)
//	|| string(reserved) || string(hash_algorithm) || string(hash)
func buildSSHSigSignedData(namespace, reserved, hashAlgorithm string, hash []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(testSSHSigMagic)
	mustWriteSSHPacketStringForTest(&buf, []byte(namespace))
	mustWriteSSHPacketStringForTest(&buf, []byte(reserved))
	mustWriteSSHPacketStringForTest(&buf, []byte(hashAlgorithm))
	mustWriteSSHPacketStringForTest(&buf, hash)
	return buf.Bytes()
}

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
	assert.Equal(t, testSSHSigHashAlgSHA512, parsed.hashAlgorithm)

	digest := sha512.Sum512(message)
	toVerify := buildSSHSigSignedData(parsed.namespace, parsed.reserved, parsed.hashAlgorithm, digest[:])
	require.NoError(t, parsedPublicKey.Verify(toVerify, parsed.signature))
}

// TestLoadSSHCommitSigner_SSHKeygenVerify signs a synthetic commit payload and
// verifies the resulting signature with the real `ssh-keygen -Y verify` binary.
// This exercises the full SSHSIG wire format (blob encoding + signed-data framing),
// not just the cryptographic operation, so it catches protocol-level regressions.
func TestLoadSSHCommitSigner_SSHKeygenVerify(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found in PATH")
	}

	privateKey, publicKey, err := GenerateSSHSigningKeyPair(nil)
	require.NoError(t, err)

	signer, err := LoadSSHCommitSigner(&corev1.Secret{
		Data: map[string][]byte{signingKeyDataKey: privateKey},
	})
	require.NoError(t, err)

	const identity = "test@example.com"
	payload := []byte("tree deadbeef\nauthor Test <test@example.com> 1 +0000\n\nsigned commit\n")

	sig, err := signer.Sign(bytes.NewReader(payload))
	require.NoError(t, err)

	tmpDir := t.TempDir()

	allowedSigners := fmt.Sprintf("%s namespaces=\"git\" %s\n", identity, publicKey)
	allowedSignersFile := tmpDir + "/allowed-signers"
	require.NoError(t, os.WriteFile(allowedSignersFile, []byte(allowedSigners), 0o600))

	sigFile := tmpDir + "/commit.sig"
	require.NoError(t, os.WriteFile(sigFile, sig, 0o600))

	// Cross-check: use ssh-keygen itself to sign the same payload with the same
	// private key and verify that it passes -Y verify. If this fails, the test
	// setup (allowed-signers format, identity, namespace) is wrong. If it passes
	// but our signature below fails, the bug is in our SSHSIG encoding.
	//
	// ssh-keygen -Y sign writes <file>.sig next to the input file; feed it a file.
	privKeyFile := tmpDir + "/id_ed25519"
	require.NoError(t, os.WriteFile(privKeyFile, privateKey, 0o600))
	payloadFile := tmpDir + "/payload"
	require.NoError(t, os.WriteFile(payloadFile, payload, 0o600))

	signCmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", privKeyFile, "-n", "git", payloadFile)
	signOut, signErr := signCmd.CombinedOutput()
	require.NoError(t, signErr, "ssh-keygen -Y sign (reference) failed:\n%s", signOut)
	refSigFile := payloadFile + ".sig"

	verifyRef := exec.Command("ssh-keygen", "-Y", "verify",
		"-f", allowedSignersFile, "-I", identity, "-n", "git", "-s", refSigFile)
	verifyRef.Stdin = bytes.NewReader(payload)
	verifyRefOut, verifyRefErr := verifyRef.CombinedOutput()
	require.NoError(t, verifyRefErr,
		"ssh-keygen -Y verify failed for reference (ssh-keygen-signed) sig — test setup is broken:\n%s", verifyRefOut)

	// Now verify our own signature.
	cmd := exec.Command("ssh-keygen", "-Y", "verify",
		"-f", allowedSignersFile,
		"-I", identity,
		"-n", "git",
		"-s", sigFile,
	)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ssh-keygen -Y verify failed for our signature:\n%s", out)
	assert.Contains(t, string(out), "Good")
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
	magic := make([]byte, len(testSSHSigMagic))
	_, err := io.ReadFull(reader, magic)
	require.NoError(t, err)
	require.Equal(t, testSSHSigMagic, string(magic))

	var version uint32
	require.NoError(t, binary.Read(reader, binary.BigEndian, &version))
	require.Equal(t, testSSHSigVersion, version)

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

func mustWriteSSHPacketStringForTest(buf *bytes.Buffer, value []byte) {
	if len(value) > math.MaxUint32 {
		panic(errors.New("ssh packet string too large"))
	}

	_ = binary.Write(buf, binary.BigEndian, uint32(len(value)))
	_, _ = buf.Write(value)
}
