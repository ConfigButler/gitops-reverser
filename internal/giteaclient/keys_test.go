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
	"net/http/httptest"
	"testing"
)

func TestClientEnsureUserKeyAsAdmin_ReplacesExistingKeyWithSameTitle(t *testing.T) {
	t.Parallel()

	var (
		deleteCalled bool
		createCalled bool
	)

	server := httptest.NewServer(newEnsureUserKeyAsAdminTestHandler(t, &deleteCalled, &createCalled))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	key, err := client.EnsureUserKeyAsAdmin(
		context.Background(),
		"giteaadmin",
		"E2E Transport Key playground",
		"ssh-rsa new-key",
	)
	if err != nil {
		t.Fatalf("EnsureUserKeyAsAdmin() error = %v", err)
	}
	if key == nil || key.ID != 9 {
		t.Fatalf("EnsureUserKeyAsAdmin() = %#v, want created key id 9", key)
	}
	if !deleteCalled {
		t.Fatal("expected stale key to be deleted before create")
	}
	if !createCalled {
		t.Fatal("expected replacement key to be created")
	}
}

func newEnsureUserKeyAsAdminTestHandler(
	t *testing.T,
	deleteCalled, createCalled *bool,
) http.Handler {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/users/giteaadmin/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if !*deleteCalled {
			_, _ = w.Write([]byte(
				`[{"id":7,"title":"E2E Transport Key playground","key":"ssh-rsa old-key","fingerprint":"old"}]`,
			))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/admin/users/giteaadmin/keys/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		*deleteCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/admin/users/giteaadmin/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		*createCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(
			`{"id":9,"title":"E2E Transport Key playground","key":"ssh-rsa new-key","fingerprint":"new"}`,
		))
	})
	return mux
}

func TestClientFindUserKeyByTitle_ReturnsMatch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/giteaadmin/keys" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`[` +
				`{"id":1,"title":"first","key":"ssh-ed25519 AAAA first","fingerprint":"fp1"},` +
				`{"id":2,"title":"second","key":"ssh-ed25519 AAAA second","fingerprint":"fp2"}` +
				`]`,
		))
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	key, found, err := client.FindUserKeyByTitle(context.Background(), "giteaadmin", "second")
	if err != nil {
		t.Fatalf("FindUserKeyByTitle() error = %v", err)
	}
	if !found {
		t.Fatal("FindUserKeyByTitle() found = false, want true")
	}
	if key == nil || key.ID != 2 {
		t.Fatalf("FindUserKeyByTitle() = %#v, want key id 2", key)
	}
}
