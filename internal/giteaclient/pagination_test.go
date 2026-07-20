// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// TestListPaginated_PropagatesRequestFailures keeps transport and non-200 errors observable to callers.
func TestListPaginated_PropagatesRequestFailures(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
		want      string
	}{
		{
			name: "transport error",
			transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			}),
			want: "connection refused",
		},
		{
			name: "unexpected status",
			transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader("rate limited")),
					Header:     make(http.Header),
				}, nil
			}),
			want: "HTTP 429: rate limited",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := New("https://gitea.example.test/api/v1", "", "")
			client.HTTPClient = &http.Client{Transport: tc.transport}

			_, err := listPaginated[string](context.Background(), client, "/items", 50, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("listPaginated() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
