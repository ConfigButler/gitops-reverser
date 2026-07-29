// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// The index is exercised through the in-memory transport rather than by calling Apply, because
// "publish a fact and have a watch event resolve it" is the whole behaviour. Driving it end to end
// also keeps the two halves honest about delivery ORDER, which the latest tier depends on.

const (
	// factIndexTestBlock keeps the follower's idle round short so a test that waits for a fact does
	// not wait out a whole second.
	factIndexTestBlock = 10 * time.Millisecond
	// factIndexTestGrace bounds a wait that is meant to succeed; a passing test never spends it.
	factIndexTestGrace = 5 * time.Second
	// factIndexTestSettle is how long a negative assertion gives a fact it does not want to see.
	factIndexTestSettle = 200 * time.Millisecond
)

// factIndexHarness is one index following one in-memory transport, with the follower running for
// the test's lifetime.
type factIndexHarness struct {
	t         *testing.T
	index     *FactIndex
	transport *MemoryFactStream
}

func newFactIndexHarness(t *testing.T, cfg FactIndexConfig) *factIndexHarness {
	t.Helper()
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = time.Hour
	}
	// The transport keeps everything for the test's lifetime: what a case with a short TTL is
	// exercising is the INDEX's horizon, and letting retention race delivery would only make it flaky.
	transport := NewMemoryFactStream(MemoryFactStreamConfig{TTL: time.Hour, Block: factIndexTestBlock})
	index := NewFactIndex(cfg)

	// The follower runs for the test's lifetime; its outcome is judged on the test's own goroutine.
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = index.Run(t.Context(), transport)
	}()
	t.Cleanup(func() {
		<-done
		require.NoError(t, runErr)
	})

	return &factIndexHarness{t: t, index: index, transport: transport}
}

// publish follows the stream if it is not followed already, then appends one batch to it.
func (h *factIndexHarness) publish(key FactStreamKey, facts ...AuthorFact) {
	h.t.Helper()
	h.index.Streams().Acquire(key)
	require.NoError(h.t, h.transport.PublishFacts(h.t.Context(), key, facts))
}

// waitForFacts blocks until the index holds at least n entries, so a NEGATIVE assertion can be made
// about a fact that has definitely been applied rather than about one still in flight.
func (h *factIndexHarness) waitForFacts(n int) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.index.Len() >= n },
		factIndexTestGrace, factIndexTestBlock, "the follower never applied %d facts", n)
}

// resolve waits out the grace for a query it expects to succeed.
func (h *factIndexHarness) resolve(query FactQuery) AuthorResolution {
	h.t.Helper()
	return h.index.Await(h.t.Context(), query, factIndexTestGrace)
}

// absent asserts a query does not resolve, giving a late fact time to arrive and prove it wrong.
func (h *factIndexHarness) absent(query FactQuery) {
	h.t.Helper()
	resolution := h.index.Await(h.t.Context(), query, factIndexTestSettle)
	require.Equal(h.t, AttributionAbsent, resolution.Result, "resolved to %q", resolution.Fact.Author)
}

func deploymentsGroupResource() schema.GroupResource {
	return schema.GroupResource{Group: "apps", Resource: "deployments"}
}

func factIndexTestStream(route string) FactStreamKey {
	return FactStreamKeyFor(route, deploymentsGroupResource())
}

// factIndexTestUID is the object every ordinary write in these tests is about.
const factIndexTestUID = "uid-1"

// objectFact is one ordinary write's fact.
func objectFact(author, rv string) AuthorFact {
	return AuthorFact{
		Namespace:       "team-a",
		UID:             factIndexTestUID,
		ResourceVersion: rv,
		Author:          author,
		Verb:            "update",
	}
}

// aliceCollectionFact is one deletecollection's fact: the actor, the scope, the selector, and the
// uids the API server said it covered.
func aliceCollectionFact(selector string, uids ...string) AuthorFact {
	return AuthorFact{
		Namespace:      "team-a",
		Author:         "alice",
		Verb:           "deletecollection",
		LabelSelector:  selector,
		UIDs:           uids,
		StageTimestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// objectQuery is one watch event's identity.
func objectQuery(route, uid, rv string, exactCapable bool) FactQuery {
	return FactQuery{
		AuditRoute:      route,
		GroupResource:   deploymentsGroupResource(),
		UID:             uid,
		ResourceVersion: rv,
		Namespace:       "team-a",
		ExactCapable:    exactCapable,
	}
}

func TestFactIndex_JoinPolicyDependsOnTheEventKind(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	harness.publish(factIndexTestStream("prod-eu-1"), objectFact("alice", "101"))

	// ADDED / MODIFIED present the resourceVersion the write produced, so they join exactly.
	exact := harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true))
	require.Equal(t, AttributionExact, exact.Result)
	require.Equal(t, "alice", exact.Fact.Author)

	harness.waitForFacts(2)

	// An exact-capable event whose rv does not match must NOT fall through to the latest tier: that
	// pointer may name a different, older author than the create or update this event represents.
	harness.absent(objectQuery("prod-eu-1", "uid-1", "999", true))

	// A removal's rv never matches the write's, so it is the event kind that consults latest.
	removal := harness.resolve(objectQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, AttributionLatest, removal.Result)
	require.Equal(t, "alice", removal.Fact.Author)
}

func TestFactIndex_RouteIsolatesOtherwiseIdenticalFacts(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	// The same type, uid, and resourceVersion on two clusters. A resourceVersion is opaque and not
	// unique across clusters, so without the route leading every key one cluster's actor would name
	// the author of the other cluster's object.
	harness.publish(factIndexTestStream("prod-eu-1"), objectFact("alice", "101"))
	harness.publish(factIndexTestStream("prod-us-1"), objectFact("bob", "101"))
	// And the rv-only hatch, which is where it bites hardest because it carries no uid at all.
	rvOnly := AuthorFact{Namespace: "team-a", ResourceVersion: "202", Verb: "update"}
	euOnly, usOnly := rvOnly, rvOnly
	euOnly.Author, usOnly.Author = "eu-rv", "us-rv"
	harness.publish(factIndexTestStream("prod-eu-1"), euOnly)
	harness.publish(factIndexTestStream("prod-us-1"), usOnly)

	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)
	require.Equal(t, "bob", harness.resolve(objectQuery("prod-us-1", "uid-1", "101", true)).Fact.Author)
	require.Equal(t, "eu-rv", harness.resolve(objectQuery("prod-eu-1", "", "202", true)).Fact.Author)
	require.Equal(t, "us-rv", harness.resolve(objectQuery("prod-us-1", "", "202", true)).Fact.Author)

	// A route nobody published under resolves nothing, rather than borrowing a neighbour's fact.
	harness.publish(factIndexTestStream("staging"))
	harness.absent(objectQuery("staging", "uid-1", "101", true))
}

func TestFactIndex_LatestTierIsLastWriterWins(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	// Two writes to one object, in one batch, in the order they were appended.
	harness.publish(key, objectFact("alice", "101"), objectFact("bob", "102"))
	harness.waitForFacts(3)

	removal := harness.resolve(objectQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, "bob", removal.Fact.Author, "the latest tier must name the last fact applied")

	// The exact tier is immutable per (uid, rv), so the earlier write is still exactly joinable.
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)
}

func TestFactIndex_FactsAreAbsentPastTheTTL(t *testing.T) {
	const ttl = 500 * time.Millisecond
	harness := newFactIndexHarness(t, FactIndexConfig{TTL: ttl})
	harness.publish(factIndexTestStream("prod-eu-1"), objectFact("alice", "101"))
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)

	time.Sleep(3 * ttl)

	// Expiry is decided on READ, so an aged-out fact is never joined merely because the sweep has
	// not run yet; the sweep then reclaims it.
	require.Equal(t, AttributionAbsent, harness.index.Lookup(objectQuery("prod-eu-1", "uid-1", "101", true)).Result)
	require.Positive(t, harness.index.Sweep(time.Now()))
	require.Zero(t, harness.index.Len())
}

// TestFactIndex_RewritingOneKeyDoesNotStrandTheEvictionOrder bounds the memory a HOT OBJECT costs.
//
// A put that replaces an entry appends an eviction reference and leaves the old one behind — that is
// what makes replacement safe, since the sequence check is how a stale reference is told from a live
// one. So an object rewritten N times inside one sweep interval holds ONE entry and N references, and
// the caps cannot see the difference: they count entries, which is also what the entries gauge
// reports. The sweep drops the dead references, and must also hand back the capacity they occupied,
// or a single retry storm leaves its peak resident for the life of the scope.
func TestFactIndex_RewritingOneKeyDoesNotStrandTheEvictionOrder(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	const rewrites = 4000
	facts := make([]AuthorFact, 0, rewrites)
	for range rewrites {
		// The same object at the same version, over and over: one exact key and one latest key.
		facts = append(facts, objectFact("alice", "101"))
	}
	harness.publish(factIndexTestStream("prod-eu-1"), facts...)

	// Waiting on the ORDER rather than on Len is the only barrier available here: Len reaches 2 on
	// the first fact and stays there, so it cannot say the batch has finished applying.
	scope := factScope{route: "prod-eu-1", groupResource: groupResourceKey("apps", "deployments")}
	require.Eventually(t, func() bool { return harness.orderCapacity(scope) > rewrites },
		factIndexTestGrace, factIndexTestBlock,
		"the references are what the sweep has to reclaim, so the test is only meaningful if they piled up")
	require.Equal(t, 2, harness.index.Len(), "one exact and one latest entry, however many rewrites landed")

	harness.index.Sweep(time.Now())

	require.Equal(t, 2, harness.index.Len(), "the sweep drops references, never live entries")
	require.Less(t, harness.orderCapacity(scope), rewrites/2,
		"and it hands back the capacity rather than keeping the peak for the life of the scope")
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", factIndexTestUID, "101", true)).Fact.Author,
		"the surviving entry is still joinable through its reference-compacted scope")
}

// orderCapacity reads one scope's eviction-order capacity under the index lock, which is where the
// cost of a replaced entry accumulates.
func (h *factIndexHarness) orderCapacity(scope factScope) int {
	h.t.Helper()
	h.index.mu.Lock()
	defer h.index.mu.Unlock()
	facts, ok := h.index.scopes[scope]
	if !ok {
		return 0
	}
	return cap(facts.order)
}

func TestFactIndex_PerTypeCapEvictsOldestFirstAndCountsIt(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	// rv-only facts occupy one entry each, so the cap counts what the test publishes.
	harness := newFactIndexHarness(t, FactIndexConfig{MaxFactsPerType: 2})
	rvFact := func(author, rv string) AuthorFact {
		return AuthorFact{ResourceVersion: rv, Author: author, Verb: "update"}
	}
	key := factIndexTestStream("prod-eu-1")
	harness.publish(key, rvFact("first", "1"), rvFact("second", "2"), rvFact("third", "3"))
	// A second type must keep its own facts: the cap is per type precisely so a burst on one noisy
	// type cannot evict everything else.
	other := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})
	harness.publish(
		other,
		AuthorFact{ResourceVersion: "9", Author: "quiet", Verb: "update"},
	)

	quiet := FactQuery{AuditRoute: "prod-eu-1", GroupResource: schema.GroupResource{Resource: "configmaps"},
		ResourceVersion: "9", ExactCapable: true}
	require.Equal(t, "quiet", harness.resolve(quiet).Fact.Author)

	require.Equal(t, "third", harness.resolve(objectQuery("prod-eu-1", "", "3", true)).Fact.Author)
	require.Equal(t, "second", harness.resolve(objectQuery("prod-eu-1", "", "2", true)).Fact.Author)
	harness.absent(objectQuery("prod-eu-1", "", "1", true))

	evicted, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_index_evictions_total",
		map[string]string{"reason": evictionReasonPerType})
	require.True(t, ok, "an eviction must be counted, never silently absorbed")
	require.Equal(t, int64(1), evicted)
}

func TestFactIndex_TotalCapEvictsFromTheLargestType(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	harness := newFactIndexHarness(t, FactIndexConfig{MaxFactsTotal: 2})
	busy := factIndexTestStream("prod-eu-1")
	harness.publish(busy,
		AuthorFact{ResourceVersion: "1", Author: "first", Verb: "update"},
		AuthorFact{ResourceVersion: "2", Author: "second", Verb: "update"},
	)
	require.Equal(t, "second", harness.resolve(objectQuery("prod-eu-1", "", "2", true)).Fact.Author)

	quiet := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})
	harness.publish(quiet,
		AuthorFact{ResourceVersion: "9", Author: "quiet", Verb: "update"})

	// The overflow falls on the type holding the most, so the pressure lands where it came from.
	quietQuery := FactQuery{AuditRoute: "prod-eu-1", GroupResource: schema.GroupResource{Resource: "configmaps"},
		ResourceVersion: "9", ExactCapable: true}
	require.Equal(t, "quiet", harness.resolve(quietQuery).Fact.Author)
	harness.absent(objectQuery("prod-eu-1", "", "1", true))
	require.Equal(t, 2, harness.index.Len())

	evicted, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_index_evictions_total",
		map[string]string{"reason": evictionReasonTotal})
	require.True(t, ok)
	require.Equal(t, int64(1), evicted)
}

func TestFactIndex_WaiterIsWokenByALateFact(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	harness.index.Streams().Acquire(key)
	query := objectQuery("prod-eu-1", "uid-1", "101", true)

	resolved := make(chan AuthorResolution, 1)
	go func() { resolved <- harness.index.Await(t.Context(), query, factIndexTestGrace) }()

	// The wait is what the whole design is for: the watch event arrives first, by roughly one audit
	// batch window, and the fact that names its author has not been published yet.
	require.Eventually(t, func() bool { return harness.index.waiters.len() > 0 },
		factIndexTestGrace, factIndexTestBlock, "the resolver never registered a waiter")
	require.NoError(t, harness.transport.PublishFacts(t.Context(), key, []AuthorFact{objectFact("alice", "101")}))

	select {
	case resolution := <-resolved:
		require.Equal(t, "alice", resolution.Fact.Author)
	case <-time.After(factIndexTestGrace):
		t.Fatal("a waiter was never woken by the fact it was waiting for")
	}
	require.Zero(t, harness.index.waiters.len(), "a resolved waiter must leave nothing registered")
}

func TestFactIndex_WaiterRegisteredBeforeTheCheckKeepsAFactAppliedInTheGap(t *testing.T) {
	index := NewFactIndex(FactIndexConfig{})
	query := objectQuery("prod-eu-1", "uid-1", "101", true)

	// Await's step 1, verbatim: register the candidates BEFORE reading the index.
	waiter := index.waiters.register(query.waiterKeys())
	defer index.waiters.unregister(waiter)

	// The gap between registering and checking. Registering after the check would lose exactly this
	// fact, which is the race the poll loop used to paper over by looking again.
	index.Apply(t.Context(), FactEntry{
		Key:   factIndexTestStream("prod-eu-1"),
		Facts: []AuthorFact{objectFact("alice", "101")},
	})

	// The signal is buffered, so it is still there for a waiter that was not listening yet.
	select {
	case <-waiter.ch:
	default:
		t.Fatal("a fact applied in the register-then-check gap left no signal")
	}
	require.Equal(t, "alice", index.Lookup(query).Fact.Author)
}

func TestFactIndex_CollectionMatchesByUIDMembership(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	harness.publish(key, aliceCollectionFact("", "uid-1", "uid-2"))
	harness.waitForFacts(1)

	// One fact, N removals: uid membership carries no over-attribution risk at all, because either
	// the API server said it deleted this object or it did not.
	for _, uid := range []string{"uid-1", "uid-2"} {
		resolution := harness.resolve(objectQuery("prod-eu-1", uid, "999", false))
		require.Equal(t, AttributionDeleteCollectionBodyUID, resolution.Result)
		require.Equal(t, "alice", resolution.Fact.Author)
	}

	// An object outside the namespace the collection named is not covered by it.
	outside := objectQuery("prod-eu-1", "uid-1", "999", false)
	outside.Namespace = "team-b"
	harness.absent(outside)
}

func TestFactIndex_CollectionMatchesByScopeAndSelector(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	// A body-less deletecollection — the shape a production cluster with audit truncation enabled
	// actually sends — so only the scope and the selector are left to join on.
	harness.publish(key, aliceCollectionFact("app=web"))
	harness.waitForFacts(1)

	matching := objectQuery("prod-eu-1", "uid-1", "999", false)
	matching.Labels = map[string]string{"app": "web", "tier": "front"}
	resolution := harness.resolve(matching)
	require.Equal(t, AttributionDeleteCollectionScope, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author)

	// The selector is the intent the actor expressed: an object it does not select was not part of
	// the collection, so naming that actor would name the wrong human.
	unmatched := objectQuery("prod-eu-1", "uid-2", "999", false)
	unmatched.Labels = map[string]string{"app": "api"}
	harness.absent(unmatched)

	// An ADDED or MODIFIED event never reaches the collection tier: a collection delete produces
	// removals only.
	written := objectQuery("prod-eu-1", "uid-1", "999", true)
	written.Labels = matching.Labels
	harness.absent(written)
}

func TestFactIndex_CollectionWithNoSelectorCoversTheWholeNamespace(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	harness.publish(factIndexTestStream("prod-eu-1"), aliceCollectionFact(""))
	harness.waitForFacts(1)

	// No selector is what --all means.
	unlabelled := objectQuery("prod-eu-1", "uid-9", "999", false)
	require.Equal(t, AttributionDeleteCollectionScope, harness.resolve(unlabelled).Result)
}

func TestFactIndex_CollectionScopeMatchStopsAtTheWindow(t *testing.T) {
	const window = 500 * time.Millisecond
	harness := newFactIndexHarness(t, FactIndexConfig{CollectionWindow: window})
	harness.publish(factIndexTestStream("prod-eu-1"), aliceCollectionFact(""))
	harness.waitForFacts(1)
	scoped := harness.resolve(objectQuery("prod-eu-1", "uid-9", "9", false))
	require.Equal(t, AttributionDeleteCollectionScope, scoped.Result)

	// An unrelated delete later in the same namespace must not be claimed by a collection that has
	// long since finished.
	time.Sleep(3 * window)
	harness.absent(objectQuery("prod-eu-1", "uid-9", "9", false))
}

// A collection fact that named no uids is evidence about a SCOPE, so it must not outrank an
// object's own fact. This is the precedence that keeps scope matching safe: an unrelated delete by
// another actor during the same window is claimed by its own fact and never reaches the scope tier.
func TestFactIndex_CollectionScopeIsWeakerThanAnObjectsOwnFact(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	harness.publish(key, aliceCollectionFact(""), objectFact("bob", "101"))
	harness.waitForFacts(3)

	resolution := harness.resolve(objectQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, AttributionLatest, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author)
}

// TestFactIndex_CollectionUIDOutranksAStaleWriteFact is the other side of that precedence, and it
// is the one the tiers originally got wrong.
//
// The latest tier says who last WROTE an object; a removal asks who DELETED it. For a single-object
// delete the two coincide, because the delete files its own fact under that uid. A COLLECTION delete
// files one fact about the collection instead, so the uid's latest entry is left holding whoever
// edited the object last — and ranking it above the collection's uid set credited every removal to
// the previous editor, never reaching the actor who ran the delete. That is the one thing the
// deleted expander got right, by overwriting that entry per object.
//
// The e2e specs could not catch it: they create and delete as the same actor, so the wrong answer
// and the right answer are the same name.
func TestFactIndex_CollectionUIDOutranksAStaleWriteFact(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")

	// Bob edits the object; alice then deletes the collection that covers it, and the API server
	// returns the set, so the fact names this uid.
	harness.publish(key, objectFact("bob", "101"), aliceCollectionFact("", factIndexTestUID))
	harness.waitForFacts(3)

	resolution := harness.resolve(objectQuery("prod-eu-1", factIndexTestUID, "999", false))
	require.Equal(t, AttributionDeleteCollectionBodyUID, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author,
		"the removal was caused by alice's collection delete, not by bob's earlier update")

	// A create/update on the same object is NOT a removal, so it keeps resolving to its own writer:
	// the collection tiers are only ever consulted for an event whose rv cannot match.
	write := harness.resolve(objectQuery("prod-eu-1", factIndexTestUID, "101", true))
	require.Equal(t, "bob", write.Fact.Author)
}

func TestFactIndex_UnjoinableFactIsNotStored(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	harness.publish(key,
		// No uid, no resourceVersion and no name: nothing about it identifies an object.
		AuthorFact{Author: "nobody", Verb: "update"},
		objectFact("alice", "101"),
	)
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)

	// Two entries for the joinable fact, none for the one no query could ever reach.
	require.Equal(t, 2, harness.index.Len())
}

// aggregatedFact is the shape an aggregated-API write produces: the API server proxied the request
// and never decoded the response, so the objectRef carries the name from the URL path and neither a
// uid nor a resourceVersion. Measured in corpus flunder/aggregated-api-delete.
func aggregatedFact(author, name, verb string) AuthorFact {
	return AuthorFact{Namespace: "team-a", Name: name, Author: author, Verb: verb}
}

// namedQuery is a watch event that knows its own name, which every watch event does.
func namedQuery(uid, rv, name string, exactCapable bool) FactQuery {
	query := objectQuery("prod-eu-1", uid, rv, exactCapable)
	query.Name = name
	return query
}

func TestFactIndex_NameTierJoinsAFactCarryingNoUIDOrResourceVersion(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	harness.publish(factIndexTestStream("prod-eu-1"), aggregatedFact("alice", "fl-1", "update"))

	// The watch event carries the full object, so it has a uid and an rv the fact does not. Every
	// stronger tier therefore misses, and the name is the only thing the two sides share.
	resolution := harness.resolve(namedQuery("uid-9", "77", "fl-1", true))
	require.Equal(t, AttributionName, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author)

	// A different object of the same type in the same namespace is not covered by it.
	harness.absent(namedQuery("uid-8", "78", "fl-2", true))
}

func TestFactIndex_NameTierIsScopedToItsNamespace(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	other := aggregatedFact("bob", "fl-1", "update")
	other.Namespace = "team-b"
	harness.publish(factIndexTestStream("prod-eu-1"), other)
	harness.waitForFacts(1)

	// Same type, same route, same name, different namespace: a name is unique only within one.
	harness.absent(namedQuery("uid-9", "77", "fl-1", true))
}

func TestFactIndex_NameTierRanksBelowEveryStrongerTier(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	// A name-only fact and a uid-bearing one about the same object. The uid tiers must win: a name is
	// reused after a delete and recreate, so it is the weakest per-object evidence there is.
	harness.publish(factIndexTestStream("prod-eu-1"),
		aggregatedFact("name-tier", "fl-1", "update"),
		objectFact("uid-tier", "101"),
	)
	harness.waitForFacts(3)

	exact := harness.resolve(namedQuery(factIndexTestUID, "101", "fl-1", true))
	require.Equal(t, AttributionExact, exact.Result)
	require.Equal(t, "uid-tier", exact.Fact.Author)

	// And the latest tier, which a removal consults, also outranks it.
	removal := harness.resolve(namedQuery(factIndexTestUID, "999", "fl-1", false))
	require.Equal(t, "uid-tier", removal.Fact.Author)
}

func TestFactIndex_NameTierResolvesAnAggregatedRemovalToItsDeleter(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	// The whole point of restoring the name: an aggregated single delete is audited with a name and
	// nothing else, so before this tier it was published and then dropped, and the removal shipped
	// committer-authored however ran it.
	harness.publish(factIndexTestStream("prod-eu-1"), aggregatedFact("alice", "fl-del", "delete"))

	removal := harness.resolve(namedQuery("uid-9", "999", "fl-del", false))
	require.Equal(t, AttributionName, removal.Result)
	require.Equal(t, "alice", removal.Fact.Author)
}

func TestFactIndex_ARemovalReachesItsDeleteFactKeyedByName(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	// The shape a built-in delete produces when the API server answers it with a Status rather than
	// the object: there is no uid in the body to recover, so the delete fact is keyed by name alone
	// (corpus configmap/owner-ref-cascade). The object also has an ordinary write fact, keyed by uid.
	harness.publish(factIndexTestStream("prod-eu-1"),
		objectFact("last-editor", "101"),
		aggregatedFact("the-deleter", "cm-parent", "delete"),
	)
	harness.waitForFacts(3)

	// The removal must reach the delete fact. Answering with the uid tier's write fact would both
	// name the wrong actor and, worse, keep the caller waiting out the whole grace for evidence that
	// is already here — blocking the watch shard for every later event of this type.
	removal := harness.resolve(namedQuery(factIndexTestUID, "999", "cm-parent", false))
	require.Equal(t, AttributionName, removal.Result)
	require.Equal(t, "the-deleter", removal.Fact.Author)
}

func TestFactIndex_ANameKeyedWriteDoesNotOutrankTheObjectsOwnUIDFact(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	// Only a fact about the DELETION may jump ahead of the uid tier. A name-keyed WRITE is not that,
	// and must not displace the object's own uid-keyed evidence.
	harness.publish(factIndexTestStream("prod-eu-1"),
		objectFact("uid-tier", "101"),
		aggregatedFact("name-tier-write", "cm-parent", "update"),
	)
	harness.waitForFacts(3)

	removal := harness.resolve(namedQuery(factIndexTestUID, "999", "cm-parent", false))
	require.Equal(t, "uid-tier", removal.Fact.Author)
}

func TestFactIndex_ARemovalStillPrefersItsOwnUIDKeyedDeleteFact(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	uidDelete := objectFact("uid-deleter", "102")
	uidDelete.Verb = "delete"
	harness.publish(factIndexTestStream("prod-eu-1"),
		uidDelete,
		aggregatedFact("name-deleter", "cm-parent", "delete"),
	)
	harness.waitForFacts(3)

	// Both are about the deletion; the uid-keyed one identifies the object exactly, so it wins.
	removal := harness.resolve(namedQuery(factIndexTestUID, "999", "cm-parent", false))
	require.Equal(t, "uid-deleter", removal.Fact.Author)
}

func TestFactIndex_NameFactWakesAWaiterThatArrivedFirst(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	harness.index.Streams().Acquire(key)

	// The watch beats the audit event, which is the ordinary case. The waiter must be registered
	// under the name tier too, or the query sleeps out its whole grace beside a fact that would match.
	resolved := make(chan AuthorResolution, 1)
	go func() {
		resolved <- harness.resolve(namedQuery("uid-9", "77", "fl-late", true))
	}()

	require.NoError(t, harness.transport.PublishFacts(t.Context(), key,
		[]AuthorFact{aggregatedFact("alice", "fl-late", "update")}))

	select {
	case resolution := <-resolved:
		require.Equal(t, AttributionName, resolution.Result)
		require.Equal(t, "alice", resolution.Fact.Author)
	case <-time.After(factIndexTestGrace * 2):
		t.Fatal("a name-tier fact never woke the waiting query")
	}
}

func TestFactIndex_TrimGapIsCountedAndNamed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	index := NewFactIndex(FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")

	index.reportGaps(t.Context(), []FactStreamGap{{Key: key, Cursor: "1-0", FirstSurviving: "9-0"}})

	gaps, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_fact_stream_gaps_total",
		map[string]string{"stream": key.String()})
	require.True(t, ok)
	require.Equal(t, int64(1), gaps)
}

func TestFactStreamSet_UnionIsReferenceCounted(t *testing.T) {
	set := NewFactStreamSet()
	var observed [][]FactStreamKey
	set.Observe(func(keys []FactStreamKey) { observed = append(observed, keys) })

	deployments := factIndexTestStream("prod-eu-1")
	configmaps := FactStreamKeyFor("prod-eu-1", schema.GroupResource{Resource: "configmaps"})

	// Two watches covering one type, and one covering another.
	firstWatch := set.Acquire(deployments)
	secondWatch := set.Acquire(deployments)
	releaseConfigMaps := set.Acquire(configmaps)
	require.Equal(t, []FactStreamKey{deployments, configmaps}, set.Keys())

	// The type stays followed while ANY watch covers it.
	firstWatch()
	require.Equal(t, []FactStreamKey{deployments, configmaps}, set.Keys())
	firstWatch() // Releasing twice must not unfollow a type another watch still needs.
	require.Equal(t, []FactStreamKey{deployments, configmaps}, set.Keys())

	secondWatch()
	require.Equal(t, []FactStreamKey{configmaps}, set.Keys())
	releaseConfigMaps()
	require.Empty(t, set.Keys())
	require.Zero(t, set.Len())

	// The observer sees the set on install and on every change to its membership, never on a
	// reference count that changed under an already-followed type.
	require.Equal(t, [][]FactStreamKey{
		{},
		{deployments},
		{deployments, configmaps},
		{configmaps},
		{},
	}, observed)
}

func TestFactIndex_FollowerPicksUpATypeWhenAWatchStartsCoveringIt(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")

	// Written before anything watches the type: the stream exists and nobody follows it.
	require.NoError(t, harness.transport.PublishFacts(t.Context(), key, []AuthorFact{objectFact("alice", "101")}))
	require.Equal(t, AttributionAbsent, harness.index.Lookup(objectQuery("prod-eu-1", "uid-1", "101", true)).Result)

	// A new watch on that type replays the retention window rather than starting empty.
	release := harness.index.Streams().Acquire(key)
	defer release()
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)
}

// TestFactIndex_AFactAgesFromWhenItWasAppended pins the TTL against the delivery path.
//
// The follower replays the whole retention window on start, so an entry appended nine minutes ago
// is read now. Stamping it with the READ time would hand it a second full TTL, and a process that
// restarted every nine minutes would keep facts alive forever — the horizon would bound nothing.
// The same applies to a follower that fell behind, and to any transport that hands back an entry
// its own retention should have dropped.
func TestFactIndex_AFactAgesFromWhenItWasAppended(t *testing.T) {
	index := NewFactIndex(FactIndexConfig{TTL: time.Minute})
	key := factIndexTestStream("prod-eu-1")

	// An entry whose position says it was appended two minutes ago, delivered now.
	stale := FactEntry{
		Key:   key,
		ID:    streamIDAt(time.Now().Add(-2 * time.Minute)),
		Facts: []AuthorFact{objectFact("alice", "101")},
	}
	index.Apply(t.Context(), stale)

	require.Equal(t, AttributionAbsent,
		index.Lookup(objectQuery("prod-eu-1", factIndexTestUID, "101", true)).Result,
		"a fact older than the TTL must not become joinable just because it was read late")

	// The same entry appended now is joinable, so the check is on age and not on the ID's shape.
	fresh := FactEntry{Key: key, ID: streamIDAt(time.Now()), Facts: []AuthorFact{objectFact("alice", "101")}}
	index.Apply(t.Context(), fresh)
	require.Equal(t, "alice",
		index.Lookup(objectQuery("prod-eu-1", factIndexTestUID, "101", true)).Fact.Author)
}

// TestFactIndex_OneFactServesEveryGitTargetWaitingForIt is the fan-in property, which is the reason
// there is one index per process rather than one per GitTarget.
//
// Two GitTargets mirroring the same object each run their own watch shard, so each gets its own
// watch event and resolves independently — but the fact naming the author is about a write that
// happened in Kubernetes, not about who is interested in it. Both therefore resolve from the SAME
// stored fact. Nothing is consumed and nothing competes: waking is a broadcast over the set of
// waiters on a key, so there is no "winner" and no second copy.
func TestFactIndex_OneFactServesEveryGitTargetWaitingForIt(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	query := objectQuery("prod-eu-1", factIndexTestUID, "101", true)

	// Both shards ask before the fact exists, so both park on the waiter registry.
	const consumers = 2
	results := make(chan AuthorResolution, consumers)
	for range consumers {
		go func() {
			results <- harness.index.Await(t.Context(), query, 5*time.Second)
		}()
	}
	require.Eventually(t, func() bool { return harness.index.waiters.len() > 0 },
		2*time.Second, 5*time.Millisecond, "both resolvers must be registered before the fact lands")

	// ONE fact is published, once.
	harness.publish(key, objectFact("alice", "101"))

	for range consumers {
		select {
		case resolution := <-results:
			require.Equal(t, "alice", resolution.Fact.Author)
			require.Equal(t, AttributionExact, resolution.Result)
		case <-time.After(10 * time.Second):
			t.Fatal("a waiter was never woken: waking must reach every resolver on the key, not one")
		}
	}

	// The index holds that one fact, not one per consumer, and every waiter is gone.
	require.Equal(t, 2, harness.index.Len(), "one write is one exact entry plus one latest entry")
	require.Zero(t, harness.index.waiters.len(), "a resolver must leave nothing registered behind")

	// A third shard arriving after the fact is already stored takes the fast path instead: it
	// registers, finds it on the immediate check, and never blocks.
	late := harness.index.Await(t.Context(), query, 0)
	require.Equal(t, "alice", late.Fact.Author)
}

// TestFactIndex_ARemovalWaitsForEvidenceAboutTheDeletion is the race the collection-precedence fix
// alone did not close, and the one that made an e2e spec pass at one process and fail at four.
//
// Precedence only decides between facts that are BOTH present. The watch event reliably arrives
// before the audit batch carrying its delete — that is the entire reason the grace window exists —
// so at the moment a removal is resolved, the only fact present is often the object's last WRITE.
// Returning on it answered "who deleted this" with "who last edited it", and no ordering of the
// tiers could have helped, because the right fact had not been delivered yet.
func TestFactIndex_ARemovalWaitsForEvidenceAboutTheDeletion(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")

	// Bob edits the object. That is the only fact in the index when the removal is resolved.
	harness.publish(key, objectFact("bob", "101"))
	harness.waitForFacts(2)

	// Alice's collection delete is still in flight, and lands while the resolver waits.
	go func() {
		time.Sleep(50 * time.Millisecond)
		harness.publish(key, aliceCollectionFact("", factIndexTestUID))
	}()

	resolution := harness.index.Await(t.Context(),
		removalFactQuery(factIndexTestUID), 5*time.Second)
	require.Equal(t, AttributionDeleteCollectionBodyUID, resolution.Result)
	require.Equal(t, "alice", resolution.Fact.Author,
		"a removal must wait for evidence about the deletion, not settle for the last edit")
}

// Waiting must never cost an attribution. When nothing better arrives, the write fact that was held
// back is returned exactly as it would have been returned immediately — the only difference is the
// wait, and a removal that spends its grace is the case the grace window is for.
func TestFactIndex_ARemovalStillNamesTheLastWriterWhenNothingBetterArrives(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	harness.publish(factIndexTestStream("prod-eu-1"), objectFact("bob", "101"))
	harness.waitForFacts(2)

	start := time.Now()
	resolution := harness.index.Await(t.Context(),
		removalFactQuery(factIndexTestUID), 150*time.Millisecond)

	require.Equal(t, AttributionLatest, resolution.Result)
	require.Equal(t, "bob", resolution.Fact.Author, "waiting must not turn a match into an absence")
	require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond,
		"the fallback is only taken once the grace has actually elapsed")
}

// An object's OWN delete fact is the strongest evidence a removal can have, so it ends the wait
// immediately rather than holding out for a collection fact that may never come.
func TestFactIndex_ARemovalsOwnDeleteFactEndsTheWaitAtOnce(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	deleteFact := objectFact("carol", "")
	deleteFact.Verb = "delete"
	harness.publish(factIndexTestStream("prod-eu-1"), deleteFact)
	harness.waitForFacts(1)

	start := time.Now()
	resolution := harness.index.Await(t.Context(),
		removalFactQuery(factIndexTestUID), 5*time.Second)

	require.Equal(t, "carol", resolution.Fact.Author)
	require.Less(t, time.Since(start), 2*time.Second, "a delete fact must not be held as a fallback")
}

// TestFactIndex_TheRemovalPointerOutlivesTheTTL pins the horizon the removal pointer deliberately
// does not share with the rest of the index.
//
// This is the replay case. An object left Terminating by a hung finalizer is first OBSERVED hours
// later, after a restart, a rollout, or a 410 rebuild collapses the watch to CURRENT state: there is
// no transition event, the file is still in Git, and that first observation renders as a DELETE.
// Every ordinary tier holding the deleter's fact aged out long before, so the commit is authored
// unresolved — after sitting out the whole grace window on the shard's serial goroutine, waiting for
// a fact that can never arrive. The pointer turns that into an immediate hit.
func TestFactIndex_TheRemovalPointerOutlivesTheTTL(t *testing.T) {
	const ttl = 300 * time.Millisecond
	harness := newFactIndexHarness(t, FactIndexConfig{TTL: ttl})
	deleteFact := objectFact("alice", "101")
	deleteFact.Verb = "delete"
	harness.publish(factIndexTestStream("prod-eu-1"), deleteFact)
	// Exact, latest, and the removal pointer: one delete fact carrying a uid and an rv fills all three.
	harness.waitForFacts(3)

	time.Sleep(3 * ttl)
	require.Equal(t, 2, harness.index.Sweep(time.Now()), "the two TTL-bounded entries are reclaimed")

	// The ordinary tiers are gone, including the exact join for the version the deletion stamped.
	require.Equal(t, AttributionAbsent,
		harness.index.Lookup(objectQuery("prod-eu-1", factIndexTestUID, "101", true)).Result)

	resolution := harness.index.Lookup(removalFactQuery(factIndexTestUID))
	require.Equal(t, AttributionDeleteSticky, resolution.Result,
		"a removal still reaches the pointer once every TTL-bounded tier has expired")
	require.Equal(t, "alice", resolution.Fact.Author)
	require.Equal(t, 1, harness.index.Len(), "and the pointer is what the index is still holding")
}

// TestFactIndex_ARecreatedNameDoesNotInheritThePreviousDeleter pins why the pointer is keyed on the
// uid and nothing else.
//
// A uid is unique across space and time, which is the whole argument for letting the pointer outlive
// the TTL: the statement can never be superseded. A NAME has no such property — it is reused after a
// delete and recreate — so the same stickiness on the name tier would not be this fix, it would be a
// defect that a longer horizon makes MORE likely rather than less. The name tier stays bounded by
// the TTL exactly as before.
func TestFactIndex_ARecreatedNameDoesNotInheritThePreviousDeleter(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	deleted := AuthorFact{
		Namespace: "team-a", UID: "uid-old", Name: "checkout",
		ResourceVersion: "101", Author: "alice", Verb: "delete",
	}
	recreated := AuthorFact{
		Namespace: "team-a", UID: "uid-new", Name: "checkout",
		ResourceVersion: "202", Author: "bob", Verb: "update",
	}
	harness.publish(key, deleted, recreated)
	harness.waitForFacts(5)

	// The recreated object is removed in its turn. It shares the name, and nothing else.
	removal := FactQuery{
		AuditRoute:      "prod-eu-1",
		GroupResource:   deploymentsGroupResource(),
		UID:             "uid-new",
		ResourceVersion: "999",
		Namespace:       "team-a",
		Name:            "checkout",
	}
	resolution := harness.index.Lookup(removal)
	require.NotEqual(t, "alice", resolution.Fact.Author,
		"the previous object's deleter must not be inherited through a reused name")
	require.Equal(t, "bob", resolution.Fact.Author)
	require.Equal(t, AttributionLatest, resolution.Result, "which leaves the ordinary fallback")

	// The first object's pointer is untouched by any of that: it is a statement about a uid.
	previous := harness.index.Lookup(removalFactQuery("uid-old"))
	require.Equal(t, AttributionDeleteSticky, previous.Result)
	require.Equal(t, "alice", previous.Fact.Author)
}

// removalFactQuery is a DELETE watch event: its resourceVersion is never the one a write produced.
func removalFactQuery(uid string) FactQuery {
	return FactQuery{
		AuditRoute:      "prod-eu-1",
		GroupResource:   deploymentsGroupResource(),
		UID:             uid,
		ResourceVersion: "999",
		Namespace:       "team-a",
		ExactCapable:    false,
	}
}

// TestFactIndex_LatestAndResourceVersionAreDistinctTiers pins the split the metric surface turns
// on. Both used to report "weak", and they are different evidence: the uid tier is the OBJECT's own
// last write, while the rv-only hatch is a fact that carried a resourceVersion and no uid at all.
// The removal path turns on the first specifically, so a reader that cannot tell them apart cannot
// tell a held fallback from an unidentified match.
func TestFactIndex_LatestAndResourceVersionAreDistinctTiers(t *testing.T) {
	harness := newFactIndexHarness(t, FactIndexConfig{})
	key := factIndexTestStream("prod-eu-1")
	rvOnly := AuthorFact{Namespace: "team-a", ResourceVersion: "202", Author: "rv-actor", Verb: "update"}
	harness.publish(key, objectFact("alice", "101"), rvOnly)
	harness.waitForFacts(3)

	// A removal joins the object's own uid: the latest tier.
	removal := harness.resolve(objectQuery("prod-eu-1", factIndexTestUID, "999", false))
	require.Equal(t, AttributionLatest, removal.Result)
	require.Equal(t, "alice", removal.Fact.Author)

	// A fact with no uid is reachable only through the resourceVersion it carried.
	hatch := harness.resolve(objectQuery("prod-eu-1", "", "202", true))
	require.Equal(t, AttributionResourceVersion, hatch.Result)
	require.Equal(t, "rv-actor", hatch.Fact.Author)
}

// TestFactIndex_ActorKindIsDerivedFromTheAuthor covers the second half of the label split: the tier
// says which evidence answered, the actor kind says who it named, and every tier can name either.
func TestFactIndex_ActorKindIsDerivedFromTheAuthor(t *testing.T) {
	require.Equal(t, ActorKindUser, AuthorFact{Author: "alice"}.ActorKind())
	require.Equal(t, ActorKindServiceAccount,
		AuthorFact{Author: "system:serviceaccount:flux-system:kustomize-controller"}.ActorKind())
	require.Equal(t, ActorKindNone, AuthorFact{}.ActorKind())

	// A resolution that matched nothing names nobody, whatever fact it is carrying.
	require.Equal(t, ActorKindNone, AuthorResolution{Result: AttributionAbsent}.ActorKind())
	require.Equal(t, ActorKindUser,
		AuthorResolution{Result: AttributionName, Fact: AuthorFact{Author: "alice"}}.ActorKind())
}

// erroringFollower fails every read, which is the shape of a wedged follower: Run does not give up
// on a transport error, so without a counter and a timestamp the failure is a log line and a slowly
// rising unresolved rate with nothing pointing at the cause.
type erroringFollower struct {
	kind FactTransportKind
	// failures counts the reads that have failed, so a test can wait for the retry loop to turn.
	failures atomic.Int64
}

func (f *erroringFollower) FollowFacts([]FactStreamKey, time.Duration) FactSubscription {
	return f
}

func (f *erroringFollower) TransportKind() FactTransportKind { return f.kind }

func (f *erroringFollower) SetStreams([]FactStreamKey) {}

func (f *erroringFollower) Next(ctx context.Context) (FactDelivery, error) {
	if err := ctx.Err(); err != nil {
		return FactDelivery{}, err
	}
	f.failures.Add(1)
	return FactDelivery{}, errors.New("transport is wedged")
}

func TestFactIndex_FollowerErrorsAreCountedAndTheTransportIsNamed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	index := NewFactIndex(FactIndexConfig{})
	follower := &erroringFollower{kind: FactTransportRedis}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = index.Run(ctx, follower)
	}()
	require.Eventually(t, func() bool { return follower.failures.Load() >= 1 },
		5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	require.NoError(t, runErr, "a transport error is retried, never returned")

	errorCount, found := telemetry.CollectInt64Sum(reader,
		"gitopsreverser_attribution_fact_follower_errors_total",
		map[string]string{"transport": string(FactTransportRedis)})
	require.True(t, found)
	require.Positive(t, errorCount)

	// The info gauge is recorded by the follower, so it is in force for exactly as long as the
	// process is reading facts, and it says which contract the other metrics are read under.
	info, found := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_transport_info",
		map[string]string{"transport": string(FactTransportRedis)})
	require.True(t, found)
	require.Equal(t, int64(1), info)

	// A follower that has only ever errored has never succeeded, so the liveness gauge must not
	// claim it has: this is the difference between "erroring while progressing" and an outage.
	_, succeeded := telemetry.CollectInt64Sum(reader,
		"gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds", nil)
	require.False(t, succeeded)
}

// A successful read stamps the liveness gauge, idle rounds included: the question it answers is
// whether the follower is READING, and a quiet cluster must not look like a wedged follower.
func TestFactIndex_FollowerSuccessStampsTheLivenessGauge(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	before := time.Now().Unix()
	harness := newFactIndexHarness(t, FactIndexConfig{})
	harness.publish(factIndexTestStream("prod-eu-1"), objectFact("alice", "101"))
	harness.waitForFacts(2)

	stamp, found := telemetry.CollectInt64Sum(reader,
		"gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds", nil)
	require.True(t, found)
	require.GreaterOrEqual(t, stamp, before)

	info, found := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_transport_info",
		map[string]string{"transport": string(FactTransportMemory)})
	require.True(t, found)
	require.Equal(t, int64(1), info)
}
