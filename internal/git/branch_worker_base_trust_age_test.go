// SPDX-License-Identifier: Apache-2.0

package git

// The maximum-age backstop on base trust, from
// docs/design/push-notification-and-reconcile-trigger.md, part 3 option 1.
//
// The mechanism is deliberately small: one timestamp written where trust is gained, and one
// method that drops trust when it is older than a configured age. What it is FOR is the idle
// target, which is converged and therefore requeues without ever touching Git. These tests pin
// the two properties that make it safe to enable: it is off by default, and a target that keeps
// publishing keeps resetting the clock, so it is never expired out from under a busy branch.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpireBaseTrust_DisabledByDefault is the property that keeps the round-trip ledger honest:
// with no configured age, nothing here reintroduces the fetch that §1.4 removed.
func TestExpireBaseTrust_DisabledByDefault(t *testing.T) {
	w := newMetricsTestWorker()
	w.setBaseTrusted(true)
	w.baseTrustedAt.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	assert.False(t, w.ExpireBaseTrust(0), "a zero age must never expire trust")
	assert.False(t, w.ExpireBaseTrust(-time.Minute), "a negative age must never expire trust")
	assert.True(t, w.baseTrusted())
}

// TestExpireBaseTrust_ExpiresOnlyWhatIsOlderThanTheAge walks the boundary in both directions.
func TestExpireBaseTrust_ExpiresOnlyWhatIsOlderThanTheAge(t *testing.T) {
	cases := []struct {
		name       string
		trustedAgo time.Duration
		maxAge     time.Duration
		wantExpire bool
	}{
		{name: "younger than the age", trustedAgo: time.Minute, maxAge: time.Hour, wantExpire: false},
		{name: "older than the age", trustedAgo: 2 * time.Hour, maxAge: time.Hour, wantExpire: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMetricsTestWorker()
			w.setBaseTrusted(true)
			w.baseTrustedAt.Store(time.Now().Add(-tc.trustedAgo).UnixNano())

			assert.Equal(t, tc.wantExpire, w.ExpireBaseTrust(tc.maxAge))
			assert.Equal(t, !tc.wantExpire, w.baseTrusted(),
				"expiring must be the same thing as invalidating the base")
		})
	}
}

// TestExpireBaseTrust_IgnoresAnUntrustedBase. There is nothing to expire, and reporting true
// would make the log line claim a transition that did not happen.
func TestExpireBaseTrust_IgnoresAnUntrustedBase(t *testing.T) {
	w := newMetricsTestWorker()
	require.False(t, w.baseTrusted())

	assert.False(t, w.ExpireBaseTrust(time.Nanosecond))
}

// TestExpireBaseTrust_EveryGainResetsTheClock is the property that keeps a BUSY target out of
// this mechanism entirely. Trust is re-gained on every successful push, and the age asks "how
// long since we last read the remote", so a target that is publishing must never be expired.
//
// The stamp is therefore written on every gain and not only on the false-to-true transition,
// which is the one way this could have been built wrong without any test noticing.
func TestExpireBaseTrust_EveryGainResetsTheClock(t *testing.T) {
	w := newMetricsTestWorker()
	w.setBaseTrusted(true)
	w.baseTrustedAt.Store(time.Now().Add(-time.Hour).UnixNano())

	// A push succeeds: trust is gained again while already true.
	w.setBaseTrusted(true)

	assert.False(t, w.ExpireBaseTrust(time.Minute),
		"a target that keeps publishing must not be expired by the age")
	assert.True(t, w.baseTrusted())
}

// TestExpireBaseTrust_NeverTrustedHasNoTimestamp guards the zero value: a worker that has never
// gained trust must not be treated as having been trusted since the epoch.
func TestExpireBaseTrust_NeverTrustedHasNoTimestamp(t *testing.T) {
	w := newMetricsTestWorker()
	// Force the flag on without going through the setter, so the stamp stays zero.
	w.baseTrustedState.Store(true)
	require.Zero(t, w.baseTrustedAt.Load())

	assert.False(t, w.ExpireBaseTrust(time.Nanosecond),
		"an unstamped trust must not expire; it is a bug elsewhere, not an old base")
	assert.True(t, w.baseTrusted())
}
