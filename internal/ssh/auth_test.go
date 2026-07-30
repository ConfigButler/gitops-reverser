// SPDX-License-Identifier: Apache-2.0

package ssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// TestMain initializes the controller-runtime logger before running tests.
// This prevents "log.SetLogger(...) was never called" warnings when code uses log.FromContext().
func TestMain(m *testing.M) {
	// Initialize controller-runtime logger for all tests
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Run all tests
	os.Exit(m.Run())
}

// testSSHPrivateKey is a test RSA private key (not for production use).
const testSSHPrivateKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAyVSi0fDJKlQnPCXjY9QGFmPuLbVFLPwuFP3KBYLTrQZ0M5n5
-----END RSA PRIVATE KEY-----`

func TestGetAuthMethod(t *testing.T) {
	privateKey := testSSHPrivateKey
	auth, err := GetAuthMethod(privateKey, "", "", false)

	// Expect error because test key is truncated/invalid
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "failed to create SSH public keys")
}

func TestGetAuthMethod_WithKnownHosts(t *testing.T) {
	privateKey := testSSHPrivateKey
	knownHosts := "github.com ssh-rsa AAAAB3NzaC1yc2EAAAABIwAAAQEAq2A7hRGmdnm9tUDbO9IDSwBK6TbQa+PXYPCPy6rbTrTtw7PHkccKrpp0yVhp5HdEIcKr6pLlVDBfOLX9QUsyCOV0wzfjIJNlGEYsdlLJizHhbn2mUjvSAHQqZETYP03HR+xYPVY/wDHEL0w1vXw1g7VQAN+5SZG1yQ+Qr2lnJbj5+6zP+Yr5s6CJXZ1F4OG8E7eHdOd5MFBjv9D9rLJvQjk5FVMzqZ+mZJ+W8Xj5MQP6vYzZh7cC9qPqJ8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8"

	auth, err := GetAuthMethod(privateKey, "", knownHosts, false)
	// Expect error because test key is truncated/invalid
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "failed to create SSH public keys")
}

func TestGetAuthMethod_InvalidKey(t *testing.T) {
	invalidKey := "this-is-not-a-valid-ssh-key"

	auth, err := GetAuthMethod(invalidKey, "", "", false)
	require.Error(t, err)
	assert.Nil(t, auth)
}

func TestGetAuthMethod_EmptyKey(t *testing.T) {
	auth, err := GetAuthMethod("", "", "", false)
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "private key cannot be empty")
}

// generateTestSSHKey returns a valid PEM-encoded RSA private key and a matching known_hosts line.
func generateTestSSHKey(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pub, err := gossh.NewPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return string(pemBytes), knownhosts.Line([]string{"example.com"}, pub)
}

func TestGetAuthMethod_FailsClosedWithoutKnownHosts(t *testing.T) {
	privateKey, _ := generateTestSSHKey(t)

	auth, err := GetAuthMethod(privateKey, "", "", false)
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "known_hosts is required")
}

func TestGetAuthMethod_InsecureOptOutWithoutKnownHosts(t *testing.T) {
	privateKey, _ := generateTestSSHKey(t)

	auth, err := GetAuthMethod(privateKey, "", "", true)
	require.NoError(t, err)
	assert.NotNil(t, auth)
}

func TestGetAuthMethod_WithValidKnownHosts(t *testing.T) {
	privateKey, knownHosts := generateTestSSHKey(t)

	auth, err := GetAuthMethod(privateKey, "", knownHosts, false)
	require.NoError(t, err)
	assert.NotNil(t, auth)
}

// A known_hosts that is present but unparseable is a hard error regardless of the missing-key
// opt-out: a declared host key must be valid. The opt-out only ever covers the no-key-at-all case.
func TestGetAuthMethod_UnparseableKnownHostsIsHardErrorEvenWithOptOut(t *testing.T) {
	privateKey, _ := generateTestSSHKey(t)

	auth, err := GetAuthMethod(privateKey, "", "this is not a valid known_hosts line", true)
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "failed to parse known_hosts")
}

// TestKeyAuth_AlwaysSetsHostKeyAlgorithms pins the fix for a go-git v6 behaviour that broke every
// SSH remote in the controller image.
//
// v6's SSH transport reads the process's default known_hosts files whenever ClientConfig returns an
// empty HostKeyAlgorithms — even when a HostKeyCallback was supplied — purely to derive the algorithm
// list, and fails the connection with "unable to find any valid known_hosts file" when neither
// ~/.ssh/known_hosts nor /etc/ssh/ssh_known_hosts exists. The controller runs distroless with
// neither, so the e2e SSH spec failed against a real Gitea while every unit test passed: the
// fallback lives in the transport's connect, not in ClientConfig.
//
// Both host-key policies must therefore yield a non-empty algorithm list.
func TestKeyAuth_AlwaysSetsHostKeyAlgorithms(t *testing.T) {
	privateKey, knownHostsLine := generateTestSSHKey(t)
	u, err := url.Parse("ssh://git@example.com/org/repo.git")
	require.NoError(t, err)
	req := &transport.Request{URL: u}

	t.Run("with a pinned known_hosts the algorithms come from the pin", func(t *testing.T) {
		auth, err := NewPublicKeyAuth(privateKey, "", knownHostsLine, false)
		require.NoError(t, err)

		cfg, err := auth.ClientConfig(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, cfg.HostKeyAlgorithms,
			"an empty list sends go-git to the default known_hosts files, which do not exist in the image")
		assert.Contains(t, cfg.HostKeyAlgorithms, gossh.KeyAlgoRSASHA256,
			"the pinned RSA key's algorithms must be offered")
	})

	t.Run("with host key verification disabled the modern set is offered", func(t *testing.T) {
		auth, err := NewPublicKeyAuth(privateKey, "", "", true)
		require.NoError(t, err)

		cfg, err := auth.ClientConfig(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, defaultHostKeyAlgorithms(), cfg.HostKeyAlgorithms)
		require.NotNil(t, cfg.HostKeyCallback)
	})
}
