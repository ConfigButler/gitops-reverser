// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// EnsureOrg creates an organization when missing.
func (c *Client) EnsureOrg(ctx context.Context, name, fullName, description string) (*Organization, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("organization name is empty")
	}

	payload := map[string]string{
		"username":    name,
		"full_name":   fullName,
		"description": description,
	}

	var org Organization
	code, raw, err := c.Do(ctx, http.MethodPost, "/orgs", payload, &org)
	if err != nil {
		return nil, err
	}

	switch code {
	case http.StatusCreated:
		return &org, nil
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return c.GetOrg(ctx, name)
	default:
		return nil, unexpectedStatus(http.MethodPost, "/orgs", code, raw)
	}
}

// GetOrg fetches an organization by name.
func (c *Client) GetOrg(ctx context.Context, name string) (*Organization, error) {
	path := "/orgs/" + PathEscape(name)
	var org Organization
	code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &org)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, unexpectedStatus(http.MethodGet, path, code, raw)
	}
	return &org, nil
}

// CreateAccessToken creates a token on the named user and returns the secret value.
func (c *Client) CreateAccessToken(ctx context.Context, login, name string, scopes []string) (string, error) {
	login = strings.TrimSpace(login)
	name = strings.TrimSpace(name)
	if login == "" {
		return "", errors.New("login is empty")
	}
	if name == "" {
		return "", errors.New("token name is empty")
	}

	payload := map[string]any{
		"name":   name,
		"scopes": scopes,
	}
	path := "/users/" + PathEscape(login) + "/tokens"
	var token AccessToken
	code, raw, err := c.Do(ctx, http.MethodPost, path, payload, &token)
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated {
		return "", unexpectedStatus(http.MethodPost, path, code, raw)
	}
	if strings.TrimSpace(token.SHA1) == "" {
		return "", errors.New("token creation response did not include sha1")
	}
	return strings.TrimSpace(token.SHA1), nil
}

// EnsureOrgRepo creates an organization-owned repository when missing.
func (c *Client) EnsureOrgRepo(
	ctx context.Context,
	org,
	name,
	description string,
	private,
	autoInit bool,
) (*Repository, error) {
	org = strings.TrimSpace(org)
	name = strings.TrimSpace(name)
	if org == "" || name == "" {
		return nil, errors.New("organization and repository name must be non-empty")
	}

	payload := map[string]any{
		"name":        name,
		"description": description,
		"private":     private,
		"auto_init":   autoInit,
	}

	path := "/orgs/" + PathEscape(org) + "/repos"
	var repo Repository
	code, raw, err := c.Do(ctx, http.MethodPost, path, payload, &repo)
	if err != nil {
		return nil, err
	}

	switch code {
	case http.StatusCreated:
		return &repo, nil
	case http.StatusConflict, http.StatusUnprocessableEntity:
		return c.GetRepo(ctx, org, name)
	default:
		return nil, unexpectedStatus(http.MethodPost, path, code, raw)
	}
}

// hookPageSize / maxHookPages bound the webhook listing. /repos/{owner}/{repo}/hooks paginates
// exactly like the keys endpoint — it is clamped to MAX_RESPONSE_ITEMS and has no "return
// everything" mode — so an unpaginated read goes blind past the first page. Latent rather than
// live today only because e2e creates a fresh repo per spec and never accumulates a page of
// hooks on one repo; the callers that delete hooks would silently miss the rest.
const (
	hookPageSize = 50
	maxHookPages = 200
)

// ListRepoHooks returns every webhook configured on owner/repo, following pagination.
func (c *Client) ListRepoHooks(ctx context.Context, owner, repo string) ([]RepoHook, error) {
	base := "/repos/" + PathEscape(owner) + "/" + PathEscape(repo) + "/hooks"
	return listPaginated[RepoHook](ctx, c, base, hookPageSize, maxHookPages)
}

// DeleteRepoHook removes a single repository webhook. Missing hooks are treated as success.
func (c *Client) DeleteRepoHook(ctx context.Context, owner, repo string, hookID int64) error {
	path := "/repos/" + PathEscape(owner) + "/" + PathEscape(repo) + "/hooks/" + strconv.FormatInt(hookID, 10)
	code, raw, err := c.Do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return unexpectedStatus(http.MethodDelete, path, code, raw)
	}
	return nil
}

// CreateGiteaWebhook creates a repository webhook of type "gitea".
func (c *Client) CreateGiteaWebhook(
	ctx context.Context,
	owner,
	repo,
	callbackURL,
	secret string,
	events []string,
) (*RepoHook, error) {
	path := "/repos/" + PathEscape(owner) + "/" + PathEscape(repo) + "/hooks"
	payload := map[string]any{
		"type":   "gitea",
		"active": true,
		"events": events,
		"config": map[string]string{
			"url":          callbackURL,
			"content_type": "json",
			"secret":       secret,
		},
	}

	var hook RepoHook
	code, raw, err := c.Do(ctx, http.MethodPost, path, payload, &hook)
	if err != nil {
		return nil, err
	}
	if code != http.StatusCreated {
		return nil, unexpectedStatus(http.MethodPost, path, code, raw)
	}
	return &hook, nil
}
