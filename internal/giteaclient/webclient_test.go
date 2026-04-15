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
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	reverserGit "github.com/ConfigButler/gitops-reverser/internal/git"
)

func TestExtractCSRFToken_FromFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "login page name before value",
			fixture: "testdata/login-page.html",
			want:    "login-csrf-token",
		},
		{
			name:    "keys page value before name",
			fixture: "testdata/keys-page-pre-verify.html",
			want:    "keys-csrf-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			html := readFixture(t, tt.fixture)
			if got := extractCSRFToken(html); got != tt.want {
				t.Fatalf("extractCSRFToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestWebSessionFetchCSRF_FallsBackToCookie(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "cookie-csrf-token", Path: "/"})
		_, _ = w.Write([]byte(`<html><body>no hidden csrf input here</body></html>`))
	}))
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}

	session := &WebSession{
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Jar: jar, Timeout: defaultHTTPTimeout},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	token, err := session.fetchCSRF(ctx, "/user/login")
	if err != nil {
		t.Fatalf("fetchCSRF() error = %v", err)
	}
	if token != "cookie-csrf-token" {
		t.Fatalf("fetchCSRF() = %q, want %q", token, "cookie-csrf-token")
	}
}

func TestNewWebSession_UsesSessionCookieForLoginSuccess(t *testing.T) {
	t.Parallel()

	loginHTML := readFixture(t, "testdata/login-page.html")
	var loginPosts int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user/login":
			http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "login-csrf-token", Path: "/"})
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
			http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "login-csrf-token", Path: "/"})
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

func TestClientVerifySSHKeyWithKeygen_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found in PATH")
	}

	privateKeyPEM, publicKey, err := reverserGit.GenerateSSHSigningKeyPair(nil)
	if err != nil {
		t.Fatalf("GenerateSSHSigningKeyPair() error = %v", err)
	}

	srv, verifyForm := newVerificationTestServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := New(srv.URL+"/api/v1", "admin", "admin-pass")
	result, err := client.VerifySSHKeyWithKeygen(ctx, &TestUser{
		Login:    "alice",
		Password: "secret",
	}, SSHKeyVerificationOptions{
		PublicKey:     strings.TrimSpace(string(publicKey)),
		Fingerprint:   "SHA256:testfingerprint",
		PrivateKeyPEM: privateKeyPEM,
	})
	if err != nil {
		t.Fatalf("VerifySSHKeyWithKeygen() error = %v", err)
	}

	if result == nil || result.Session == nil {
		t.Fatal("VerifySSHKeyWithKeygen() returned nil result")
	}
	if result.Token != "token-to-sign" {
		t.Fatalf("result.Token = %q, want %q", result.Token, "token-to-sign")
	}
	if !strings.Contains(result.Signature, "BEGIN SSH SIGNATURE") {
		t.Fatalf("result.Signature missing SSH signature armor: %q", result.Signature)
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
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "login-csrf-token", Path: "/"})
	_, _ = w.Write([]byte(loginHTML))
}

func handleVerificationLoginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("_csrf") != "login-csrf-token" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "session-ok", Path: "/"})
	_, _ = w.Write([]byte(`<html><body>logged in</body></html>`))
}

func handleVerificationKeysPage(w http.ResponseWriter, keysHTML string) {
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "keys-csrf-token", Path: "/"})
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

	if got := verifyForm.Get("_csrf"); got != "keys-csrf-token" {
		t.Fatalf("verify form _csrf = %q, want %q", got, "keys-csrf-token")
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
