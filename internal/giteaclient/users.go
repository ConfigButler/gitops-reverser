/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
*/

package giteaclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GetUser returns the Gitea user with the given login, or (nil, false, nil) if
// it does not exist.
func (c *Client) GetUser(ctx context.Context, login string) (*User, bool, error) {
	var u User
	path := "/users/" + PathEscape(login)
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &u)
	if err != nil {
		return nil, false, err
	}
	switch code {
	case http.StatusOK:
		return &u, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, unexpectedStatus(http.MethodGet, path, code, raw)
	}
}

// CreateUser creates a Gitea user via the admin API. Returns the created user
// plus the generated password. Returns ErrUserExists (wrapped) if the user
// already exists with a different email.
func (c *Client) CreateUser(ctx context.Context, login, email string) (*User, string, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, "", errors.New("login is empty")
	}
	password, err := randomPassword()
	if err != nil {
		return nil, "", err
	}
	payload := map[string]any{
		"username":             login,
		"email":                email,
		"password":             password,
		"must_change_password": false,
		"source_id":            0,
		"login_name":           login,
		"full_name":            login,
	}
	var created User
	code, raw, err := c.Do(ctx, http.MethodPost, "/admin/users", payload, &created)
	if err != nil {
		return nil, "", err
	}
	if code != http.StatusCreated {
		return nil, "", fmt.Errorf("create user %s: HTTP %d: %s", login, code, TruncateBody(string(raw)))
	}
	if strings.TrimSpace(created.Email) == "" {
		created.Email = email
	}
	return &created, password, nil
}

// SetUserPassword resets a user's password via the admin API so tooling can
// authenticate as that user after the fact.
func (c *Client) SetUserPassword(ctx context.Context, login, password string) error {
	payload := map[string]any{
		"source_id":            0,
		"login_name":           login,
		"password":             password,
		"must_change_password": false,
	}
	path := "/admin/users/" + PathEscape(login)
	code, raw, err := c.Do(ctx, http.MethodPatch, path, payload, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return unexpectedStatus(http.MethodPatch, path, code, raw)
	}
	return nil
}

// EnsureUser creates the user if missing and always returns a TestUser with a
// known password (rotating it on reuse so callers can always re-authenticate).
func (c *Client) EnsureUser(ctx context.Context, login, email string) (*TestUser, error) {
	existing, found, err := c.GetUser(ctx, login)
	if err != nil {
		return nil, err
	}
	if found {
		password, err := randomPassword()
		if err != nil {
			return nil, err
		}
		if err := c.SetUserPassword(ctx, login, password); err != nil {
			return nil, fmt.Errorf("rotate password for %s: %w", login, err)
		}
		if strings.TrimSpace(existing.Email) == "" {
			existing.Email = email
		}
		return &TestUser{
			Login: existing.Login, Email: existing.Email, Password: password, ID: existing.ID,
		}, nil
	}
	created, password, err := c.CreateUser(ctx, login, email)
	if err != nil {
		return nil, err
	}
	return &TestUser{
		Login: created.Login, Email: created.Email, Password: password, ID: created.ID,
	}, nil
}

func randomPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate random password: %w", err)
	}
	return "dbg-" + hex.EncodeToString(b[:]), nil
}
