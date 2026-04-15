/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
*/

// Package giteaclient is a small, focused Gitea REST client used by e2e tests
// and debug tools. It is intentionally not a full SDK: only the endpoints the
// project exercises are covered.
package giteaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a Gitea instance using basic auth.
// Username/Password are used for every request; for flows that require acting
// as a specific user (e.g. key verification), construct a second Client with
// that user's credentials.
type Client struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

// New returns a Client with sensible defaults. baseURL must be the /api/v1 root.
func New(baseURL, username, password string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Username:   username,
		Password:   password,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Do issues an authenticated JSON request. If out is non-nil and the response
// is 2xx with a body, the body is unmarshalled into out. The raw body is
// always returned so callers can report unexpected responses.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (int, []byte, error) {
	var reader io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal %s %s body: %w", method, path, err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read %s %s body: %w", method, path, err)
	}

	if out != nil && len(raw) > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("decode %s %s: %w (body=%s)",
				method, path, err, TruncateBody(string(raw)))
		}
	}
	return resp.StatusCode, raw, nil
}

// PathEscape percent-encodes a single URL path segment.
func PathEscape(v string) string { return url.PathEscape(strings.TrimSpace(v)) }

// TruncateBody clips a response body for safe inclusion in error messages.
func TruncateBody(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// unexpectedStatus is a convenience for building a consistent error string.
func unexpectedStatus(method, path string, code int, raw []byte) error {
	return fmt.Errorf("%s %s: HTTP %d: %s", method, path, code, TruncateBody(string(raw)))
}
