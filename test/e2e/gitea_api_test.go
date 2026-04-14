/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// giteaPublicKey mirrors the subset of the Gitea /user/keys payload we use.
// The same endpoint stores both transport SSH keys and signing verification
// keys in Gitea 1.25.x; a dedicated signing-key endpoint is not exposed yet.
type giteaPublicKey struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

// giteaCommitVerification is the verification block of the repo commit API.
type giteaCommitVerification struct {
	Verified  bool   `json:"verified"`
	Reason    string `json:"reason"`
	Signature string `json:"signature"`
}

type giteaCommitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Verification giteaCommitVerification `json:"verification"`
	} `json:"commit"`
}

func giteaAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("GITEA_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:13000/api/v1"
}

func giteaAdminCreds() (string, string) {
	user := strings.TrimSpace(os.Getenv("GITEA_ADMIN_USER"))
	if user == "" {
		user = "giteaadmin"
	}
	pass := strings.TrimSpace(os.Getenv("GITEA_ADMIN_PASS"))
	if pass == "" {
		pass = "giteapassword123"
	}
	return user, pass
}

// giteaOrg returns the Gitea org used for e2e test repos.
func giteaOrg() string {
	if v := strings.TrimSpace(os.Getenv("ORG_NAME")); v != "" {
		return v
	}
	return "testorg"
}

func giteaDo(method, path string, body any, out any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal gitea request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	url := giteaAPIBase() + path
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build gitea request: %w", err)
	}
	user, pass := giteaAdminCreds()
	req.SetBasicAuth(user, pass)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("gitea %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read gitea %s %s response: %w", method, path, err)
	}

	if out != nil && len(raw) > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("decode gitea %s %s response: %w (body=%s)",
				method, path, err, truncate(string(raw), 512))
		}
	}
	return resp.StatusCode, raw, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// normalizeAuthorizedKey returns the "<type> <base64>" prefix of an
// authorized_keys entry, dropping any comment. This is how Gitea stores and
// returns the key body, so it is the right shape for equality comparisons.
func normalizeAuthorizedKey(k string) string {
	fields := strings.Fields(strings.TrimSpace(k))
	if len(fields) < 2 {
		return strings.TrimSpace(k)
	}
	return fields[0] + " " + fields[1]
}

// ListUserPublicKeys returns the authenticated user's registered public keys.
func ListUserPublicKeys() ([]giteaPublicKey, error) {
	var keys []giteaPublicKey
	code, raw, err := giteaDo(http.MethodGet, "/user/keys", nil, &keys)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list user keys: HTTP %d: %s", code, truncate(string(raw), 512))
	}
	return keys, nil
}

// FindUserPublicKeyByKey looks up a registered public key by its authorized
// key material, ignoring the trailing comment.
func FindUserPublicKeyByKey(publicKey string) (*giteaPublicKey, error) {
	want := normalizeAuthorizedKey(publicKey)
	keys, err := ListUserPublicKeys()
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if normalizeAuthorizedKey(keys[i].Key) == want {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// RegisterSigningPublicKey idempotently registers a public key with Gitea so
// that commits signed by the corresponding private key are reported as
// verified by the commit API.
//
// Gitea 1.25.x does not expose a dedicated signing-key endpoint, so this uses
// /user/keys. The helper name stays signing-focused so the implementation can
// migrate if a dedicated endpoint becomes available.
func RegisterSigningPublicKey(publicKey, title string) (*giteaPublicKey, error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return nil, fmt.Errorf("public key is empty")
	}

	if existing, err := FindUserPublicKeyByKey(publicKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	payload := map[string]string{
		"title": title,
		"key":   publicKey,
	}
	var created giteaPublicKey
	code, raw, err := giteaDo(http.MethodPost, "/user/keys", payload, &created)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		return &created, nil
	case http.StatusUnprocessableEntity:
		// Gitea may reject a duplicate fingerprint with 422; re-scan in case a
		// different title already carries the same key body.
		if existing, lookupErr := FindUserPublicKeyByKey(publicKey); lookupErr == nil && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("register signing key: HTTP 422: %s", truncate(string(raw), 512))
	default:
		return nil, fmt.Errorf("register signing key: HTTP %d: %s", code, truncate(string(raw), 512))
	}
}

// DeleteUserPublicKey removes a registered public key by id. Intended for
// DeferCleanup in tests that provisioned their own signing keys.
func DeleteUserPublicKey(id int64) error {
	code, raw, err := giteaDo(http.MethodDelete, fmt.Sprintf("/user/keys/%d", id), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return fmt.Errorf("delete user key %d: HTTP %d: %s", id, code, truncate(string(raw), 512))
	}
	return nil
}

// giteaUser is the subset of the Gitea user payload we inspect.
type giteaUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

// getGiteaUser fetches a user via the admin API.
func getGiteaUser(username string) (*giteaUser, error) {
	var u giteaUser
	code, raw, err := giteaDo(http.MethodGet, "/users/"+username, nil, &u)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("get user %s: HTTP %d: %s", username, code, truncate(string(raw), 512))
	}
	return &u, nil
}

// EnsureAdminUserPrimaryEmail ensures the given Gitea user's primary email
// matches `email`. Gitea associates SSH-signed commits with a user by walking
// its verified email addresses; the user's primary email is always treated as
// verified, so overriding it is the most reliable way to bind e2e commits with
// a fixed committer email to the admin account that owns the signing keys.
//
// Returns true if an update was performed.
func EnsureAdminUserPrimaryEmail(username, email string) (bool, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return false, fmt.Errorf("email is empty")
	}

	user, err := getGiteaUser(username)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(user.Email, email) {
		return false, nil
	}

	// AdminEditUserOption requires source_id and login_name as of Gitea 1.21+.
	payload := map[string]any{
		"source_id":  0,
		"login_name": username,
		"email":      email,
	}
	code, raw, err := giteaDo(http.MethodPatch, "/admin/users/"+username, payload, nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusBadRequest && strings.Contains(string(raw), "already in use") {
		// Gitea rejects the primary-email change when the target address is
		// still attached to the same user as a secondary email. Remove the
		// secondary binding and retry.
		if rmErr := removeUserEmail(email); rmErr != nil {
			return false, fmt.Errorf("patch admin user %s email: already in use and failed to remove secondary: %w",
				username, rmErr)
		}
		code, raw, err = giteaDo(http.MethodPatch, "/admin/users/"+username, payload, nil)
		if err != nil {
			return false, err
		}
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return false, fmt.Errorf("patch admin user %s email: HTTP %d: %s",
			username, code, truncate(string(raw), 512))
	}
	return true, nil
}

// removeUserEmail removes an email from the authenticated user's secondary
// email list. Safe to call when the email is not present.
func removeUserEmail(email string) error {
	payload := map[string][]string{"emails": {email}}
	code, raw, err := giteaDo(http.MethodDelete, "/user/emails", payload, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return fmt.Errorf("delete user email %s: HTTP %d: %s", email, code, truncate(string(raw), 512))
	}
	return nil
}

// giteaEmail describes one entry in the authenticated user's email list.
type giteaEmail struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
	Primary  bool   `json:"primary"`
}

// EnsureUserEmail makes sure the authenticated user has the given email
// registered, so that Gitea associates commits bearing that committer email
// with this user (and therefore with the signing keys the user owns).
//
// Returns true if the email was newly added. Safe to call repeatedly.
func EnsureUserEmail(email string) (bool, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return false, fmt.Errorf("email is empty")
	}

	var existing []giteaEmail
	code, raw, err := giteaDo(http.MethodGet, "/user/emails", nil, &existing)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("list user emails: HTTP %d: %s", code, truncate(string(raw), 512))
	}
	for _, e := range existing {
		if strings.EqualFold(e.Email, email) {
			return false, nil
		}
	}

	payload := map[string][]string{"emails": {email}}
	code, raw, err = giteaDo(http.MethodPost, "/user/emails", payload, nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return true, nil
	case http.StatusUnprocessableEntity:
		// Already registered under another user or duplicate — tolerate.
		return false, nil
	default:
		return false, fmt.Errorf("add user email %s: HTTP %d: %s", email, code, truncate(string(raw), 512))
	}
}

// GetCommitVerification fetches the repo commit API for the given SHA and
// returns its verification block.
func GetCommitVerification(owner, repo, sha string) (*giteaCommitVerification, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/commits/%s", owner, repo, sha)
	var resp giteaCommitResponse
	code, raw, err := giteaDo(http.MethodGet, path, nil, &resp)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("get commit %s/%s@%s: HTTP %d: %s",
			owner, repo, sha, code, truncate(string(raw), 512))
	}
	v := resp.Commit.Verification
	return &v, nil
}
