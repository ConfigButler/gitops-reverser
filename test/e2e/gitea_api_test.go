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

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// giteaPublicKey mirrors the subset of the Gitea public-key payload we use.
// Gitea 1.25.x stores SSH transport and SSH signing verification keys in the
// same backing store, so the same shape works for both helpers.
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

// giteaUser is the subset of the Gitea user payload we inspect.
type giteaUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

// giteaTestUser is the per-repo Gitea identity used by signing e2e tests.
type giteaTestUser struct {
	Login    string
	Email    string
	Password string
	ID       int64
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

	requestURL := giteaAPIBase() + path
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
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
				method, path, err, truncateGiteaBody(string(raw)))
		}
	}
	return resp.StatusCode, raw, nil
}

func truncateGiteaBody(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func giteaUserPathSegment(v string) string {
	return url.PathEscape(strings.TrimSpace(v))
}

func testUserEmail(login string) string {
	return login + "@configbutler.test"
}

func randomPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate random password: %w", err)
	}
	return "e2e-" + hex.EncodeToString(b[:]), nil
}

func getGiteaUser(username string) (*giteaUser, bool, error) {
	var u giteaUser
	path := "/users/" + giteaUserPathSegment(username)
	code, raw, err := giteaDo(http.MethodGet, path, nil, &u)
	if err != nil {
		return nil, false, err
	}
	switch code {
	case http.StatusOK:
		return &u, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("get user %s: HTTP %d: %s", username, code, truncateGiteaBody(string(raw)))
	}
}

func updateGiteaUserEmail(username, email string) error {
	payload := map[string]any{
		"source_id":  0,
		"login_name": username,
		"email":      email,
		"full_name":  username,
	}
	path := "/admin/users/" + giteaUserPathSegment(username)
	code, raw, err := giteaDo(http.MethodPatch, path, payload, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("patch user %s email: HTTP %d: %s", username, code, truncateGiteaBody(string(raw)))
	}
	return nil
}

func toTestUser(user *giteaUser, password string) *giteaTestUser {
	if user == nil {
		return nil
	}
	return &giteaTestUser{
		Login:    user.Login,
		Email:    user.Email,
		Password: password,
		ID:       user.ID,
	}
}

func existingTestUser(login, wantEmail string, existing *giteaUser) (*giteaTestUser, error) {
	if existing == nil {
		return nil, errors.New("existing user is nil")
	}
	if strings.EqualFold(strings.TrimSpace(existing.Email), wantEmail) {
		if strings.TrimSpace(existing.Email) == "" {
			existing.Email = wantEmail
		}
		return toTestUser(existing, ""), nil
	}

	if err := updateGiteaUserEmail(login, wantEmail); err != nil {
		return nil, err
	}
	refreshed, found, err := getGiteaUser(login)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("user %s disappeared after email update", login)
	}
	if strings.TrimSpace(refreshed.Email) == "" {
		refreshed.Email = wantEmail
	}
	return toTestUser(refreshed, ""), nil
}

func createNewTestUser(login, wantEmail string) (*giteaTestUser, error) {
	password, err := randomPassword()
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"username":             login,
		"email":                wantEmail,
		"password":             password,
		"must_change_password": false,
		"source_id":            0,
		"login_name":           login,
		"full_name":            login,
	}
	var created giteaUser
	code, raw, err := giteaDo(http.MethodPost, "/admin/users", payload, &created)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		if strings.TrimSpace(created.Email) == "" {
			created.Email = wantEmail
		}
		return toTestUser(&created, password), nil
	case http.StatusUnprocessableEntity:
		existing, found, lookupErr := getGiteaUser(login)
		if lookupErr == nil && found {
			if strings.TrimSpace(existing.Email) == "" {
				existing.Email = wantEmail
			}
			return toTestUser(existing, ""), nil
		}
		return nil, fmt.Errorf("create test user %s: HTTP 422: %s", login, truncateGiteaBody(string(raw)))
	default:
		return nil, fmt.Errorf("create test user %s: HTTP %d: %s", login, code, truncateGiteaBody(string(raw)))
	}
}

// CreateTestUser creates or reuses a dedicated Gitea user for one e2e repo.
// The helper is intentionally idempotent so reruns against an already-created
// repo name succeed without manual cleanup.
func CreateTestUser(login string) (*giteaTestUser, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, errors.New("login is empty")
	}

	wantEmail := testUserEmail(login)
	existing, found, err := getGiteaUser(login)
	if err != nil {
		return nil, err
	}
	if found {
		return existingTestUser(login, wantEmail, existing)
	}
	return createNewTestUser(login, wantEmail)
}

// EnsureRepoCollaborator grants the per-repo user write access to the repo.
func EnsureRepoCollaborator(owner, repo string, user *giteaTestUser) error {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" {
		return errors.New("owner is empty")
	}
	if repo == "" {
		return errors.New("repo is empty")
	}
	if user == nil || strings.TrimSpace(user.Login) == "" {
		return errors.New("user login is empty")
	}

	path := fmt.Sprintf(
		"/repos/%s/%s/collaborators/%s",
		giteaUserPathSegment(owner),
		giteaUserPathSegment(repo),
		giteaUserPathSegment(user.Login),
	)
	payload := map[string]string{"permission": "write"}
	code, raw, err := giteaDo(http.MethodPut, path, payload, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("ensure collaborator %s on %s/%s: HTTP %d: %s",
			user.Login, owner, repo, code, truncateGiteaBody(string(raw)))
	}
	return nil
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

func listUserPublicKeys(username string) ([]giteaPublicKey, error) {
	var keys []giteaPublicKey
	path := "/users/" + giteaUserPathSegment(username) + "/keys"
	code, raw, err := giteaDo(http.MethodGet, path, nil, &keys)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list user keys for %s: HTTP %d: %s", username, code, truncateGiteaBody(string(raw)))
	}
	return keys, nil
}

// ListUserPublicKeys returns the authenticated admin user's registered public
// keys. It remains available for the transport-SSH bootstrap flow.
func ListUserPublicKeys() ([]giteaPublicKey, error) {
	adminUser, _ := giteaAdminCreds()
	return listUserPublicKeys(adminUser)
}

// FindUserPublicKeyByKey looks up the authenticated admin user's public key by
// key material, ignoring the trailing comment.
func FindUserPublicKeyByKey(publicKey string) (*giteaPublicKey, bool, error) {
	adminUser, _ := giteaAdminCreds()
	return findUserPublicKeyByKey(adminUser, publicKey)
}

func findUserPublicKeyByKey(username, publicKey string) (*giteaPublicKey, bool, error) {
	want := normalizeAuthorizedKey(publicKey)
	keys, err := listUserPublicKeys(username)
	if err != nil {
		return nil, false, err
	}
	for i := range keys {
		if normalizeAuthorizedKey(keys[i].Key) == want {
			return &keys[i], true, nil
		}
	}
	return nil, false, nil
}

// RegisterSigningPublicKey idempotently registers a public key for the admin
// user. It remains available for the existing transport-SSH setup.
func RegisterSigningPublicKey(publicKey, title string) (*giteaPublicKey, error) {
	adminUser, _ := giteaAdminCreds()
	return RegisterSigningPublicKeyAs(&giteaTestUser{Login: adminUser}, publicKey, title)
}

// RegisterSigningPublicKeyAs idempotently registers a public key on behalf of
// the provided Gitea user so SSH-signed commits can be resolved to that user.
func RegisterSigningPublicKeyAs(user *giteaTestUser, publicKey, title string) (*giteaPublicKey, error) {
	if user == nil || strings.TrimSpace(user.Login) == "" {
		return nil, errors.New("user login is empty")
	}

	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return nil, errors.New("public key is empty")
	}

	if existing, found, err := findUserPublicKeyByKey(user.Login, publicKey); err != nil {
		return nil, err
	} else if found {
		return existing, nil
	}

	payload := map[string]string{
		"title": title,
		"key":   publicKey,
	}
	path := "/admin/users/" + giteaUserPathSegment(user.Login) + "/keys"
	var created giteaPublicKey
	code, raw, err := giteaDo(http.MethodPost, path, payload, &created)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		return &created, nil
	case http.StatusUnprocessableEntity:
		if existing, found, lookupErr := findUserPublicKeyByKey(user.Login, publicKey); lookupErr == nil && found {
			return existing, nil
		}
		return nil, fmt.Errorf("register signing key for %s: HTTP 422: %s",
			user.Login, truncateGiteaBody(string(raw)))
	default:
		return nil, fmt.Errorf("register signing key for %s: HTTP %d: %s",
			user.Login, code, truncateGiteaBody(string(raw)))
	}
}

// GetCommitVerification fetches the repo commit API for the given SHA and
// returns its verification block.
func GetCommitVerification(owner, repo, sha string) (*giteaCommitVerification, error) {
	path := fmt.Sprintf(
		"/repos/%s/%s/git/commits/%s",
		giteaUserPathSegment(owner),
		giteaUserPathSegment(repo),
		giteaUserPathSegment(sha),
	)
	var resp giteaCommitResponse
	code, raw, err := giteaDo(http.MethodGet, path, nil, &resp)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("get commit %s/%s@%s: HTTP %d: %s",
			owner, repo, sha, code, truncateGiteaBody(string(raw)))
	}
	v := resp.Commit.Verification
	return &v, nil
}
