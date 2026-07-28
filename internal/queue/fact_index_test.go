// SPDX-License-Identifier: Apache-2.0

package queue

import (
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
	require.Equal(t, AttributionExactUser, exact.Result)
	require.Equal(t, "alice", exact.Fact.Author)

	harness.waitForFacts(2)

	// An exact-capable event whose rv does not match must NOT fall through to the latest tier: that
	// pointer may name a different, older author than the create or update this event represents.
	harness.absent(objectQuery("prod-eu-1", "uid-1", "999", true))

	// A removal's rv never matches the write's, so it is the event kind that consults latest.
	removal := harness.resolve(objectQuery("prod-eu-1", "uid-1", "999", false))
	require.Equal(t, AttributionWeak, removal.Result)
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
		require.Equal(t, AttributionCollectionUID, resolution.Result)
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
	require.Equal(t, AttributionCollectionScope, resolution.Result)
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
	require.Equal(t, AttributionCollectionScope, harness.resolve(unlabelled).Result)
}

func TestFactIndex_CollectionScopeMatchStopsAtTheWindow(t *testing.T) {
	const window = 500 * time.Millisecond
	harness := newFactIndexHarness(t, FactIndexConfig{CollectionWindow: window})
	harness.publish(factIndexTestStream("prod-eu-1"), aliceCollectionFact(""))
	harness.waitForFacts(1)
	require.Equal(t, AttributionCollectionScope, harness.resolve(objectQuery("prod-eu-1", "uid-9", "9", false)).Result)

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
	require.Equal(t, AttributionWeak, resolution.Result)
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
	require.Equal(t, AttributionCollectionUID, resolution.Result)
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
		AuthorFact{Author: "nobody", Verb: "update"},
		objectFact("alice", "101"),
	)
	require.Equal(t, "alice", harness.resolve(objectQuery("prod-eu-1", "uid-1", "101", true)).Fact.Author)

	// Two entries for the joinable fact, none for the one no query could ever reach.
	require.Equal(t, 2, harness.index.Len())
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
			require.Equal(t, AttributionExactUser, resolution.Result)
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
