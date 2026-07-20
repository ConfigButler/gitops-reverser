// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// keysPageJSON renders one page of the fake keys listing: ids [start, end) of `total`, where
// the LAST key of the whole set carries lastTitle so a title-search must reach the final page.
func keysPageJSON(start, end, total int, lastTitle string) string {
	var b strings.Builder
	b.WriteString("[")
	for i := start; i < end; i++ {
		if i > start {
			b.WriteString(",")
		}
		title := fmt.Sprintf("key-%d", i)
		if i == total-1 {
			title = lastTitle
		}
		fmt.Fprintf(&b, `{"id":%d,"title":%q,"key":"ssh-ed25519 AAAA k%d","fingerprint":"fp%d"}`,
			i+1, title, i, i)
	}
	b.WriteString("]")
	return b.String()
}

// Gitea paginates key listings: DEFAULT_PAGING_NUM is 30 and MAX_RESPONSE_ITEMS is 50, and
// `ToCorrectPageSize` clamps a limit of 0 or less straight back to the default — so there is
// no server-side "return everything" mode for this endpoint. An unpaginated listing therefore
// goes blind past the first page, which silently breaks FindUserKeyByTitle and makes
// EnsureUserKeyAsAdmin fail with `422 Key title has been used` for a key that exists but
// cannot be seen (and so cannot be replaced).
//
// The fake below serves a key set larger than one page, with the sought title on the LAST
// page. Without the page loop the client only ever sees page 1 and the title is reported
// missing.
func TestClientListUserKeys_FollowsPaginationBeyondTheFirstPage(t *testing.T) {
	t.Parallel()

	const (
		pageSize  = 50
		total     = 124 // > 2 full pages, so a single-page read cannot pass by luck
		lastTitle = "E2E Transport Key playground"
	)
	var pagesServed []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := requireKeysPageRequest(t, r)
		pagesServed = append(pagesServed, strconv.Itoa(page))

		start := (page - 1) * pageSize
		w.Header().Set("Content-Type", "application/json")
		if start >= total {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(keysPageJSON(start, min(start+pageSize, total), total, lastTitle)))
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")

	keys, err := client.ListUserKeys(context.Background(), "giteaadmin")
	if err != nil {
		t.Fatalf("ListUserKeys() error = %v", err)
	}
	if len(keys) != total {
		t.Fatalf("ListUserKeys() returned %d keys, want all %d — pagination was not followed", len(keys), total)
	}
	if len(pagesServed) != 3 {
		t.Fatalf("pages requested = %v, want 3 (two full pages plus a short final page)", pagesServed)
	}

	// The point of the loop: a title on a later page must still be findable, because that is
	// what EnsureUserKeyAsAdmin needs in order to replace a stale key instead of colliding.
	found, ok, err := client.FindUserKeyByTitle(context.Background(), "giteaadmin", lastTitle)
	if err != nil {
		t.Fatalf("FindUserKeyByTitle() error = %v", err)
	}
	if !ok {
		t.Fatal("FindUserKeyByTitle() did not find a key that exists on a later page")
	}
	if found.ID != total {
		t.Fatalf("FindUserKeyByTitle() = id %d, want %d", found.ID, total)
	}
}

// requireKeysPageRequest asserts the request is an explicitly paginated keys read and returns
// the requested page. An unpaginated read is the bug this file exists to prevent, so it fails
// here rather than silently truncating at the server default.
func requireKeysPageRequest(t *testing.T, r *http.Request) int {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/users/giteaadmin/keys" {
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}
	query := r.URL.Query()
	page := query.Get("page")
	if page == "" {
		t.Fatal("ListUserKeys must request an explicit page; an unpaginated read silently truncates at 30")
	}
	if got := query.Get("limit"); got != "50" {
		t.Fatalf("limit = %q, want 50 (Gitea's MAX_RESPONSE_ITEMS; a larger value is silently clamped)", got)
	}
	n, err := strconv.Atoi(page)
	if err != nil {
		t.Fatalf("bad page parameter %q: %v", page, err)
	}
	return n
}

// A listing that exactly fills a page must still terminate: the client asks for one more page,
// gets an empty body, and stops. Without that, an exact multiple would either loop forever or
// drop the tail.
func TestClientListUserKeys_ExactPageMultipleTerminates(t *testing.T) {
	t.Parallel()

	const pageSize = 50
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 5 {
			t.Fatal("ListUserKeys did not terminate on an empty page")
		}
		page := requireKeysPageRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		if page != 1 {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(keysPageJSON(0, pageSize, pageSize+1, "unused")))
	}))
	defer server.Close()

	keys, err := New(server.URL, "admin", "password").ListUserKeys(context.Background(), "giteaadmin")
	if err != nil {
		t.Fatalf("ListUserKeys() error = %v", err)
	}
	if len(keys) != pageSize {
		t.Fatalf("ListUserKeys() returned %d keys, want %d", len(keys), pageSize)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2 (the full page, then the empty page that ends the loop)", requests)
	}
}
