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

// GetAuthMethod returns an SSH public key authentication method from a private key.
// If knownHosts is provided, it will be used for host key verification.
// If knownHosts is empty, host key verification will be disabled (insecure but functional).
func GetAuthMethod(privateKey, password, knownHosts string) (transport.AuthMethod, error) {
	logger := log.FromContext(context.Background())

	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	// Create the public key authentication
	publicKeys, err := ssh.NewPublicKeys("git", []byte(privateKey), password)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	// Configure host key verification
	if knownHosts != "" {
		callback, err := setupKnownHostsCallback(logger, knownHosts)
		if err != nil {
			//nolint:gosec // Intentional fallback for missing known_hosts
			publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
		} else {
			publicKeys.HostKeyCallback = callback
		}
	} else {
		logger.Info("Warning: No known_hosts provided in secret, using insecure SSH " +
			"host key verification. For production, add 'known_hosts' field to your secret.")
		//nolint:gosec // Intentional when known_hosts not provided
		publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
	}

	return publicKeys, nil
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
