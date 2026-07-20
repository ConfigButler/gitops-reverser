// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

	// These handlers report with t.Errorf, never t.Fatalf: Fatal from a non-test goroutine runs
	// runtime.Goexit on the handler, killing the connection mid-response, so the client reports a
	// spurious EOF on top of the real failure.
	mux := http.NewServeMux()
	mux.HandleFunc("/users/giteaadmin/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Honour the page parameter: everything past the first page is empty, which is what
		// terminates ListUserKeys.
		if r.URL.Query().Get("page") != "1" || *deleteCalled {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(
			`[{"id":7,"title":"E2E Transport Key playground","key":"ssh-rsa old-key","fingerprint":"old"}]`,
		))
	})
	mux.HandleFunc("/admin/users/giteaadmin/keys/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*deleteCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/admin/users/giteaadmin/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
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
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path) // never Fatal off the test goroutine
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`)) // the empty page that terminates the listing
			return
		}
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

// keysServer is a fake /users/{login}/keys endpoint that records what the client asked for and
// what it got wrong.
//
// Handler goroutine failures are RECORDED, not raised: calling t.Fatal from an httptest handler
// runs runtime.Goexit on the handler goroutine, which kills the connection mid-response and
// surfaces a spurious `EOF` from the client on top of the real message — two reported errors for
// one fault, with the misleading one first. Assertions run on the test goroutine via check().
type keysServer struct {
	mu sync.Mutex
	// serverPageSize is what the server actually returns per page, regardless of the limit the
	// client requests. Gitea's ToCorrectPageSize clamps to MAX_RESPONSE_ITEMS, which operators
	// can set below our requested keyPageSize.
	serverPageSize int
	total          int
	lastTitle      string
	// ignorePageParam models a server that serves page 1 forever.
	ignorePageParam bool
	pagesServed     []int
	problems        []string
}

func (s *keysServer) failf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

// check surfaces recorded handler problems on the test goroutine, where t.Fatal is safe.
func (s *keysServer) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.problems) > 0 {
		t.Fatalf("fake keys server rejected the client's requests:\n  %s", strings.Join(s.problems, "\n  "))
	}
}

func (s *keysServer) requests() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pagesServed...)
}

// requireKeysPageRequest validates the request is an explicitly paginated keys read and returns
// the requested page. An unpaginated read is the bug this file exists to prevent.
func (s *keysServer) requireKeysPageRequest(r *http.Request) (int, bool) {
	if r.Method != http.MethodGet || r.URL.Path != "/users/giteaadmin/keys" {
		s.failf("unexpected request: %s %s", r.Method, r.URL.Path)
		return 0, false
	}
	query := r.URL.Query()
	page := query.Get("page")
	if page == "" {
		s.failf("ListUserKeys must request an explicit page; an unpaginated read silently truncates at 30")
		return 0, false
	}
	if got := query.Get("limit"); got != strconv.Itoa(keyPageSize) {
		s.failf("limit = %q, want %d", got, keyPageSize)
		return 0, false
	}
	n, err := strconv.Atoi(page)
	if err != nil {
		s.failf("bad page parameter %q: %v", page, err)
		return 0, false
	}
	return n, true
}

func (s *keysServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := s.requireKeysPageRequest(r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.pagesServed = append(s.pagesServed, page)
		tooMany := len(s.pagesServed) > maxKeyPages+1
		s.mu.Unlock()
		if tooMany {
			s.failf("client exceeded the maxKeyPages cap")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if s.ignorePageParam {
			page = 1
		}
		start := (page - 1) * s.serverPageSize
		w.Header().Set("Content-Type", "application/json")
		if start >= s.total {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(keysPageJSON(start, min(start+s.serverPageSize, s.total), s.total, s.lastTitle)))
	})
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
		total     = 124 // > 2 full pages, so a single-page read cannot pass by luck
		lastTitle = "E2E Transport Key playground"
	)
	fake := &keysServer{serverPageSize: keyPageSize, total: total, lastTitle: lastTitle}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := New(server.URL, "admin", "password")

	keys, err := client.ListUserKeys(context.Background(), "giteaadmin")
	fake.check(t)
	if err != nil {
		t.Fatalf("ListUserKeys() error = %v", err)
	}
	if len(keys) != total {
		t.Fatalf("ListUserKeys() returned %d keys, want all %d — pagination was not followed", len(keys), total)
	}
	// Four requests, not three: termination is an EMPTY page, so the 24-key page 3 does not end
	// the loop. That extra round trip is the price of being correct for any server page size.
	if got := fake.requests(); len(got) != 4 {
		t.Fatalf("pages requested = %v, want 4 (three pages of keys, then the empty page that ends it)", got)
	}

	// The point of the loop: a title on a later page must still be findable, because that is
	// what EnsureUserKeyAsAdmin needs in order to replace a stale key instead of colliding.
	found, ok, err := client.FindUserKeyByTitle(context.Background(), "giteaadmin", lastTitle)
	fake.check(t)
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

// A server whose page size is SMALLER than the limit we request must still be paginated
// correctly. MAX_RESPONSE_ITEMS is operator-configurable and ToCorrectPageSize clamps every
// request down to it, so against such a server EVERY page is short — and a loop that stops on a
// short page stops after page 1, byte-for-byte re-creating the original bug (FindUserKeyByTitle
// missing an existing key, then `422 Key title has been used`). The e2e lab pins no [api] block
// in gitea-values.yaml, so today's pass depends only on an unpinned server default.
func TestClientListUserKeys_ServerPageSizeSmallerThanRequested(t *testing.T) {
	t.Parallel()

	const (
		serverPageSize = 30 // MAX_RESPONSE_ITEMS set below our requested keyPageSize
		total          = 100
		lastTitle      = "E2E Transport Key playground"
	)
	fake := &keysServer{serverPageSize: serverPageSize, total: total, lastTitle: lastTitle}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := New(server.URL, "admin", "password")

	keys, err := client.ListUserKeys(context.Background(), "giteaadmin")
	fake.check(t)
	if err != nil {
		t.Fatalf("ListUserKeys() error = %v", err)
	}
	if len(keys) != total {
		t.Fatalf("ListUserKeys() returned %d keys, want all %d — the loop stopped on a short page, "+
			"which is not the last page when the server clamps below the requested limit", len(keys), total)
	}

	_, ok, err := client.FindUserKeyByTitle(context.Background(), "giteaadmin", lastTitle)
	fake.check(t)
	if err != nil {
		t.Fatalf("FindUserKeyByTitle() error = %v", err)
	}
	if !ok {
		t.Fatal("FindUserKeyByTitle() did not find an existing key: this is the `422 Key title has " +
			"been used` failure the pagination exists to prevent")
	}
}

// A listing that exactly fills a page must still terminate: the client asks for one more page,
// gets an empty body, and stops.
func TestClientListUserKeys_ExactPageMultipleTerminates(t *testing.T) {
	t.Parallel()

	fake := &keysServer{serverPageSize: keyPageSize, total: keyPageSize, lastTitle: "unused"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	keys, err := New(server.URL, "admin", "password").ListUserKeys(context.Background(), "giteaadmin")
	fake.check(t)
	if err != nil {
		t.Fatalf("ListUserKeys() error = %v", err)
	}
	if len(keys) != keyPageSize {
		t.Fatalf("ListUserKeys() returned %d keys, want %d", len(keys), keyPageSize)
	}
	if got := fake.requests(); len(got) != 2 {
		t.Fatalf("requests = %v, want 2 (the full page, then the empty page that ends the loop)", got)
	}
}

// A server that ignores the `page` parameter serves a full first page forever. Without a cap the
// loop spins until the caller's context deadline and reports `context deadline exceeded`, which
// names neither the endpoint nor the cause. The cap turns it into a diagnosable error, bounded.
func TestClientListUserKeys_CapsPagesAgainstANonPaginatingServer(t *testing.T) {
	t.Parallel()

	fake := &keysServer{
		serverPageSize:  keyPageSize,
		total:           1_000_000,
		lastTitle:       "unused",
		ignorePageParam: true,
	}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, err := New(server.URL, "admin", "password").ListUserKeys(context.Background(), "giteaadmin")
	fake.check(t)
	if err == nil {
		t.Fatal("ListUserKeys() returned no error against a server that never paginates")
	}
	if !strings.Contains(err.Error(), "ignore the page parameter") {
		t.Fatalf("ListUserKeys() error = %v, want it to name the non-paginating server", err)
	}
	if got := len(fake.requests()); got != maxKeyPages {
		t.Fatalf("requests = %d, want exactly the %d-page cap", got, maxKeyPages)
	}
}
