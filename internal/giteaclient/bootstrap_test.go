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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientEnsureOrg_ReturnsExistingOnConflict(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/orgs":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"already exists"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/testorg":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7,"username":"testorg","full_name":"Test Organization"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	org, err := client.EnsureOrg(context.Background(), "testorg", "Test Organization", "desc")
	if err != nil {
		t.Fatalf("EnsureOrg() error = %v", err)
	}
	if org == nil {
		t.Fatal("EnsureOrg() returned nil org")
	}
	if org.UserName != "testorg" {
		t.Fatalf("EnsureOrg() username = %q, want testorg", org.UserName)
	}
}

func TestClientCreateAccessToken_ReturnsSHA1(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/users/giteaadmin/tokens" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":3,"name":"e2e-token","sha1":"abc123"}`))
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	token, err := client.CreateAccessToken(
		context.Background(),
		"giteaadmin",
		"e2e-token",
		[]string{"read:repository", "write:repository"},
	)
	if err != nil {
		t.Fatalf("CreateAccessToken() error = %v", err)
	}
	if token != "abc123" {
		t.Fatalf("CreateAccessToken() token = %q, want abc123", token)
	}
}

func TestClientEnsureOrgRepo_UsesOrgEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/orgs/testorg/repos" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(
			`{"id":9,"name":"demo","full_name":"testorg/demo",` +
				`"clone_url":"http://example/demo.git","ssh_url":"ssh://example/demo.git"}`,
		))
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	repo, err := client.EnsureOrgRepo(context.Background(), "testorg", "demo", "desc", false, false)
	if err != nil {
		t.Fatalf("EnsureOrgRepo() error = %v", err)
	}
	if repo == nil {
		t.Fatal("EnsureOrgRepo() returned nil repo")
	}
	if repo.FullName != "testorg/demo" {
		t.Fatalf("EnsureOrgRepo() full_name = %q, want testorg/demo", repo.FullName)
	}
}

func TestClientCreateGiteaWebhook_SendsExpectedPayload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(webhookTestHandler(t))
	defer server.Close()

	client := New(server.URL, "admin", "password")

	hooks, err := client.ListRepoHooks(context.Background(), "testorg", "demo")
	if err != nil {
		t.Fatalf("ListRepoHooks() error = %v", err)
	}
	if len(hooks) != 1 || hooks[0].ID != 42 {
		t.Fatalf("ListRepoHooks() hooks = %#v, want single hook id 42", hooks)
	}

	hook, err := client.CreateGiteaWebhook(
		context.Background(),
		"testorg",
		"demo",
		"http://receiver.example/hook/demo",
		"receiver-token",
		[]string{"push", "create", "delete"},
	)
	if err != nil {
		t.Fatalf("CreateGiteaWebhook() error = %v", err)
	}
	if hook == nil || hook.ID != 42 {
		t.Fatalf("CreateGiteaWebhook() = %#v, want hook id 42", hook)
	}

	if err := client.DeleteRepoHook(context.Background(), "testorg", "demo", 42); err != nil {
		t.Fatalf("DeleteRepoHook() error = %v", err)
	}
}

func webhookTestHandler(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/testorg/demo/hooks":
			assertWebhookPayload(t, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(
				`{"id":42,"type":"gitea","active":true,` +
					`"config":{"url":"http://receiver.example/hook/demo","content_type":"json"}}`,
			))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/testorg/demo/hooks/42":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/demo/hooks":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`[{"id":42,"type":"gitea","active":true,` +
					`"config":{"url":"http://receiver.example/hook/demo","content_type":"json"}}]`,
			))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}

func assertWebhookPayload(t *testing.T, r *http.Request) {
	t.Helper()

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if payload["type"] != "gitea" {
		t.Fatalf("hook type = %v, want gitea", payload["type"])
	}

	config, ok := payload["config"].(map[string]any)
	if !ok {
		t.Fatalf("config payload missing or wrong type: %T", payload["config"])
	}
	if config["url"] != "http://receiver.example/hook/demo" {
		t.Fatalf("hook url = %v", config["url"])
	}
	if config["secret"] != "receiver-token" {
		t.Fatalf("hook secret = %v", config["secret"])
	}
}
