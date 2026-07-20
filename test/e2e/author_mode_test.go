// SPDX-License-Identifier: Apache-2.0

package e2e

import "testing"

// The author-mode decision must be read from the Deployment's args, and both defaults are
// ENABLED in cmd/main.go — so an absent flag means attribution mode, and only an explicit
// opt-out selects configured-author mode. Getting this backwards silently flips the commit
// author assertion instead of failing it.
func TestConfiguredAuthorModeFromArgs(t *testing.T) {
	// The real jsonpath output shape: a bracketed, quoted, comma-separated list.
	const live = `["--metrics-bind-address=:8443","--admission-webhook",` +
		`"--redis-addr=valkey.valkey-e2e.svc.cluster.local:6379","--redis-insecure"]`

	for _, tc := range []struct {
		name string
		args string
		want bool
	}{
		{"live e2e deployment attributes authors", live, false},
		{"no flags at all means attribution (both defaults enabled)", `[]`, false},
		{"explicit attribution opt-out", `["--redis-addr=valkey:6379","--author-attribution=false"]`, true},
		{"redis explicitly cleared", `["--redis-addr="]`, true},
		{"attribution off and redis cleared", `["--author-attribution=false","--redis-addr="]`, true},
		{"bare --author-attribution is the true form, not an opt-out", `["--author-attribution"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configuredAuthorModeFromArgs(tc.args); got != tc.want {
				t.Fatalf("configuredAuthorModeFromArgs(%s) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
