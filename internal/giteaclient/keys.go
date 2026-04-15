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
	"net/http"
	"strings"
)

const authorizedKeyFieldCount = 2

// NormalizeAuthorizedKey returns the "<type> <base64>" prefix of an
// authorized_keys entry, dropping any trailing comment. This matches how Gitea
// stores and returns keys, so it is the right shape for equality comparisons.
func NormalizeAuthorizedKey(k string) string {
	fields := strings.Fields(strings.TrimSpace(k))
	if len(fields) < authorizedKeyFieldCount {
		return strings.TrimSpace(k)
	}
	return fields[0] + " " + fields[1]
}

// ListUserKeys returns every public key registered for the named user.
func (c *Client) ListUserKeys(ctx context.Context, login string) ([]PublicKey, error) {
	var keys []PublicKey
	path := "/users/" + PathEscape(login) + "/keys"
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &keys)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, unexpectedStatus(http.MethodGet, path, code, raw)
	}
	return keys, nil
}

// FindUserKey looks up a public key on the named user by key material,
// ignoring the trailing comment.
func (c *Client) FindUserKey(ctx context.Context, login, publicKey string) (*PublicKey, bool, error) {
	want := NormalizeAuthorizedKey(publicKey)
	keys, err := c.ListUserKeys(ctx, login)
	if err != nil {
		return nil, false, err
	}
	for i := range keys {
		if NormalizeAuthorizedKey(keys[i].Key) == want {
			return &keys[i], true, nil
		}
	}
	return nil, false, nil
}

// RegisterUserKeyAsAdmin idempotently registers a public key on the named user
// using the admin endpoint POST /admin/users/{username}/keys.
func (c *Client) RegisterUserKeyAsAdmin(ctx context.Context, login, title, publicKey string) (*PublicKey, error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return nil, errors.New("public key is empty")
	}
	if existing, found, err := c.FindUserKey(ctx, login, publicKey); err != nil {
		return nil, err
	} else if found {
		return existing, nil
	}

	payload := map[string]string{"title": title, "key": publicKey}
	path := "/admin/users/" + PathEscape(login) + "/keys"
	var created PublicKey
	code, raw, err := c.Do(ctx, http.MethodPost, path, payload, &created)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		return &created, nil
	case http.StatusUnprocessableEntity:
		if existing, found, lookupErr := c.FindUserKey(ctx, login, publicKey); lookupErr == nil && found {
			return existing, nil
		}
		return nil, fmt.Errorf("register key for %s: HTTP 422: %s", login, TruncateBody(string(raw)))
	default:
		return nil, unexpectedStatus(http.MethodPost, path, code, raw)
	}
}

// RegisterUserKeyAsUser registers a public key via POST /user/keys using the
// currently authenticated user. Use this when comparing whether admin-upload
// vs user-upload lands the key with a different key_type in Gitea's classification.
func (c *Client) RegisterUserKeyAsUser(ctx context.Context, title, publicKey string) (*PublicKey, error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return nil, errors.New("public key is empty")
	}
	payload := map[string]any{"title": title, "key": publicKey, "read_only": false}
	var created PublicKey
	code, raw, err := c.Do(ctx, http.MethodPost, "/user/keys", payload, &created)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		return &created, nil
	case http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("register user key: HTTP 422: %s", TruncateBody(string(raw)))
	default:
		return nil, unexpectedStatus(http.MethodPost, "/user/keys", code, raw)
	}
}

// GetVerificationToken returns the per-user verification token. The same token
// is used by the GPG and SSH verification flows; Gitea's web form accepts an
// SSH signature over it (namespace "gitea") to flip public_key.verified=1.
// Requires the Client to be authenticated as the target user.
func (c *Client) GetVerificationToken(ctx context.Context) (string, error) {
	code, raw, err := c.Do(ctx, http.MethodGet, "/user/gpg_key_token", nil, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", unexpectedStatus(http.MethodGet, "/user/gpg_key_token", code, raw)
	}
	return strings.TrimSpace(string(raw)), nil
}

// GetKeyRaw fetches a single key by ID and returns the raw JSON body, so
// callers can inspect fields the typed struct may not decode (e.g. internal
// columns Gitea might expose in newer versions).
func (c *Client) GetKeyRaw(ctx context.Context, keyID int64) ([]byte, error) {
	path := fmt.Sprintf("/user/keys/%d", keyID)
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, unexpectedStatus(http.MethodGet, path, code, raw)
	}
	return raw, nil
}
