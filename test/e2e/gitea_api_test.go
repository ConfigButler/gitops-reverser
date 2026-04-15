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
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
)

type giteaPublicKey = giteaclient.PublicKey
type giteaCommitVerification = giteaclient.CommitVerification
type giteaTestUser = giteaclient.TestUser

const giteaRequestTimeout = 15 * time.Second

func giteaAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("GITEA_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:13000/api/v1"
}

func giteaWebBase() string {
	return strings.TrimSuffix(strings.TrimRight(giteaAPIBase(), "/"), "/api/v1")
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

func testUserEmail(login string) string {
	return login + "@configbutler.test"
}

func giteaAdminClient() *giteaclient.Client {
	user, pass := giteaAdminCreds()
	return giteaclient.New(giteaAPIBase(), user, pass)
}

func giteaContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), giteaRequestTimeout)
}

// CreateTestUser creates or reuses a dedicated Gitea user for one e2e repo.
// The helper is intentionally idempotent so reruns against an already-created
// repo name succeed without manual cleanup.
func CreateTestUser(login string) (*giteaTestUser, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, errors.New("login is empty")
	}

	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().EnsureUser(ctx, login, testUserEmail(login))
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

	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().EnsureCollaborator(ctx, owner, repo, user.Login)
}

// normalizeAuthorizedKey returns the "<type> <base64>" prefix of an
// authorized_keys entry, dropping any comment.
func normalizeAuthorizedKey(k string) string {
	return giteaclient.NormalizeAuthorizedKey(k)
}

func listUserPublicKeys(username string) ([]giteaPublicKey, error) {
	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().ListUserKeys(ctx, username)
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
	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().FindUserKey(ctx, username, publicKey)
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

	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().RegisterUserKeyAsAdmin(ctx, user.Login, title, publicKey)
}

// GetCommitVerification fetches the repo commit API for the given SHA and
// returns its verification block.
func GetCommitVerification(owner, repo, sha string) (*giteaCommitVerification, error) {
	ctx, cancel := giteaContext()
	defer cancel()

	return giteaAdminClient().GetCommitVerification(ctx, owner, repo, sha)
}
