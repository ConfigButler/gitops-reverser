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

package giteaclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const restrictedFileMode = 0o600

// SSHKeyVerificationOptions controls the shared SSH-key verification flow that
// signs Gitea's verification token with ssh-keygen and submits the web form.
type SSHKeyVerificationOptions struct {
	PublicKey      string
	Fingerprint    string
	PrivateKeyPEM  []byte
	PrivateKeyPath string
	WorkDir        string
	Debug          bool
}

// SSHKeyVerificationResult contains the main artifacts from a verification run.
type SSHKeyVerificationResult struct {
	Token     string
	Signature string
	Session   *WebSession
}

// WebBaseURL derives the Gitea web root from the client's /api/v1 base URL.
func (c *Client) WebBaseURL() string {
	return strings.TrimSuffix(strings.TrimRight(c.BaseURL, "/"), "/api/v1")
}

// VerifySSHKeyWithKeygen performs the full user-scoped SSH key verification flow:
// fetch token, sign it with ssh-keygen, log into the web UI, and submit the
// verify_ssh form for the given public key fingerprint.
func (c *Client) VerifySSHKeyWithKeygen(
	ctx context.Context,
	user *TestUser,
	opts SSHKeyVerificationOptions,
) (*SSHKeyVerificationResult, error) {
	if c == nil {
		return nil, errors.New("client is nil")
	}
	if user == nil {
		return nil, errors.New("user is nil")
	}
	if strings.TrimSpace(user.Login) == "" {
		return nil, errors.New("user login is empty")
	}
	if strings.TrimSpace(user.Password) == "" {
		return nil, errors.New("user password is empty")
	}
	if strings.TrimSpace(opts.PublicKey) == "" {
		return nil, errors.New("public key is empty")
	}
	if strings.TrimSpace(opts.Fingerprint) == "" {
		return nil, errors.New("fingerprint is empty")
	}

	workDir, cleanup, err := ensureVerificationWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	privateKeyPath, cleanupKey, err := ensurePrivateKeyPath(workDir, opts.PrivateKeyPath, opts.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	defer cleanupKey()

	userClient := New(c.BaseURL, user.Login, user.Password)
	token, err := userClient.GetVerificationToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get verification token: %w", err)
	}

	armoredSig, err := signTokenWithSSHKeygen(ctx, workDir, privateKeyPath, token)
	if err != nil {
		return nil, err
	}

	session, err := NewWebSession(ctx, c.WebBaseURL(), user.Login, user.Password, opts.Debug)
	if err != nil {
		return nil, fmt.Errorf("create web session: %w", err)
	}
	if err := session.VerifySSHKey(ctx, opts.PublicKey, opts.Fingerprint, armoredSig); err != nil {
		return nil, fmt.Errorf("verify SSH key in Gitea: %w", err)
	}

	return &SSHKeyVerificationResult{
		Token:     token,
		Signature: armoredSig,
		Session:   session,
	}, nil
}

func ensureVerificationWorkDir(workDir string) (string, func(), error) {
	if strings.TrimSpace(workDir) != "" {
		return workDir, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "gitea-verify-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func ensurePrivateKeyPath(workDir, privateKeyPath string, privateKeyPEM []byte) (string, func(), error) {
	if strings.TrimSpace(privateKeyPath) != "" {
		return privateKeyPath, func() {}, nil
	}
	if len(privateKeyPEM) == 0 {
		return "", nil, errors.New("private key is empty")
	}

	path := filepath.Join(workDir, "id_sign")
	if err := os.WriteFile(path, privateKeyPEM, restrictedFileMode); err != nil {
		return "", nil, fmt.Errorf("write private key: %w", err)
	}
	return path, func() {
		_ = os.Remove(path)
		_ = os.Remove(path + ".pub")
	}, nil
}

func signTokenWithSSHKeygen(ctx context.Context, workDir, privateKeyPath, token string) (string, error) {
	tokenPath := filepath.Join(workDir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte(token), restrictedFileMode); err != nil {
		return "", fmt.Errorf("write verification token: %w", err)
	}

	cmd := exec.CommandContext(ctx, "ssh-keygen", "-Y", "sign", "-n", "gitea", "-f", privateKeyPath, tokenPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen -Y sign: %w (%s)", err, out)
	}

	sigBytes, err := os.ReadFile(tokenPath + ".sig")
	if err != nil {
		return "", fmt.Errorf("read ssh-keygen signature: %w", err)
	}
	return string(sigBytes), nil
}
