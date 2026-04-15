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

// CreateUserRepo creates a repository owned by the named user via the admin API.
// If the repo already exists, it is returned as-is.
//
// trustModel must be one of: "default", "collaborator", "committer",
// "collaboratorcommitter", or "" to let Gitea pick the instance default.
// For verifying user-signed commits, "committer" is the relevant value.
func (c *Client) CreateUserRepo(
	ctx context.Context,
	owner, name string,
	autoInit bool,
	trustModel string,
) (*Repository, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return nil, errors.New("owner and name must be non-empty")
	}
	payload := map[string]any{
		"name":           name,
		"auto_init":      autoInit,
		"default_branch": "main",
		"private":        false,
	}
	if trustModel != "" {
		payload["trust_model"] = trustModel
	}
	path := "/admin/users/" + PathEscape(owner) + "/repos"
	var repo Repository
	code, raw, err := c.Do(ctx, http.MethodPost, path, payload, &repo)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusCreated:
		return &repo, nil
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return c.GetRepo(ctx, owner, name)
	default:
		return nil, unexpectedStatus(http.MethodPost, path, code, raw)
	}
}

// GetRepo fetches a repository by owner and name.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (*Repository, error) {
	path := "/repos/" + PathEscape(owner) + "/" + PathEscape(name)
	var repo Repository
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &repo)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, unexpectedStatus(http.MethodGet, path, code, raw)
	}
	return &repo, nil
}

// DeleteRepo removes a repository. Missing repos are treated as success.
func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error {
	path := "/repos/" + PathEscape(owner) + "/" + PathEscape(name)
	code, raw, err := c.Do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return unexpectedStatus(http.MethodDelete, path, code, raw)
	}
	return nil
}

// EnsureCollaborator grants the named user write access on owner/repo.
func (c *Client) EnsureCollaborator(ctx context.Context, owner, repo, user string) error {
	path := fmt.Sprintf("/repos/%s/%s/collaborators/%s",
		PathEscape(owner), PathEscape(repo), PathEscape(user))
	code, raw, err := c.Do(ctx, http.MethodPut, path, map[string]string{"permission": "write"}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return unexpectedStatus(http.MethodPut, path, code, raw)
	}
	return nil
}

// GetCommitVerification returns the verification block for a commit.
func (c *Client) GetCommitVerification(ctx context.Context, owner, repo, sha string) (*CommitVerification, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/commits/%s",
		PathEscape(owner), PathEscape(repo), PathEscape(sha))
	var resp CommitResponse
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, unexpectedStatus(http.MethodGet, path, code, raw)
	}
	v := resp.Commit.Verification
	return &v, nil
}
