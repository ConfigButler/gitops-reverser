// SPDX-License-Identifier: Apache-2.0

package attribution

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestRouteHealth_FirstFactPublished pins the latch's producer half: the FIRST fact on a route is
// the signal, every later one is nothing new, and the answer is per route.
func TestRouteHealth_FirstFactPublished(t *testing.T) {
	at := time.Date(2026, 9, 4, 10, 30, 0, 0, time.UTC)
	h := &RouteHealth{now: func() time.Time { return at }}

	_, seen := h.FirstFactAt("default")
	assert.False(t, seen, "a route nobody has posted under has no first fact")

	assert.True(t, h.RecordFactPublished("default"), "the first fact on a route is the signal")
	assert.False(t, h.RecordFactPublished("default"), "a later fact is not a new signal")

	got, seen := h.FirstFactAt("default")
	require.True(t, seen)
	assert.Equal(t, at, got, "the recorded time is the first fact's, not the latest one's")

	_, seen = h.FirstFactAt("srcns-delegating")
	assert.False(t, seen, "one route's traffic says nothing about another's")

	assert.False(t, h.RecordFactPublished(""), "an empty route is not a route")
}

// TestRouteHealth_FirstFactEvents pins the prompt-flip notification: exactly one event per route,
// naming the route, and a full buffer degrades to silence rather than blocking the audit path.
func TestRouteHealth_FirstFactEvents(t *testing.T) {
	h := &RouteHealth{}
	events := h.FirstFactEvents()

	h.RecordFactPublished("default")
	h.RecordFactPublished("default")

	select {
	case evt := <-events:
		assert.Equal(t, "default", evt.Object.GetName(), "the event carries the ROUTE, not an object")
	default:
		t.Fatal("the first fact on a route must notify the controller")
	}
	select {
	case <-events:
		t.Fatal("only the first fact notifies; the route is already latched")
	default:
	}
}

// TestRouteHealth_FirstFactEventsDropWhenFull pins that the audit ingress is never blocked by a
// controller that is not draining: the notification is best-effort and the periodic reconcile is
// the backstop.
func TestRouteHealth_FirstFactEventsDropWhenFull(t *testing.T) {
	h := &RouteHealth{firstFactEvents: make(chan event.GenericEvent, 1)}
	h.RecordFactPublished("a")
	h.RecordFactPublished("b")

	assert.Len(t, h.firstFactEvents, 1)
	_, seen := h.FirstFactAt("b")
	assert.True(t, seen, "a dropped notification must not lose the recorded fact")
}

// TestRouteHealth_NilIsUsable covers the configured-author wiring, where no registry exists at all.
func TestRouteHealth_NilIsUsable(t *testing.T) {
	var h *RouteHealth
	assert.False(t, h.RecordFactPublished("default"))
	_, seen := h.FirstFactAt("default")
	assert.False(t, seen)
	assert.Nil(t, h.FirstFactEvents())
	warn, streak := h.ObserveResolution("default", false)
	assert.False(t, warn)
	assert.Zero(t, streak)
}

// TestRouteHealth_WarnsOnceForARouteThatNeverResolves pins the loud half of the read side.
// A route no API server posts under produces a run of unresolved events and never a resolution,
// which is the one signature worth interrupting an operator for. Everything else stays quiet: a
// single late fact is ordinary, and a route that has ever resolved is merely missing one.
func TestRouteHealth_WarnsOnceForARouteThatNeverResolves(t *testing.T) {
	t.Run("a run of misses on a never-resolved route warns exactly once", func(t *testing.T) {
		var health RouteHealth
		for i := 1; i < UnresolvedWarnThreshold; i++ {
			warn, _ := health.ObserveResolution("srcns-delegating", false)
			assert.False(t, warn, "a short run of misses is ordinary under audit-batch delay")
		}
		warn, streak := health.ObserveResolution("srcns-delegating", false)
		assert.True(t, warn)
		assert.Equal(t, UnresolvedWarnThreshold, streak)

		warn, _ = health.ObserveResolution("srcns-delegating", false)
		assert.False(t, warn, "a configuration mistake is worth saying once, not once per event")
	})

	t.Run("a route that has resolved never warns", func(t *testing.T) {
		var health RouteHealth
		_, _ = health.ObserveResolution("default", true)
		for range UnresolvedWarnThreshold * 3 {
			warn, _ := health.ObserveResolution("default", false)
			assert.False(t, warn, "a working route missing some facts is a freshness problem, not a misconfiguration")
		}
	})

	t.Run("routes are tracked independently", func(t *testing.T) {
		var health RouteHealth
		for range UnresolvedWarnThreshold {
			_, _ = health.ObserveResolution("broken", false)
		}
		warn, _ := health.ObserveResolution("other", false)
		assert.False(t, warn, "one broken route must not implicate another")
	})

	t.Run("a resolution clears the streak", func(t *testing.T) {
		var health RouteHealth
		for i := 1; i < UnresolvedWarnThreshold; i++ {
			_, _ = health.ObserveResolution("flaky", false)
		}
		_, _ = health.ObserveResolution("flaky", true)
		warn, streak := health.ObserveResolution("flaky", false)
		assert.False(t, warn)
		assert.Equal(t, 1, streak, "the run restarts, so an intermittent miss never accumulates into a warning")
	})
}

// TestRouteHealth_ProcessWideSignals covers the two flags that separate the three silences. Both
// are deliberately process-wide: an audit request reaching this operator, and the transport
// accepting a write, are properties of the pipeline rather than of any one route.
func TestRouteHealth_ProcessWideSignals(t *testing.T) {
	h := &RouteHealth{}
	assert.False(t, h.AuditDelivered(), "nothing has posted to this operator yet")
	assert.False(t, h.TransportFailing(), "no append has been attempted, so nothing has failed")

	h.RecordAuditDelivery()
	assert.True(t, h.AuditDelivered())
	assert.False(t, h.TransportFailing(), "delivery says nothing about the transport")

	h.RecordPublishFailure()
	assert.True(t, h.TransportFailing())

	// A successful append is the only evidence of recovery, and it counts whichever route it lands
	// on: the transport is one thing, shared by every route.
	h.RecordFactPublished("some-other-route")
	assert.False(t, h.TransportFailing(), "a landed append proves the transport accepts writes again")
	_, seen := h.FirstFactAt("this-route")
	assert.False(t, seen, "recovery on another route is not evidence about this one")
}

// TestRouteHealth_NilIsUsableForProcessWideSignals extends the configured-author wiring to the two
// new flags.
func TestRouteHealth_NilIsUsableForProcessWideSignals(t *testing.T) {
	var h *RouteHealth
	h.RecordAuditDelivery()
	h.RecordPublishFailure()
	assert.False(t, h.AuditDelivered())
	assert.False(t, h.TransportFailing())
}
