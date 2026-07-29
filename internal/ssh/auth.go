// SPDX-License-Identifier: Apache-2.0

// Package ssh provides SSH authentication utilities for Git operations.
package ssh

import (
	"context"
	"crypto"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-logr/logr"
	gossh "golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// KeyAuth wraps go-git's SSH public key auth and carries the raw material
// needed by the system-git fallback (unencrypted private key PEM and known_hosts text).
// It satisfies transport.AuthMethod via the embedded *gogitssh.PublicKeys so the
// go-git path uses it transparently; the system-git path type-asserts to *KeyAuth.
type KeyAuth struct {
	*gogitssh.PublicKeys

	PrivateKeyPEM []byte // always unencrypted; written to a 0600 temp file for system git
	KnownHosts    string // raw known_hosts content; empty means no pinning (accept-new)
}

// NewSSHKeyAuth constructs a KeyAuth from a PEM private key, optional passphrase, and
// optional known_hosts content. It validates and sets up host key verification on the embedded
// PublicKeys (same policy as GetAuthMethod), and also decrypts and re-serialises the private
// key without a passphrase so the system-git subprocess can use it without an ssh-agent.
func NewSSHKeyAuth(privateKey, password, knownHosts string, allowMissingKnownHosts bool) (*KeyAuth, error) {
	logger := log.FromContext(context.Background())

	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	publicKeys, err := gogitssh.NewPublicKeys("git", []byte(privateKey), password)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	switch {
	case knownHosts != "":
		callback, err := setupKnownHostsCallback(logger, knownHosts)
		if err != nil {
			return nil, fmt.Errorf("failed to parse known_hosts for SSH host key verification: %w", err)
		}
		publicKeys.HostKeyCallback = callback
	case !allowMissingKnownHosts:
		return nil, errors.New(
			"known_hosts is required for SSH host key verification: add a 'known_hosts' entry to the " +
				"Git credentials Secret, point spec.knownHostsRef at a ConfigMap/Secret, or configure an " +
				"install-level default known-hosts ConfigMap; set '" + InsecureAllowMissingKnownHostsFlag +
				"' on the controller for throwaway/dev clusters only",
		)
	default:
		logInsecureHostKey(logger, "no known_hosts provided")
		//nolint:gosec // explicit development opt-out via --insecure-allow-missing-known-hosts
		publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
	}

	// Attempt to re-serialise the key without a passphrase for the system-git
	// ADO fallback. This fails for unsupported types (e.g. DSA) but that must
	// not break non-ADO SSH providers — defer the error to newSystemGitEnv,
	// which is the only caller that actually needs PrivateKeyPEM.
	unencryptedPEM, _ := decryptPrivateKeyPEM([]byte(privateKey), []byte(password))

	return &KeyAuth{
		PublicKeys:    publicKeys,
		PrivateKeyPEM: unencryptedPEM, // nil when key type is unsupported by MarshalPrivateKey
		KnownHosts:    knownHosts,
	}, nil
}

// decryptPrivateKeyPEM parses the PEM private key (with optional passphrase) and
// re-serialises it without a passphrase. This lets the system-git subprocess load
// the key via -i without needing an ssh-agent or interactive prompt.
func decryptPrivateKeyPEM(pemBytes, passphrase []byte) ([]byte, error) {
	var rawKey interface{}
	var err error
	if len(passphrase) > 0 {
		rawKey, err = gossh.ParseRawPrivateKeyWithPassphrase(pemBytes, passphrase)
	} else {
		rawKey, err = gossh.ParseRawPrivateKey(pemBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}
	privKey, ok := rawKey.(crypto.PrivateKey)
	if !ok {
		return nil, errors.New("parsed SSH key does not implement crypto.PrivateKey")
	}
	block, err := gossh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, fmt.Errorf("marshal unencrypted SSH private key: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

// InsecureAllowMissingKnownHostsFlag is the controller flag, surfaced in error text, that opts
// out of SSH host key verification when no host-key source produced any known_hosts at all.
const InsecureAllowMissingKnownHostsFlag = "--insecure-allow-missing-known-hosts"

// GetAuthMethod returns an SSH public key authentication method from a private key.
// It delegates to NewSSHKeyAuth and returns the result as transport.AuthMethod.
// The concrete type is *KeyAuth, which the system-git ADO fallback can type-assert
// to access the raw private key bytes and known_hosts needed to invoke system git.
func GetAuthMethod(privateKey, password, knownHosts string, allowMissingKnownHosts bool) (transport.AuthMethod, error) {
	return NewSSHKeyAuth(privateKey, password, knownHosts, allowMissingKnownHosts)
}

// logInsecureHostKey emits a loud warning whenever SSH host key verification is disabled.
func logInsecureHostKey(logger logr.Logger, reason string) {
	logger.Info("INSECURE: SSH host key verification disabled via "+InsecureAllowMissingKnownHostsFlag+
		"; do not use in production", "reason", reason)
}

// setupKnownHostsCallback creates a host key callback from known_hosts content.
func setupKnownHostsCallback(logger logr.Logger, knownHosts string) (gossh.HostKeyCallback, error) {
	tmpFile, err := os.CreateTemp("", "known_hosts_*")
	if err != nil {
		logger.Info("Warning: failed to create temp known_hosts file", "error", err)
		return nil, err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(knownHosts); err != nil {
		logger.Info("Warning: failed to write known_hosts", "error", err)
		return nil, err
	}

	if err := tmpFile.Close(); err != nil {
		logger.Info("Warning: failed to close temp file", "error", err)
		return nil, err
	}

	callback, err := gogitssh.NewKnownHostsCallback(tmpFile.Name())
	if err != nil {
		logger.Info("Warning: failed to parse known_hosts", "error", err)
		return nil, err
	}

	logger.V(1).Info("Using known_hosts for SSH host key verification")
	return callback, nil
}
