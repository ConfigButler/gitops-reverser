// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/attribution"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// fakeLookup stands in for the fact index. The resolver makes exactly ONE call now — the waiting
// belongs to the index, which is the only thing that knows when a fact arrives — so lateness is
// modelled as a delay before the resolution is returned rather than as a number of retries.
type fakeLookup struct {
	resolution queue.AuthorResolution
	// availableAfter is how long the fact takes to arrive. Longer than the grace it is handed means
	// it never arrives in time, which is what the index reports as absent.
	availableAfter time.Duration
	calls          int
	lastQuery      queue.FactQuery
	lastGrace      time.Duration
}

func (f *fakeLookup) Await(
	ctx context.Context,
	query queue.FactQuery,
	grace time.Duration,
) queue.AuthorResolution {
	f.calls++
	f.lastQuery = query
	f.lastGrace = grace
	if f.availableAfter > grace {
		return queue.AuthorResolution{Result: queue.AttributionAbsent}
	}
	if f.availableAfter > 0 {
		select {
		case <-ctx.Done():
			return queue.AuthorResolution{Result: queue.AttributionAbsent}
		case <-time.After(f.availableAfter):
		}
	}
	return f.resolution
}

// resolverQuery is the ordinary object-write query these tests drive the resolver with.
func resolverQuery(route, uid, rv string, exactCapable bool) AuthorQuery {
	return AuthorQuery{
		AuditRoute:      route,
		GVR:             resolverGVR,
		UID:             k8stypes.UID(uid),
		ResourceVersion: rv,
		Namespace:       "team-a",
		ExactCapable:    exactCapable,
	}
}

var resolverGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func TestAuthorResolver_HumanHit(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice", Email: "a@x.io"},
			Result: queue.AttributionExact,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard(), nil)

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "alice", ui.Username)
	assert.Equal(t, "a@x.io", ui.Email)
	assert.Equal(t, 1, lookup.calls)
	assert.True(t, lookup.lastQuery.ExactCapable, "an ADDED/MODIFIED event is exact-capable")
}

func TestAuthorResolver_ServiceAccountIsNamed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	// A matched service account is always named by its own username — never collapsed to the
	// committer — and the tier and the actor kind are recorded as two separate labels. They used to
	// be one value, exact_serviceaccount, which made "how many exact resolutions" a sum of two
	// series and made the actor kind unaskable of any other tier.
	sa := "system:serviceaccount:flux-system:kustomize-controller"
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: sa},
			Result: queue.AttributionExact,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard(), nil)

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, git.AttributionResolved, outcome,
		"a matched service account is named, not collapsed to the committer")
	assert.Equal(t, sa, ui.Username)

	count, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{
			"tier":       string(queue.AttributionExact),
			"actor_kind": string(queue.ActorKindServiceAccount),
			"resource":   "deployments",
		})
	require.True(t, ok)
	assert.Equal(t, int64(1), count)

	// The wait histogram carries the tier and the KIND OF EVENT instead of the actor: an ADDED or
	// MODIFIED event is a write, and a write does not hold a fallback and keep waiting.
	waitCount, ok := telemetry.CollectHistogramCount(reader, "gitopsreverser_attribution_resolution_wait_seconds",
		map[string]string{
			"tier":       string(queue.AttributionExact),
			"event_kind": attributionEventKindWrite,
		})
	require.True(t, ok)
	assert.Equal(t, uint64(1), waitCount)
}

// TestAuthorResolver_UserAndServiceAccountShareOneTier proves the split does what it was for: one
// tier series, two actor kinds under it.
func TestAuthorResolver_UserAndServiceAccountShareOneTier(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	for _, author := range []string{"alice", "system:serviceaccount:flux-system:kustomize-controller"} {
		lookup := &fakeLookup{
			resolution: queue.AuthorResolution{
				Fact:   queue.AuthorFact{Author: author},
				Result: queue.AttributionExact,
			},
		}
		r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard(), nil)
		_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
		require.Equal(t, git.AttributionResolved, outcome)
	}

	tierTotal, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{"tier": string(queue.AttributionExact)})
	require.True(t, ok)
	assert.Equal(t, int64(2), tierTotal, "counting one tier is one selector, not a sum of two series")

	for kind, want := range map[queue.ActorKind]int64{
		queue.ActorKindUser:           1,
		queue.ActorKindServiceAccount: 1,
		// Neither resolution was a miss, so nothing was named "none" — the third value is asserted
		// absent rather than left unstated.
		queue.ActorKindNone: 0,
	} {
		got, found := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
			map[string]string{"tier": string(queue.AttributionExact), "actor_kind": string(kind)})
		require.Equal(t, want > 0, found, "actor kind %q", kind)
		assert.Equal(t, want, got, "actor kind %q", kind)
	}
}

// TestAuthorResolver_RemovalWaitIsItsOwnSeries covers the distinction --author-attribution-grace is
// tuned from: an absent write and an absent removal used to land in one histogram series, and only
// the removal sits out the whole grace window.
func TestAuthorResolver_RemovalWaitIsItsOwnSeries(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}}
	r := NewAuthorResolver(lookup, 0, logr.Discard(), nil)

	_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, git.AttributionUnresolved, outcome)

	removals, ok := telemetry.CollectHistogramCount(reader, "gitopsreverser_attribution_resolution_wait_seconds",
		map[string]string{
			"tier":       string(queue.AttributionAbsent),
			"event_kind": attributionEventKindRemoval,
		})
	require.True(t, ok)
	assert.Equal(t, uint64(1), removals)

	_, writes := telemetry.CollectHistogramCount(reader, "gitopsreverser_attribution_resolution_wait_seconds",
		map[string]string{
			"tier":       string(queue.AttributionAbsent),
			"event_kind": attributionEventKindWrite,
		})
	assert.False(t, writes, "a removal's wait must not be counted as a write's")

	// An unmatched resolution names nobody, and says so rather than leaving the label off.
	absent, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{
			"tier":       string(queue.AttributionAbsent),
			"actor_kind": string(queue.ActorKindNone),
		})
	require.True(t, ok)
	assert.Equal(t, int64(1), absent)
}

func TestAuthorResolver_MissExpiresToUnresolved(t *testing.T) {
	lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}}
	r := NewAuthorResolver(lookup, 0, logr.Discard(), nil)

	// A zero grace does a single lookup and, on a miss, reports UNRESOLVED — attribution ran
	// and did not name anyone. It is deliberately not NotAttempted, which would claim
	// attribution was never switched on. There is no miss-marker write-back.
	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	assert.Equal(t, git.AttributionUnresolved, outcome)
	assert.Empty(t, ui.Username, "an unresolved attribution names nobody")
	assert.Equal(t, 1, lookup.calls)
}

func TestAuthorResolver_DeleteEventIsNotExactCapable(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice"},
			Result: queue.AttributionLatest,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard(), nil)

	_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, git.AttributionResolved, outcome)
	assert.False(t, lookup.lastQuery.ExactCapable, "a removal event may consult the weaker tiers")
}

func TestAuthorResolver_WaitsThroughGraceWindowForLateFact(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "bob"},
			Result: queue.AttributionExact,
		},
		availableAfter: 50 * time.Millisecond,
	}
	r := NewAuthorResolver(lookup, 2*time.Second, logr.Discard(), nil)

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "bob", ui.Username)
	assert.Equal(t, 1, lookup.calls, "the resolver asks once and the index does the waiting")
}

// A nil lookup is configured-author mode: attribution was never switched on, so the outcome
// must be NotAttempted — not Unresolved. Conflating the two is what made a lost actor
// indistinguishable from a deployment that simply does not do attribution.
func TestAuthorResolver_NilLookupIsNotAttempted(t *testing.T) {
	r := NewAuthorResolver(nil, DefaultAttributionGraceWindow, logr.Discard(), nil)

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))

	assert.Equal(t, git.AttributionNotAttempted, outcome,
		"attribution that was never enabled has not failed — the committer legitimately authors")
	assert.Empty(t, ui.Username)
}

// A fact that exists but carries no author is also unresolved, not not-attempted: attribution
// ran, found something, and still could not name anyone.
//
// The publish gate makes this unreachable in production — AuthorFactFromEvent refuses an event
// whose user cannot be resolved, and counts it as no_attribution_fact — so this pins the DEFENSIVE
// branch, and with it the invariant every coverage query rests on: the tier says which evidence
// answered, and if that evidence names nobody the metrics say so on the actor_kind label rather
// than quietly counting it as a named actor.
func TestAuthorResolver_AuthorlessFactIsUnresolved(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: ""},
			Result: queue.AttributionExact,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard(), nil)

	_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))

	assert.Equal(t, git.AttributionUnresolved, outcome)

	// The tier is the one that matched, and the actor kind is none — so a reader can tell this apart
	// from a resolution that named somebody, which reading coverage off the tier alone cannot.
	named, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{
			"tier":       string(queue.AttributionExact),
			"actor_kind": string(queue.ActorKindNone),
		})
	require.True(t, ok, "an authorless match must not be recorded as a named actor")
	assert.Equal(t, int64(1), named)
}

// TestAuthorResolver_WarnsOnceForARouteThatNeverResolves drives the whole resolver, not just the
// health counter, so the warning path is exercised the way production reaches it: repeated events
// on one audit route that never match a fact.
//
// This is the loud half of the audit-route fix. A ClusterProvider pointed at a route no API server
// posts under mirrors correctly and loses only the commit author, which is why the original bug went
// unnoticed until an explicit unresolved-author placeholder made it visible in Git.
func TestAuthorResolver_WarnsOnceForARouteThatNeverResolves(t *testing.T) {
	// hitAfter is beyond any call this test makes, so every lookup misses.
	lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}}
	resolver := NewAuthorResolver(lookup, 0, logr.Discard(), &attribution.RouteHealth{})
	concrete, ok := resolver.(*attributionResolver)
	require.True(t, ok)

	const route = "srcns-delegating"
	for range attribution.UnresolvedWarnThreshold {
		_, outcome := resolver.ResolveAuthor(context.Background(), resolverQuery(route, "uid-1", "101", true))
		require.Equal(t, git.AttributionUnresolved, outcome)
	}

	// The threshold has been reached, so the route is marked warned and never warns again.
	warn, _ := concrete.health.ObserveResolution(route, false)
	assert.False(t, warn, "a configuration mistake is worth saying once, not once per event")

	// A route that resolves is never implicated, even after the other one has warned.
	other := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice"},
			Result: queue.AttributionExact,
		},
	}
	healthy := NewAuthorResolver(other, 0, logr.Discard(), &attribution.RouteHealth{})
	_, outcome := healthy.ResolveAuthor(context.Background(), resolverQuery("default", "uid-2", "1", true))
	assert.Equal(t, git.AttributionResolved, outcome)
}

// TestAuthorResolver_AuthorlessMatchProvesTheRouteIsAlive covers a matched fact that names nobody.
// It used to be observed as a MISS, which was wrong in both directions: a few of them followed by
// real misses could push the route over the warn threshold and log "no audit facts have ever
// arrived on this audit route" about a route that had just delivered several, and a run of them on
// its own reached the threshold inside ObserveResolution — which latches warned[route] — while the
// caller discarded the returned bool.
//
// A matched fact means the route is alive, which is the only thing that warning is about. The
// missing author is a different problem with a different fix.
func TestAuthorResolver_AuthorlessMatchProvesTheRouteIsAlive(t *testing.T) {
	const route = "srcns-delegating"
	health := &attribution.RouteHealth{}

	authorless := &fakeLookup{resolution: queue.AuthorResolution{
		Fact:   queue.AuthorFact{Author: ""},
		Result: queue.AttributionExact,
	}}
	resolver := NewAuthorResolver(authorless, 0, logr.Discard(), health)

	for range attribution.UnresolvedWarnThreshold {
		_, outcome := resolver.ResolveAuthor(context.Background(), resolverQuery(route, "uid-1", "101", true))
		require.Equal(t, git.AttributionUnresolved, outcome, "an authorless fact still names nobody")
	}

	// However many real misses follow, the route never claims "no facts have ever arrived": facts
	// HAVE arrived on it, so that sentence would be false.
	for range attribution.UnresolvedWarnThreshold * 2 {
		warn, _ := health.ObserveResolution(route, false)
		assert.False(t, warn, "a route that has delivered a fact must never be accused of delivering none")
	}
}
