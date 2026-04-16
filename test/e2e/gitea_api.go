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

const giteaRequestTimeout = 15 * time.Second

// GiteaTestInstance describes the live Gitea instance the e2e suite uses for
// repo fixtures, signing-key registration, and commit verification.
type GiteaTestInstance struct {
	APIBaseURL     string
	Org            string
	AdminUser      string
	AdminPassword  string
	RequestTimeout time.Duration
}

func giteaTestInstance() *GiteaTestInstance {
	apiBaseURL := strings.TrimSpace(os.Getenv("GITEA_API_URL"))
	if apiBaseURL == "" {
		apiBaseURL = "http://localhost:13000/api/v1"
	}

	adminUser := strings.TrimSpace(os.Getenv("GITEA_ADMIN_USER"))
	if adminUser == "" {
		adminUser = "giteaadmin"
	}

	adminPassword := strings.TrimSpace(os.Getenv("GITEA_ADMIN_PASS"))
	if adminPassword == "" {
		adminPassword = "giteapassword123"
	}

	org := strings.TrimSpace(os.Getenv("ORG_NAME"))
	if org == "" {
		org = "testorg"
	}

	return &GiteaTestInstance{
		APIBaseURL:     strings.TrimRight(apiBaseURL, "/"),
		Org:            org,
		AdminUser:      adminUser,
		AdminPassword:  adminPassword,
		RequestTimeout: giteaRequestTimeout,
	}
}

func (g *GiteaTestInstance) Client() *giteaclient.Client {
	return giteaclient.New(g.APIBaseURL, g.AdminUser, g.AdminPassword)
}

func (g *GiteaTestInstance) Context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), g.RequestTimeout)
}

func (g *GiteaTestInstance) TestUserEmail(login string) string {
	return strings.TrimSpace(login) + "@configbutler.test"
}

func (g *GiteaTestInstance) EnsureTestUser(login string) (*giteaclient.TestUser, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, errors.New("login is empty")
	}

	ctx, cancel := g.Context()
	defer cancel()

	return g.Client().EnsureUser(ctx, login, g.TestUserEmail(login))
}

func (g *GiteaTestInstance) EnsureRepoCollaborator(owner, repo string, user *giteaclient.TestUser) error {
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

	ctx, cancel := g.Context()
	defer cancel()

	return g.Client().EnsureCollaborator(ctx, owner, repo, user.Login)
}

func (g *GiteaTestInstance) RegisterSigningPublicKey(
	user *giteaclient.TestUser,
	publicKey, title string,
) (*giteaclient.PublicKey, error) {
	if user == nil || strings.TrimSpace(user.Login) == "" {
		return nil, errors.New("user login is empty")
	}

	ctx, cancel := g.Context()
	defer cancel()

	return g.Client().RegisterUserKeyAsAdmin(ctx, user.Login, title, publicKey)
}

func (g *GiteaTestInstance) CommitVerification(owner, repo, sha string) (*giteaclient.CommitVerification, error) {
	ctx, cancel := g.Context()
	defer cancel()

	return g.Client().GetCommitVerification(ctx, owner, repo, sha)
}
