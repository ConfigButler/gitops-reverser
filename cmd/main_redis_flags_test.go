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
		"attribution over the redis transport needs redis": {
			args:    []string{"--redis-addr=", "--author-attribution=true"},
			wantErr: "redis-addr is required when author-attribution is enabled",
		},
		// The narrowed rule, and the whole reason the transport is selectable: attribution needs a
		// fact TRANSPORT, not Redis specifically. In-memory plus an empty address is a supported
		// configuration rather than a rejected one, or the mode would be unreachable.
		"attribution over the memory transport runs without redis": {
			args: []string{
				"--redis-addr=", "--author-attribution=true",
				"--author-attribution-transport=memory",
				"--audit-insecure",
			},
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

// TestParseFlags_AttributionTransportSelection pins the guard rails around the transport choice.
// The replica gate is the one that matters: the in-memory transport carries facts only within one
// process, so under more than one replica an audit POST answered by pod A leaves pod B's watch with
// nothing to join. That has to be a startup error, because the alternative is commits that are
// silently authored "attribution unresolved" and an operator with no way to tell why.
func TestParseFlags_AttributionTransportSelection(t *testing.T) {
	tests := map[string]struct {
		args    []string
		wantErr string
	}{
		"memory with one replica is fine": {
			args: []string{"--author-attribution-transport=memory", "--replica-count=1", "--audit-insecure"},
		},
		"memory with more than one replica is refused": {
			args:    []string{"--author-attribution-transport=memory", "--replica-count=2"},
			wantErr: "author-attribution-transport=memory requires a single replica",
		},
		// Redis carries facts between processes, so the replica count is not its business.
		"redis with more than one replica is not this check's business": {
			args: []string{"--author-attribution-transport=redis", "--replica-count=2", "--audit-insecure"},
		},
		"an unknown transport is refused": {
			args:    []string{"--author-attribution-transport=kafka"},
			wantErr: `author-attribution-transport must be "redis" or "memory", got "kafka"`,
		},
		// With attribution off there is no transport in play, so an unknown value is still a typo
		// worth catching, but the replica pairing is not checked.
		"memory with many replicas is moot when attribution is off": {
			args: []string{
				"--author-attribution=false", "--author-attribution-transport=memory", "--replica-count=3",
			},
		},
		"a total cap below the per-type cap makes the per-type cap unreachable": {
			args: []string{
				"--author-attribution-max-facts-per-type=4096", "--author-attribution-max-facts=1024",
			},
			wantErr: "must be >= author-attribution-max-facts-per-type",
		},
		"a zero collection window is refused": {
			args:    []string{"--author-attribution-collection-window=0"},
			wantErr: "author-attribution-collection-window must be > 0",
		},
		"a zero uid cap is refused": {
			args:    []string{"--author-attribution-collection-uid-cap=0"},
			wantErr: "author-attribution-collection-uid-cap must be > 0",
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
