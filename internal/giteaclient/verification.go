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
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/sshsig"
)

// SSHKeyVerificationOptions controls the shared SSH-key verification flow that
// signs Gitea's verification token and submits the web form.
type SSHKeyVerificationOptions struct {
	PublicKey      string
	Fingerprint    string
	PrivateKeyPEM  []byte
	PrivateKeyPath string
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

// VerifySSHKey performs the full user-scoped SSH key verification flow:
// fetch token, sign it in process, log into the web UI, and submit the
// verify_ssh form for the given public key fingerprint.
func (c *Client) VerifySSHKey(
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

	privateKeyPEM, err := resolveVerificationPrivateKeyPEM(opts.PrivateKeyPEM, opts.PrivateKeyPath)
	if err != nil {
		return nil, err
	}

	userClient := New(c.BaseURL, user.Login, user.Password)
	token, err := userClient.GetVerificationToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get verification token: %w", err)
	}

	armoredSig, err := signVerificationToken(privateKeyPEM, token)
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

func resolveVerificationPrivateKeyPEM(privateKeyPEM []byte, privateKeyPath string) ([]byte, error) {
	if len(privateKeyPEM) > 0 {
		return privateKeyPEM, nil
	}
	if strings.TrimSpace(privateKeyPath) == "" {
		return nil, errors.New("private key is empty")
	}

	privateKeyPEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return privateKeyPEM, nil
}

func signVerificationToken(privateKeyPEM []byte, token string) (string, error) {
	signer, err := sshsig.ParsePrivateKey(privateKeyPEM, nil)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	armoredSig, err := sshsig.SignMessage(signer, "gitea", []byte(token))
	if err != nil {
		return "", fmt.Errorf("sign verification token: %w", err)
	}
	return string(armoredSig), nil
}
