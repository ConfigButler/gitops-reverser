// SPDX-License-Identifier: Apache-2.0

// Package ssh provides SSH authentication utilities for Git operations.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	gogitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/go-git/go-git/v6/plumbing/transport/ssh/knownhosts"
	"github.com/go-logr/logr"
	gossh "golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// InsecureAllowMissingKnownHostsFlag is the controller flag, surfaced in error text, that opts
// out of SSH host key verification when no host-key source produced any known_hosts at all.
const InsecureAllowMissingKnownHostsFlag = "--insecure-allow-missing-known-hosts"

// defaultHostKeyAlgorithms is offered when host key verification is disabled, so that go-git never
// has a reason to consult on-disk known_hosts files. See KeyAuth.ClientConfig.
func defaultHostKeyAlgorithms() []string {
	return []string{
		gossh.KeyAlgoED25519,
		gossh.CertAlgoED25519v01,
		gossh.KeyAlgoECDSA256, gossh.KeyAlgoECDSA384, gossh.KeyAlgoECDSA521,
		gossh.CertAlgoECDSA256v01, gossh.CertAlgoECDSA384v01, gossh.CertAlgoECDSA521v01,
		gossh.KeyAlgoRSASHA256, gossh.KeyAlgoRSASHA512, gossh.KeyAlgoRSA,
		gossh.CertAlgoRSASHA256v01, gossh.CertAlgoRSASHA512v01, gossh.CertAlgoRSAv01,
	}
}

// KeyAuth is SSH public key authentication that always states which host key algorithms it will
// accept.
//
// It exists to close a hole in go-git v6. Its SSH transport reads the process's default known_hosts
// files — `~/.ssh/known_hosts` and `/etc/ssh/ssh_known_hosts` — whenever ClientConfig comes back with
// an empty HostKeyAlgorithms, *even when a HostKeyCallback was supplied*, purely to derive the
// algorithm list (`plumbing/transport/ssh/ssh.go`, the `else if len(config.HostKeyAlgorithms) == 0`
// branch). If neither file exists it fails the connection with "unable to find any valid known_hosts
// file, set SSH_KNOWN_HOSTS env variable". The controller image is distroless with no home directory
// and no system known_hosts, so every SSH remote would fail there regardless of the credential —
// which is exactly what the e2e suite caught. v5 derived no algorithms and so never looked.
//
// Populating the field ourselves keeps that fallback unreachable. The algorithms are derived from the
// pinned known_hosts when we have one, matching git's own behaviour: offering an algorithm the pin
// does not cover would make the server present a key our callback then rejects.
type KeyAuth struct {
	*gogitssh.PublicKeys

	// db is built from the supplied known_hosts, or nil when host key verification is disabled.
	db *knownhosts.HostKeyDB
}

// NewPublicKeyAuth builds go-git's SSH public key authentication from a private key, applying this
// project's host-key policy.
//
// Host key verification fails closed: a known_hosts source is required. A known_hosts value that
// is present but cannot be parsed is always a hard error — if a host key is declared it must be
// valid. When no known_hosts is available at all, NewPublicKeyAuth returns an error unless
// allowMissingKnownHosts is set (the controller's --insecure-allow-missing-known-hosts flag),
// which disables host key verification and is intended for throwaway/dev clusters only.
func NewPublicKeyAuth(
	privateKey, password, knownHosts string,
	allowMissingKnownHosts bool,
) (*KeyAuth, error) {
	logger := log.FromContext(context.Background())

	if privateKey == "" {
		return nil, errors.New("private key cannot be empty")
	}

	// Create the public key authentication
	publicKeys, err := gogitssh.NewPublicKeys("git", []byte(privateKey), password)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	if knownHosts != "" {
		// A declared host key must parse: this is a hard error regardless of the
		// allow-missing opt-out, which only ever covers the no-key-at-all case.
		db, err := knownHostsDB(logger, knownHosts)
		if err != nil {
			return nil, fmt.Errorf("failed to parse known_hosts for SSH host key verification: %w", err)
		}
		publicKeys.HostKeyCallback = db.HostKeyCallback()
		return &KeyAuth{PublicKeys: publicKeys, db: db}, nil
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
	return &KeyAuth{PublicKeys: publicKeys}, nil
}

// ClientConfig implements gitclient.SSHAuth. It delegates to go-git for the credential and host key
// callback, then guarantees HostKeyAlgorithms is set.
func (a *KeyAuth) ClientConfig(ctx context.Context, req *transport.Request) (*gossh.ClientConfig, error) {
	cfg, err := a.PublicKeys.ClientConfig(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(cfg.HostKeyAlgorithms) > 0 {
		return cfg, nil
	}

	if a.db != nil {
		cfg.HostKeyAlgorithms = a.db.HostKeyAlgorithms(hostWithPort(req))
	}
	if len(cfg.HostKeyAlgorithms) == 0 {
		// No pin to narrow the list: offer the modern set. Verification is still the callback's job.
		cfg.HostKeyAlgorithms = defaultHostKeyAlgorithms()
	}

	return cfg, nil
}

// hostWithPort renders the request's host in the "host:port" form the known_hosts lookup expects,
// defaulting to the SSH port.
func hostWithPort(req *transport.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	port := req.URL.Port()
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(req.URL.Hostname(), port)
}

// GetAuthMethod returns SSH public key authentication as transport client options.
//
// go-git v6 removed the single transport.AuthMethod interface: authentication is supplied as
// functional options on the transport client (gitclient.WithSSHAuth / gitclient.WithHTTPAuth), so a
// credential travels as a []gitclient.Option and a nil slice means anonymous.
//
// The options are opaque closures, so callers cannot inspect what kind of credential they hold.
// NewPublicKeyAuth is the introspectable half, kept exported so the host-key policy can be asserted
// directly rather than through the option wrapper.
func GetAuthMethod(privateKey, password, knownHosts string, allowMissingKnownHosts bool) ([]gitclient.Option, error) {
	publicKeys, err := NewPublicKeyAuth(privateKey, password, knownHosts, allowMissingKnownHosts)
	if err != nil {
		return nil, err
	}
	return []gitclient.Option{gitclient.WithSSHAuth(publicKeys)}, nil
}

// logInsecureHostKey emits a loud warning whenever SSH host key verification is disabled.
func logInsecureHostKey(logger logr.Logger, reason string) {
	logger.Info("INSECURE: SSH host key verification disabled via "+InsecureAllowMissingKnownHostsFlag+
		"; do not use in production", "reason", reason)
}

// knownHostsDB parses known_hosts content into a host key database.
//
// The database carries both halves we need: the verification callback, and the per-host algorithm
// list that keeps go-git from reaching for the process's default known_hosts files (see KeyAuth).
// go-git only parses from a path, so the content is staged in a temp file; the parse is eager, so
// the file is removed before returning.
func knownHostsDB(logger logr.Logger, knownHosts string) (*knownhosts.HostKeyDB, error) {
	tmpFile, err := os.CreateTemp("", "known_hosts_*")
	if err != nil {
		logger.Info("Warning: failed to create temp known_hosts file", "error", err)
		return nil, err
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(knownHosts); err != nil {
		logger.Info("Warning: failed to write known_hosts", "error", err)
		return nil, err
	}

	if err := tmpFile.Close(); err != nil {
		logger.Info("Warning: failed to close temp file", "error", err)
		return nil, err
	}

	db, err := knownhosts.NewDB(tmpFile.Name())
	if err != nil {
		logger.Info("Warning: failed to parse known_hosts", "error", err)
		return nil, err
	}

	logger.V(1).Info("Using known_hosts for SSH host key verification")
	return db, nil
}
