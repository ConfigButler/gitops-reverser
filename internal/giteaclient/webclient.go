/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
*/

package giteaclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebSession drives Gitea's web UI for flows the REST API does not expose
// (most importantly: SSH key verification, which lives at
// POST /user/settings/keys?type=verify_ssh).
//
// BaseURL must be the Gitea host root WITHOUT /api/v1 (e.g.
// "http://gitea-http.gitea-e2e.svc.cluster.local:13000").
type WebSession struct {
	BaseURL    string
	HTTPClient *http.Client
	// Debug, when true, prints each HTTP request/response so we can see
	// exactly what Gitea says on login and verify.
	Debug bool
}

// NewWebSession logs the given user into Gitea and returns a session whose
// cookie jar carries the login cookies required for subsequent form POSTs.
func NewWebSession(ctx context.Context, baseURL, username, password string, debug bool) (*WebSession, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	s := &WebSession{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Jar: jar, Timeout: 15 * time.Second},
		Debug:      debug,
	}

	// Step 1: GET the login page to receive a _csrf cookie and form token.
	csrf, err := s.fetchCSRF(ctx, "/user/login")
	if err != nil {
		return nil, fmt.Errorf("fetch login CSRF: %w", err)
	}

	// Step 2: POST credentials. On success Gitea sets the session cookie
	// (`i_like_gitea`) and 302-redirects away from /user/login.
	form := url.Values{}
	form.Set("_csrf", csrf)
	form.Set("user_name", username)
	form.Set("password", password)
	form.Set("remember", "off")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.BaseURL+"/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /user/login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if s.Debug {
		fmt.Printf("   [web] login POST -> HTTP %d, final URL %s, body len=%d\n",
			resp.StatusCode, resp.Request.URL.String(), len(body))
	}

	// On a failed login Gitea returns 200 and re-renders the login page.
	// Detect the explicit username/password form so we fail loudly.
	if strings.Contains(string(body), `name="user_name"`) &&
		strings.Contains(string(body), `name="password"`) &&
		strings.Contains(resp.Request.URL.Path, "/user/login") {
		return nil, fmt.Errorf("login failed for %s (still on /user/login). body preview: %s",
			username, TruncateBody(string(body)))
	}
	return s, nil
}

// VerifySSHKey drives the web form at POST /user/settings/keys?type=verify_ssh
// to flip `public_key.verified = 1` for the uploaded key matching fingerprint.
// publicKey is the authorized_keys-form public key ("ssh-ed25519 AAAA..."),
// which Gitea requires in the `content` form field even though it already has
// the key stored. signature is the armored `-----BEGIN SSH SIGNATURE-----`
// block produced by `ssh-keygen -Y sign -n gitea` over the token from
// /user/gpg_key_token.
func (s *WebSession) VerifySSHKey(ctx context.Context, publicKey, fingerprint, signature string) error {
	// Gitea's keys settings page renders a fresh CSRF token we must echo back.
	csrf, err := s.fetchCSRF(ctx, "/user/settings/keys")
	if err != nil {
		return fmt.Errorf("fetch keys-page CSRF: %w", err)
	}

	form := url.Values{}
	form.Set("_csrf", csrf)
	form.Set("type", "verify_ssh")
	// The browser-rendered form sets title=none unconditionally and posts the
	// public key in `content`. Replicate both exactly.
	form.Set("title", "none")
	form.Set("content", publicKey)
	form.Set("fingerprint", fingerprint)
	form.Set("signature", signature)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.BaseURL+"/user/settings/keys", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /user/settings/keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if s.Debug {
		fmt.Printf("   [web] verify POST -> HTTP %d, final URL %s, body len=%d\n",
			resp.StatusCode, resp.Request.URL.String(), len(body))
		preview := string(body)
		for _, needle := range []string{"verify_ssh_key_success", "ssh_invalid_token_signature",
			"HasSSHVerifyError", "flash-", "error", "Verified Key", "Unverified Key"} {
			if idx := strings.Index(preview, needle); idx >= 0 {
				start := idx - 40
				if start < 0 {
					start = 0
				}
				end := idx + 120
				if end > len(preview) {
					end = len(preview)
				}
				fmt.Printf("   [web] body match %q: ...%s...\n", needle,
					strings.ReplaceAll(preview[start:end], "\n", " "))
			}
		}
	}

	// Success path: Gitea 302-redirects to /user/settings/keys and flashes
	// a success message; the rendered page no longer offers a Verify form
	// for this fingerprint. Failure path: the page re-renders with an error
	// block. We detect failure by the presence of the invalid-signature class
	// or the presence of the "HasSSHVerifyError" template data leaking into
	// form state.
	html := string(body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("verify_ssh HTTP %d: %s", resp.StatusCode, TruncateBody(html))
	}
	// Gitea surfaces both success and failure through flash messages rendered
	// on the redirected settings page. Treat any `flash-error` as failure.
	if idx := strings.Index(html, "flash-error"); idx >= 0 {
		end := idx + 400
		if end > len(html) {
			end = len(html)
		}
		return fmt.Errorf("verify_ssh rejected (fingerprint=%s): %s",
			fingerprint, strings.ReplaceAll(html[idx:end], "\n", " "))
	}
	return nil
}

// fetchCSRF GETs the given path and extracts the form CSRF token. Gitea
// renders it as: <input ... name="_csrf" value="...">.
func (s *WebSession) fetchCSRF(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	m := csrfInputRE.FindStringSubmatch(string(body))
	if len(m) < 3 {
		return "", fmt.Errorf("no _csrf input found at %s (HTTP %d)", path, resp.StatusCode)
	}
	if m[1] != "" {
		return m[1], nil
	}
	return m[2], nil
}

// csrfInputRE matches `<input ... name="_csrf" value="TOKEN">` in either order.
var csrfInputRE = regexp.MustCompile(
	`<input[^>]*\bname="_csrf"[^>]*\bvalue="([^"]+)"|` +
		`<input[^>]*\bvalue="([^"]+)"[^>]*\bname="_csrf"`,
)
