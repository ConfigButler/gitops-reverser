// SPDX-License-Identifier: Apache-2.0

// Package ssh provides SSH authentication utilities for Git operations.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-logr/logr"
	gossh "golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// InsecureAllowMissingKnownHostsFlag is the controller flag, surfaced in error text, that opts
// out of SSH host key verification when no host-key source produced any known_hosts at all.
const InsecureAllowMissingKnownHostsFlag = "--insecure-allow-missing-known-hosts"

// GetAuthMethod returns an SSH public key authentication method from a private key.
//
// Host key verification fails closed: a known_hosts source is required. A known_hosts value that
// is present but cannot be parsed is always a hard error — if a host key is declared it must be
// valid. When no known_hosts is available at all, GetAuthMethod returns an error unless
// allowMissingKnownHosts is set (the controller's --insecure-allow-missing-known-hosts flag),
// which disables host key verification and is intended for throwaway/dev clusters only.
func GetAuthMethod(privateKey, password, knownHosts string, allowMissingKnownHosts bool) (transport.AuthMethod, error) {
	logger := log.FromContext(context.Background())

	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	// Create the public key authentication
	publicKeys, err := ssh.NewPublicKeys("git", []byte(privateKey), password)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	if knownHosts != "" {
		// A declared host key must parse: this is a hard error regardless of the
		// allow-missing opt-out, which only ever covers the no-key-at-all case.
		callback, err := setupKnownHostsCallback(logger, knownHosts)
		if err != nil {
			return nil, fmt.Errorf("failed to parse known_hosts for SSH host key verification: %w", err)
		}
		publicKeys.HostKeyCallback = callback
		return publicKeys, nil
	}

	if !allowMissingKnownHosts {
		return nil, errors.New(
			"known_hosts is required for SSH host key verification: add a 'known_hosts' entry to the " +
				"Git credentials Secret, point spec.knownHostsRef at a ConfigMap/Secret, or configure an " +
				"install-level default known-hosts ConfigMap; set '" + InsecureAllowMissingKnownHostsFlag +
				"' on the controller for throwaway/dev clusters only",
		)
	}
	logInsecureHostKey(logger, "no known_hosts provided")
	//nolint:gosec // explicit development opt-out via --insecure-allow-missing-known-hosts
	publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
	return publicKeys, nil
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

	callback, err := ssh.NewKnownHostsCallback(tmpFile.Name())
	if err != nil {
		logger.Info("Warning: failed to parse known_hosts", "error", err)
		return nil, err
	}

	logger.V(1).Info("Using known_hosts for SSH host key verification")
	return callback, nil
}
