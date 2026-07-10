// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func parseArgs(t *testing.T, args ...string) (appConfig, error) {
	t.Helper()
	return parseFlagsWithArgs(flag.NewFlagSet("manager", flag.ContinueOnError), args)
}

// The --redis-addr help text is a promise about which combinations start. Keep it true.
func TestParseFlags_RedisAddrIsOnlyRequiredByAttribution(t *testing.T) {
	tests := map[string]struct {
		args    []string
		wantErr string
	}{
		"attribution needs redis": {
			args:    []string{"--redis-addr=", "--author-attribution=true"},
			wantErr: "redis-addr is required when author-attribution is enabled",
		},
		// The webhook is failurePolicy: Ignore by design and the controller is the real gate,
		// so running it without Redis is a supported, degraded mode — not a usage error.
		// (--admission-webhook-cert-path has no default and is required whenever the webhook
		// is on; that requirement is about TLS, not about Redis.)
		"admission webhook runs without redis": {
			args: []string{
				"--redis-addr=", "--author-attribution=false",
				"--admission-webhook", "--admission-webhook-cert-path=/tmp/certs",
			},
		},
		"admission webhook still requires its cert path": {
			args:    []string{"--redis-addr=", "--author-attribution=false", "--admission-webhook"},
			wantErr: "admission-webhook-cert-path is required",
		},
		"configured-author runs without redis": {
			args: []string{"--redis-addr=", "--author-attribution=false"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseArgs(t, tc.args...)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}
