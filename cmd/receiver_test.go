// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlags_ReceiverIsOffByDefault(t *testing.T) {
	cfg, err := parseFlagsWithArgs(flag.NewFlagSet("receiver-default", flag.ContinueOnError), nil)
	require.NoError(t, err)
	assert.Equal(t, "0", cfg.receiverBindAddress)
	assert.False(t, receiverEnabled(cfg))
}

func TestParseFlags_ReceiverBindAddress(t *testing.T) {
	tests := map[string]struct {
		value       string
		wantEnabled bool
		wantErr     string
	}{
		"port only":         {value: ":9292", wantEnabled: true},
		"host and port":     {value: "0.0.0.0:9292", wantEnabled: true},
		"explicit off":      {value: "0"},
		"empty is off":      {value: ""},
		"missing port":      {value: "localhost", wantErr: "invalid receiver-bind-address"},
		"non-numeric port":  {value: ":http", wantErr: "invalid receiver-bind-address"},
		"zero port refused": {value: ":0", wantErr: "invalid receiver-bind-address"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseFlagsWithArgs(flag.NewFlagSet("receiver", flag.ContinueOnError),
				[]string{"--receiver-bind-address=" + tc.value})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantEnabled, receiverEnabled(cfg))
		})
	}
}

// TestReceiverServerRunnable_ServesOnlyThePrefixAndShutsDown runs the listener for real: only
// /git-push/ reaches the handler, and cancelling the context is a clean stop.
func TestReceiverServerRunnable_ServesOnlyThePrefixAndShutsDown(t *testing.T) {
	var gotPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})
	r := newReceiverServerRunnable("127.0.0.1:0", handler)
	assert.True(t, r.NeedLeaderElection(), "a delivery drives a branch worker, which only the leader runs")
	assert.Equal(t, receiverReadHeaderTimeout, r.server.ReadHeaderTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	require.Eventually(t, func() bool { return r.boundAddr.Load() != nil }, 2*time.Second, 5*time.Millisecond)
	base := "http://" + *r.boundAddr.Load()

	resp, err := http.Post(base+"/git-push/team-a/repo", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, "/git-push/team-a/repo", gotPath)

	resp, err = http.Post(base+"/audit-webhook/x", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the receiver listener serves nothing but /git-push/")

	cancel()
	require.NoError(t, <-done, "graceful shutdown returns nil")
	assert.Nil(t, r.boundAddr.Load())
}

func TestReceiverServerRunnable_BindFailureIsAnError(t *testing.T) {
	occupied := httptest.NewServer(http.NotFoundHandler())
	defer occupied.Close()

	r := newReceiverServerRunnable(strings.TrimPrefix(occupied.URL, "http://"), http.NotFoundHandler())
	err := r.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "push receiver failed to bind")
}
