// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
)

const (
	debugMatchContextBefore = 40
	debugMatchContextAfter  = 120
	flashErrorPreviewSize   = 400
	httpErrorThreshold      = 400
	sessionCookieName       = "i_like_gitea"
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
		HTTPClient: &http.Client{Jar: jar, Timeout: defaultHTTPTimeout},
		Debug:      debug,
	}

	// POST credentials. On success Gitea sets the session cookie
	// (`i_like_gitea`) and 302-redirects away from /user/login.
	//
	// No CSRF token is sent. Gitea 1.26 replaced form-token CSRF with Go's
	// stdlib http.CrossOriginProtection (routers/web/web.go), which allows any
	// request carrying neither Sec-Fetch-Site nor Origin — i.e. every non-browser
	// client. /user/login is additionally exempt, being SignOutRequired.
	form := url.Values{}
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
	s.debugLogin(resp.StatusCode, resp.Request.URL.String(), len(body))

	if resp.StatusCode >= httpErrorThreshold {
		return nil, fmt.Errorf("login failed for %s: HTTP %d: %s",
			username, resp.StatusCode, TruncateBody(string(body)))
	}
	if !s.hasCookie(sessionCookieName) {
		return nil, fmt.Errorf("login failed for %s: missing %s session cookie. body preview: %s",
			username, sessionCookieName, TruncateBody(string(body)))
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
	form := url.Values{}
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
	s.debugVerifyResponse(resp.StatusCode, resp.Request.URL.String(), string(body))

	// Success path: Gitea 302-redirects to /user/settings/keys and flashes
	// a success message; the rendered page no longer offers a Verify form
	// for this fingerprint. Failure path: the page re-renders with an error
	// block. We detect failure by the presence of the invalid-signature class
	// or the presence of the "HasSSHVerifyError" template data leaking into
	// form state.
	return verifySSHFailure(resp.StatusCode, string(body), fingerprint)
}

func (s *WebSession) hasCookie(name string) bool {
	return s.cookieValue(name) != ""
}

func (s *WebSession) cookieValue(name string) string {
	if s == nil || s.HTTPClient == nil || s.HTTPClient.Jar == nil {
		return ""
	}

	baseURL, err := url.Parse(s.BaseURL)
	if err != nil {
		return ""
	}

	for _, cookie := range s.HTTPClient.Jar.Cookies(baseURL) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func (s *WebSession) debugLogin(statusCode int, finalURL string, bodyLen int) {
	if !s.Debug {
		return
	}

	_, _ = fmt.Fprintf(os.Stderr, "   [web] login POST -> HTTP %d, final URL %s, body len=%d\n",
		statusCode, finalURL, bodyLen)
}

func (s *WebSession) debugVerifyResponse(statusCode int, finalURL, body string) {
	if !s.Debug {
		return
	}

	_, _ = fmt.Fprintf(os.Stderr, "   [web] verify POST -> HTTP %d, final URL %s, body len=%d\n",
		statusCode, finalURL, len(body))
	for _, needle := range []string{
		"verify_ssh_key_success",
		"ssh_invalid_token_signature",
		"HasSSHVerifyError",
		"flash-",
		"error",
		"Verified Key",
		"Unverified Key",
	} {
		if snippet, ok := debugSnippet(body, needle); ok {
			_, _ = fmt.Fprintf(os.Stderr, "   [web] body match %q: ...%s...\n", needle, snippet)
		}
	}
}

func debugSnippet(body, needle string) (string, bool) {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return "", false
	}

	start := max(idx-debugMatchContextBefore, 0)
	end := min(idx+debugMatchContextAfter, len(body))
	return strings.ReplaceAll(body[start:end], "\n", " "), true
}

func verifySSHFailure(statusCode int, html, fingerprint string) error {
	if statusCode >= httpErrorThreshold {
		return fmt.Errorf("verify_ssh HTTP %d: %s", statusCode, TruncateBody(html))
	}

	if idx := strings.Index(html, "flash-error"); idx >= 0 {
		end := min(idx+flashErrorPreviewSize, len(html))
		return fmt.Errorf("verify_ssh rejected (fingerprint=%s): %s",
			fingerprint, strings.ReplaceAll(html[idx:end], "\n", " "))
	}
	return nil
}
