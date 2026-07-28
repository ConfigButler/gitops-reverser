// SPDX-License-Identifier: Apache-2.0

package e2e

import "testing"

// TestConfiguredAuthorModeFromArgs_AllScenarios verifies that the author-mode decision is read from
// the Deployment's args. Both defaults are
// ENABLED in cmd/main.go — so an absent flag means attribution mode, and only an explicit
// opt-out selects configured-author mode. Getting this backwards silently flips the commit
// author assertion instead of failing it.
//
// Note there is no `["--redis-addr="]` row: clearing Redis while attribution stays at its
// default of true is FATAL at startup ("redis-addr is required when author-attribution is
// enabled", cmd/main.go), so it is not a mode this probe can ever be asked to classify. Every
// configured-author row below therefore turns attribution off explicitly.
func TestConfiguredAuthorModeFromArgs_AllScenarios(t *testing.T) {
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
		{"attribution off and redis cleared", `["--author-attribution=false","--redis-addr="]`, true},
		{"bare --author-attribution is the true form, not an opt-out", `["--author-attribution"]`, false},

		// Go's flag package parses booleans with strconv.ParseBool, so every one of these is a
		// real opt-out a chart value could produce (e.g. `--set attribution.enabled=0`). Matching
		// only the literal "=false" would classify them as attribution mode while the controller
		// runs configured-author, flipping the commit-author assertion instead of failing it.
		{"numeric false", `["--author-attribution=0","--redis-addr="]`, true},
		{"short false", `["--author-attribution=f","--redis-addr="]`, true},
		{"capitalised false", `["--author-attribution=False","--redis-addr="]`, true},
		{"upper false", `["--author-attribution=FALSE","--redis-addr="]`, true},
		{"numeric true is not an opt-out", `["--author-attribution=1"]`, false},
		{"capitalised true is not an opt-out", `["--author-attribution=True"]`, false},

		// Attribution needs a fact TRANSPORT, which stopped meaning Redis. The in-memory transport
		// runs it with no Redis at all, so reading an empty --redis-addr as configured-author would
		// skip every attribution spec and report a green run that asserted nothing about the mode
		// it was supposed to be exercising.
		{"memory transport needs no redis", `["--redis-addr=","--author-attribution-transport=memory"]`, false},
		{"redis transport with no redis is still configured-author",
			`["--redis-addr=","--author-attribution-transport=redis"]`, true},
		{"attribution off beats any transport",
			`["--author-attribution=false","--author-attribution-transport=memory"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configuredAuthorModeFromArgs(tc.args); got != tc.want {
				t.Fatalf("configuredAuthorModeFromArgs(%s) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
