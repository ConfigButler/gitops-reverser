// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reverserGit "github.com/ConfigButler/gitops-reverser/internal/git"
)

func TestVerifySSHFailure_FromFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fixture   string
		wantError string
	}{
		{
			name:    "post verify success page",
			fixture: "testdata/keys-page-post-verify.html",
		},
		{
			name:      "flash error page",
			fixture:   "testdata/keys-page-flash-error.html",
			wantError: "verify_ssh rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := verifySSHFailure(http.StatusOK, readFixture(t, tt.fixture), "SHA256:testfingerprint")
			if tt.wantError == "" && err != nil {
				t.Fatalf("verifySSHFailure() unexpected error: %v", err)
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("verifySSHFailure() = %v, want substring %q", err, tt.wantError)
				}
			}
		})
	}
}

// TestNewWebSession_SendsNoCSRFToken pins the Gitea 1.26+ contract: form-token
// CSRF is gone, replaced by stdlib http.CrossOriginProtection, which admits any
// request carrying neither Sec-Fetch-Site nor Origin. A login that posts a
// `_csrf` field would be sending a field the server no longer models, and — more
// importantly — a client that still tried to *scrape* one would fail outright,
// because the login page no longer renders it.
func TestNewWebSession_SendsNoCSRFToken(t *testing.T) {
	t.Parallel()

	var loginForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/login" && r.Method == http.MethodPost {
			_ = r.ParseForm()
			loginForm = r.Form
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "session-ok", Path: "/"})
			_, _ = w.Write([]byte(`<html><body>logged in</body></html>`))
			return
		}
		// Serve a login page with no _csrf input, exactly as Gitea 1.26+ does.
		_, _ = w.Write([]byte(`<html><body><form action="/user/login" method="post">` +
			`<input name="user_name"><input name="password"></form></body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := NewWebSession(ctx, srv.URL, "alice", "secret", false); err != nil {
		t.Fatalf("NewWebSession() error = %v", err)
	}
	if got := loginForm.Get("_csrf"); got != "" {
		t.Fatalf("login form carried _csrf = %q, want it absent", got)
	}
	if got := loginForm.Get("user_name"); got != "alice" {
		t.Fatalf("login form user_name = %q, want %q", got, "alice")
	}
}

func TestNewWebSession_UsesSessionCookieForLoginSuccess(t *testing.T) {
	t.Parallel()

	loginHTML := readFixture(t, "testdata/login-page.html")
	var loginPosts int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/login":
			_, _ = w.Write([]byte(loginHTML))
		case r.Method == http.MethodPost && r.URL.Path == "/user/login":
			loginPosts++
			http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "session-ok", Path: "/"})
			// Keep login-like fields in the body so the old body heuristic would
			// have treated this as a failure despite the valid session cookie.
			_, _ = w.Write([]byte(loginHTML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := NewWebSession(ctx, srv.URL, "alice", "secret", false)
	if err != nil {
		t.Fatalf("NewWebSession() error = %v", err)
	}
	if session == nil {
		t.Fatal("NewWebSession() returned nil session")
	}
	if loginPosts != 1 {
		t.Fatalf("login POST count = %d, want 1", loginPosts)
	}
	if !session.hasCookie(sessionCookieName) {
		t.Fatalf("session missing %s cookie", sessionCookieName)
	}
}

func TestNewWebSession_FailsWithoutSessionCookie(t *testing.T) {
	t.Parallel()

	loginHTML := readFixture(t, "testdata/login-page.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/login":
			_, _ = w.Write([]byte(loginHTML))
		case r.Method == http.MethodPost && r.URL.Path == "/user/login":
			_, _ = w.Write([]byte(loginHTML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := NewWebSession(ctx, srv.URL, "alice", "secret", false)
	if err == nil || !strings.Contains(err.Error(), "missing i_like_gitea session cookie") {
		t.Fatalf("NewWebSession() error = %v, want missing session cookie", err)
	}
}

func TestClientVerifySSHKey_EndToEnd(t *testing.T) {
	privateKeyPEM, publicKey, err := reverserGit.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		t.Fatalf("GenerateSSHSigningKeyPair() error = %v", err)
	}

	srv, verifyForm := newVerificationTestServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := New(srv.URL+"/api/v1", "admin", "admin-pass")
	result, err := client.VerifySSHKey(ctx, &TestUser{
		Login:    "alice",
		Password: "secret",
	}, SSHKeyVerificationOptions{
		PublicKey:     strings.TrimSpace(string(publicKey)),
		Fingerprint:   "SHA256:testfingerprint",
		PrivateKeyPEM: privateKeyPEM,
	})
	if err != nil {
		t.Fatalf("VerifySSHKey() error = %v", err)
	}

	if result == nil || result.Session == nil {
		t.Fatal("VerifySSHKey() returned nil result")
	}
	if result.Token != "token-to-sign" {
		t.Fatalf("result.Token = %q, want %q", result.Token, "token-to-sign")
	}
	if !strings.Contains(result.Signature, "BEGIN SSH SIGNATURE") {
		t.Fatalf("result.Signature missing SSH signature armor: %q", result.Signature)
	}
	assertVerifyForm(t, verifyForm, strings.TrimSpace(string(publicKey)))
}

func TestClientVerifySSHKey_PrivateKeyPathFallback(t *testing.T) {
	privateKeyPEM, publicKey, err := reverserGit.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		t.Fatalf("GenerateSSHSigningKeyPair() error = %v", err)
	}

	srv, verifyForm := newVerificationTestServer(t)
	defer srv.Close()

	keyPath := filepath.Join(t.TempDir(), "id_sign")
	if err := os.WriteFile(keyPath, privateKeyPEM, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", keyPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := New(srv.URL+"/api/v1", "admin", "admin-pass")
	result, err := client.VerifySSHKey(ctx, &TestUser{
		Login:    "alice",
		Password: "secret",
	}, SSHKeyVerificationOptions{
		PublicKey:      strings.TrimSpace(string(publicKey)),
		Fingerprint:    "SHA256:testfingerprint",
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("VerifySSHKey() error = %v", err)
	}

	if result == nil || result.Session == nil {
		t.Fatal("VerifySSHKey() returned nil result")
	}
	assertVerifyForm(t, verifyForm, strings.TrimSpace(string(publicKey)))
}

func readFixture(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	return string(data)
}

func newVerificationTestServer(t *testing.T) (*httptest.Server, *url.Values) {
	t.Helper()

	var verifyForm url.Values
	fixtures := verificationFixtures{
		loginHTML:   readFixture(t, "testdata/login-page.html"),
		keysHTML:    readFixture(t, "testdata/keys-page-pre-verify.html"),
		successHTML: readFixture(t, "testdata/keys-page-post-verify.html"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleVerificationTestRequest(w, r, fixtures, &verifyForm)
	}))

	return srv, &verifyForm
}

type verificationFixtures struct {
	loginHTML   string
	keysHTML    string
	successHTML string
}

func handleVerificationTestRequest(
	w http.ResponseWriter,
	r *http.Request,
	fixtures verificationFixtures,
	verifyForm *url.Values,
) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user/gpg_key_token":
		handleVerificationTokenRequest(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/user/login":
		handleVerificationLoginPage(w, fixtures.loginHTML)
	case r.Method == http.MethodPost && r.URL.Path == "/user/login":
		handleVerificationLoginPost(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/user/settings/keys":
		handleVerificationKeysPage(w, fixtures.keysHTML)
	case r.Method == http.MethodPost && r.URL.Path == "/user/settings/keys":
		handleVerificationKeysPost(w, r, fixtures.successHTML, verifyForm)
	default:
		http.NotFound(w, r)
	}
}

func handleVerificationTokenRequest(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "alice" || pass != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, _ = w.Write([]byte("token-to-sign"))
}

func handleVerificationLoginPage(w http.ResponseWriter, loginHTML string) {
	_, _ = w.Write([]byte(loginHTML))
}

func handleVerificationLoginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "session-ok", Path: "/"})
	_, _ = w.Write([]byte(`<html><body>logged in</body></html>`))
}

func handleVerificationKeysPage(w http.ResponseWriter, keysHTML string) {
	_, _ = w.Write([]byte(keysHTML))
}

func handleVerificationKeysPost(
	w http.ResponseWriter,
	r *http.Request,
	successHTML string,
	verifyForm *url.Values,
) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	*verifyForm = r.Form
	_, _ = w.Write([]byte(successHTML))
}

func assertVerifyForm(t *testing.T, verifyForm *url.Values, publicKey string) {
	t.Helper()

	if got := verifyForm.Get("_csrf"); got != "" {
		t.Fatalf("verify form carried _csrf = %q, want it absent", got)
	}
	if got := verifyForm.Get("type"); got != "verify_ssh" {
		t.Fatalf("verify form type = %q, want %q", got, "verify_ssh")
	}
	if got := verifyForm.Get("title"); got != "none" {
		t.Fatalf("verify form title = %q, want %q", got, "none")
	}
	if got := verifyForm.Get("content"); got != publicKey {
		t.Fatalf("verify form content = %q, want uploaded public key", got)
	}
	if got := verifyForm.Get("fingerprint"); got != "SHA256:testfingerprint" {
		t.Fatalf("verify form fingerprint = %q, want %q", got, "SHA256:testfingerprint")
	}
	if !strings.Contains(verifyForm.Get("signature"), "BEGIN SSH SIGNATURE") {
		t.Fatalf("verify form signature missing armored SSH signature")
	}
}
