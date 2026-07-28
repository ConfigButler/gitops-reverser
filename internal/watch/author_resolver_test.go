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
			Result: queue.AttributionExactUser,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

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

	// A matched service account is always named by its own username — never collapsed
	// to the committer — and the resolution is recorded as exact_serviceaccount.
	sa := "system:serviceaccount:flux-system:kustomize-controller"
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: sa},
			Result: queue.AttributionExactServiceAccount,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, git.AttributionResolved, outcome,
		"a matched service account is named, not collapsed to the committer")
	assert.Equal(t, sa, ui.Username)

	count, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{"result": string(queue.AttributionExactServiceAccount)})
	require.True(t, ok)
	assert.Equal(t, int64(1), count)

	waitCount, ok := telemetry.CollectHistogramCount(reader, "gitopsreverser_attribution_resolution_wait_seconds",
		map[string]string{"result": string(queue.AttributionExactServiceAccount)})
	require.True(t, ok)
	assert.Equal(t, uint64(1), waitCount)
}

func TestAuthorResolver_MissExpiresToUnresolved(t *testing.T) {
	lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}}
	r := NewAuthorResolver(lookup, 0, logr.Discard())

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
			Result: queue.AttributionWeak,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, git.AttributionResolved, outcome)
	assert.False(t, lookup.lastQuery.ExactCapable, "a removal event may consult the weaker tiers")
}

func TestAuthorResolver_WaitsThroughGraceWindowForLateFact(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "bob"},
			Result: queue.AttributionExactUser,
		},
		availableAfter: 50 * time.Millisecond,
	}
	r := NewAuthorResolver(lookup, 2*time.Second, logr.Discard())

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, git.AttributionResolved, outcome)
	assert.Equal(t, "bob", ui.Username)
	assert.Equal(t, 1, lookup.calls, "the resolver asks once and the index does the waiting")
}

// A nil lookup is configured-author mode: attribution was never switched on, so the outcome
// must be NotAttempted — not Unresolved. Conflating the two is what made a lost actor
// indistinguishable from a deployment that simply does not do attribution.
func TestAuthorResolver_NilLookupIsNotAttempted(t *testing.T) {
	r := NewAuthorResolver(nil, DefaultAttributionGraceWindow, logr.Discard())

	ui, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))

	assert.Equal(t, git.AttributionNotAttempted, outcome,
		"attribution that was never enabled has not failed — the committer legitimately authors")
	assert.Empty(t, ui.Username)
}

// A fact that exists but carries no author is also unresolved, not not-attempted: attribution
// ran, found something, and still could not name anyone.
func TestAuthorResolver_AuthorlessFactIsUnresolved(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: ""},
			Result: queue.AttributionExactUser,
		},
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	_, outcome := r.ResolveAuthor(context.Background(), resolverQuery("prod-eu-1", "uid-1", "101", true))

	assert.Equal(t, git.AttributionUnresolved, outcome)
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
	resolver := NewAuthorResolver(lookup, 0, logr.Discard())
	concrete, ok := resolver.(*attributionResolver)
	require.True(t, ok)

	const route = "srcns-delegating"
	for range attributionUnresolvedWarnThreshold {
		_, outcome := resolver.ResolveAuthor(context.Background(), resolverQuery(route, "uid-1", "101", true))
		require.Equal(t, git.AttributionUnresolved, outcome)
	}

	// The threshold has been reached, so the route is marked warned and never warns again.
	warn, _ := concrete.health.observe(route, false)
	assert.False(t, warn, "a configuration mistake is worth saying once, not once per event")

	// A route that resolves is never implicated, even after the other one has warned.
	other := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice"},
			Result: queue.AttributionExactUser,
		},
	}
	healthy := NewAuthorResolver(other, 0, logr.Discard())
	_, outcome := healthy.ResolveAuthor(context.Background(), resolverQuery("default", "uid-2", "1", true))
	assert.Equal(t, git.AttributionResolved, outcome)
}
