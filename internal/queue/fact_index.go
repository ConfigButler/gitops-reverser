// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// Defaults for the in-memory fact index. Redis used to enforce the TTL and the memory ceiling for
// free; holding the facts in process moves both jobs here, which is why every one of these numbers
// is a bound rather than a hint.
const (
	// DefaultFactIndexMaxFactsPerType caps one (audit route, group/resource)'s entries across all
	// four match structures. It is the primary cap because it is the fair one: a burst on one noisy
	// type — a deletecollection over ten thousand objects, a large rollout — must not evict every
	// other type's facts. One entry is a fact plus its bookkeeping, a few hundred bytes, so a type
	// at its cap costs low single-digit megabytes.
	DefaultFactIndexMaxFactsPerType = 4096

	// DefaultFactIndexMaxFactsTotal caps the whole index, so the pod's memory is bounded by a number
	// that does not scale with how many types happen to be watched. It sits well above the per-type
	// cap: reaching it takes many types simultaneously busy, and eviction then falls on the type
	// holding the most.
	DefaultFactIndexMaxFactsTotal = 65536

	// DefaultFactCollectionWindow is how long after a deletecollection's stageTimestamp a removal in
	// its scope may still be credited to it. It is far shorter than the fact TTL and can afford to
	// be: under the deletion-as-intent rule the removal being attributed happens at delete-REQUEST
	// time, so finalizers do not stretch it, and the window only has to cover audit batching plus
	// clock skew. Ten times the default grace window leaves room for a slow batch without letting an
	// unrelated delete a minute later be claimed.
	DefaultFactCollectionWindow = 30 * time.Second

	// DefaultFactIndexSweepInterval is how often aged-out entries are reclaimed. It only bounds
	// MEMORY, never correctness: a lookup checks the TTL itself, so an entry past its horizon is
	// never joined merely because the sweep has not run yet.
	DefaultFactIndexSweepInterval = 30 * time.Second

	// factFollowErrorBackoff paces retries when the transport fails. The follower does not give up
	// on a transport error: a follower that returned would leave attribution silently dead for the
	// life of the process.
	factFollowErrorBackoff = time.Second

	// deleteCollectionVerb is the one verb published as a fact about a COLLECTION rather than about
	// an object.
	deleteCollectionVerb = "deletecollection"
)

// evictionReasonPerType and evictionReasonTotal are the bounded reasons on the eviction counter.
// They are separate because they mean different things to an operator: per-type says one type is
// hotter than its share, total says the whole index is under pressure.
const (
	evictionReasonPerType = "per_type"
	evictionReasonTotal   = "total"
)

// FactQuery is one watch event's identity, as the join reads it. It is everything the index needs
// to try all five tiers, so a caller assembles it once rather than threading five arguments.
type FactQuery struct {
	// AuditRoute partitions the index. It leads every key for the same reason the streams are named
	// per route: a fact from cluster A must never name the author of an object watched on cluster B.
	AuditRoute      string
	GroupResource   schema.GroupResource
	UID             string
	ResourceVersion string
	// Namespace and Labels serve the collection tier only: they are how a removal finds the
	// deletecollection whose scope covered it.
	Namespace string
	Labels    map[string]string
	// ExactCapable is true for ADDED and MODIFIED, whose resourceVersion is the one the write
	// produced. A removal's is not, so it consults the weaker tiers the exact-capable events skip.
	ExactCapable bool
}

// FactIndexConfig configures the index. Every zero field falls back to its Default… constant, so
// the zero value is the supported configuration.
type FactIndexConfig struct {
	// TTL is how long a fact stays joinable, and doubles as the follower's replay horizon so a
	// restart warms the index with exactly the window that is still usable.
	TTL time.Duration
	// MaxFactsPerType caps one (route, group/resource); MaxFactsTotal caps the whole index.
	MaxFactsPerType int
	MaxFactsTotal   int
	// CollectionWindow bounds the scope-matching tier.
	CollectionWindow time.Duration
	// SweepInterval is how often aged-out entries are reclaimed.
	SweepInterval time.Duration
	Log           logr.Logger
}

// FactIndex is the transport-agnostic half of attribution: the four match structures the join reads,
// the waiter registry a blocked resolver parks on, and the loop that fills both from whatever
// transport it was handed.
//
// There is exactly ONE index per process, not one per GitTarget. A fact names a write that happened
// in Kubernetes, not a consumer interested in it, so one fact already serves every GitTarget that
// needs it; five GitTargets mirroring one Deployment would otherwise hold five copies of every fact
// and bill memory against a number that has nothing to do with how much is happening in the cluster.
// The fan-out that does do useful work is the SUBSCRIPTION set, which is per type.
type FactIndex struct {
	ttl              time.Duration
	maxPerType       int
	maxTotal         int
	collectionWindow time.Duration
	sweepInterval    time.Duration
	log              logr.Logger

	streams *FactStreamSet
	waiters *factWaiterRegistry

	mu     sync.Mutex
	scopes map[factScope]*scopeFacts
	total  int
	seq    uint64
}

// NewFactIndex builds an empty index.
func NewFactIndex(cfg FactIndexConfig) *FactIndex {
	index := &FactIndex{
		ttl:              cfg.TTL,
		maxPerType:       cfg.MaxFactsPerType,
		maxTotal:         cfg.MaxFactsTotal,
		collectionWindow: cfg.CollectionWindow,
		sweepInterval:    cfg.SweepInterval,
		log:              cfg.Log,
		streams:          NewFactStreamSet(),
		waiters:          newFactWaiterRegistry(),
		scopes:           map[factScope]*scopeFacts{},
	}
	if index.ttl <= 0 {
		index.ttl = DefaultAttributionFactTTL
	}
	if index.maxPerType <= 0 {
		index.maxPerType = DefaultFactIndexMaxFactsPerType
	}
	if index.maxTotal <= 0 {
		index.maxTotal = DefaultFactIndexMaxFactsTotal
	}
	if index.collectionWindow <= 0 {
		index.collectionWindow = DefaultFactCollectionWindow
	}
	if index.sweepInterval <= 0 {
		index.sweepInterval = DefaultFactIndexSweepInterval
	}
	return index
}

// Apply stores one delivered entry's facts and wakes whoever was waiting for them. Facts are
// applied in the order they were delivered, which is what makes the latest tier last-writer-wins
// mean the last fact APPENDED rather than whichever goroutine reached the map first.
func (i *FactIndex) Apply(ctx context.Context, entry FactEntry) {
	scope := factScope{route: entry.Key.AuditRoute, groupResource: entry.Key.groupResource()}
	now := time.Now()
	for _, fact := range entry.Facts {
		i.waiters.wake(i.store(ctx, scope, fact, now))
	}
}

// Await resolves a watch event, waiting up to grace for a fact that has not been delivered yet. It
// returns an AttributionAbsent resolution when nothing matched in time; it never blocks longer than
// the grace and never returns an error path.
//
// The order of the first two statements is the design, not a detail. The waiter is registered
// BEFORE the index is read, so a fact applied in the gap between the two signals a waiter that is
// already listening. Checking first and registering after loses exactly that fact — the race the
// poll loop used to paper over by looking again.
func (i *FactIndex) Await(ctx context.Context, query FactQuery, grace time.Duration) AuthorResolution {
	waiter := i.waiters.register(query.waiterKeys())
	defer i.waiters.unregister(waiter)

	if resolution := i.Lookup(query); resolution.Result != AttributionAbsent {
		return resolution
	}
	if grace <= 0 {
		return AuthorResolution{Result: AttributionAbsent}
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return AuthorResolution{Result: AttributionAbsent}
		case <-timer.C:
			return AuthorResolution{Result: AttributionAbsent}
		case <-waiter.ch:
			if resolution := i.Lookup(query); resolution.Result != AttributionAbsent {
				return resolution
			}
		}
	}
}

// Lookup reads the index once, trying the tiers strongest-first:
//
//  1. the exact (uid, rv) fact, the only exact-capable join;
//  2. the last-writer-wins fact for that uid, for a removal whose rv never matches;
//  3. a collection fact whose uid set contains this object;
//  4. a collection fact whose scope, selector, and window cover it;
//  5. the rv-only escape hatch.
//
// Precedence is the correctness argument for the collection tiers. A scope match is the weakest
// evidence here and can name the wrong human, so it is only ever reached when nothing more specific
// applies: an unrelated delete by another actor during the same window is claimed by its own fact
// at tier 2 and never reaches tier 4.
func (i *FactIndex) Lookup(query FactQuery) AuthorResolution {
	now := time.Now()
	cutoff := now.Add(-i.ttl)

	i.mu.Lock()
	defer i.mu.Unlock()
	facts, ok := i.scopes[query.scope()]
	if !ok {
		return AuthorResolution{Result: AttributionAbsent}
	}

	if query.UID != "" && query.ResourceVersion != "" {
		if fact, found := facts.lookupExact(query.UID, query.ResourceVersion, cutoff); found {
			return AuthorResolution{Fact: fact, Result: attributionResultForFact(fact, false)}
		}
	}
	if !query.ExactCapable {
		if query.UID != "" {
			if fact, found := facts.lookupLatest(query.UID, cutoff); found {
				return AuthorResolution{Fact: fact, Result: attributionResultForFact(fact, true)}
			}
		}
		resolution := facts.matchCollection(query, now, cutoff, i.collectionWindow)
		if resolution.Result != AttributionAbsent {
			return resolution
		}
	}
	if query.ResourceVersion != "" {
		if fact, found := facts.lookupRV(query.ResourceVersion, cutoff); found {
			return AuthorResolution{Fact: fact, Result: attributionResultForFact(fact, true)}
		}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

// Run follows the subscription set until the context ends, applying what it reads and reporting
// what it lost. It returns only when the context ends: a transport failure is retried, because a
// follower that gave up would leave attribution silently dead for the life of the process.
func (i *FactIndex) Run(ctx context.Context, follower FactFollower) error {
	subscription := follower.FollowFacts(i.streams.Keys(), i.ttl)
	i.streams.Observe(subscription.SetStreams)
	defer i.streams.Observe(nil)

	lastSweep := time.Now()
	for {
		delivery, err := subscription.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			i.log.Error(err, "attribution fact follower failed; retrying")
			if waitErr := waitBlock(ctx, factFollowErrorBackoff); waitErr != nil {
				return nil
			}
			continue
		}
		for _, entry := range delivery.Entries {
			i.Apply(ctx, entry)
		}
		i.reportGaps(ctx, delivery.Gaps)
		if now := time.Now(); now.Sub(lastSweep) >= i.sweepInterval {
			i.Sweep(now)
			lastSweep = now
		}
	}
}

// Streams is the reference-counted set of (route, group/resource) pairs this process follows. The
// watch side acquires a reference when it starts covering a type and releases it when the last
// watch on that type goes away; Run makes the follower track it.
func (i *FactIndex) Streams() *FactStreamSet {
	return i.streams
}

// Sweep reclaims every entry past the TTL horizon. Lookups already ignore aged-out entries, so this
// bounds memory rather than deciding what may be joined.
func (i *FactIndex) Sweep(now time.Time) int {
	cutoff := now.Add(-i.ttl)
	i.mu.Lock()
	defer i.mu.Unlock()
	removed := 0
	for scope, facts := range i.scopes {
		removed += facts.sweep(cutoff)
		if facts.empty() {
			delete(i.scopes, scope)
		}
	}
	i.total -= removed
	return removed
}

// Len reports how many entries the index holds across every scope and structure.
func (i *FactIndex) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.total
}

// store files one fact under every structure it can serve and returns the waiter keys it filled.
// The policy mirrors the tiers Lookup reads: a collection fact is about a collection and lands only
// there, a uid-bearing fact takes the exact and latest tiers, and the rv-only hatch exists for the
// fact that has an rv and no uid — a uid-bearing fact's rv-only entry would be dead, since the watch
// side always carries a uid.
func (i *FactIndex) store(ctx context.Context, scope factScope, fact AuthorFact, now time.Time) []factWaiterKey {
	i.mu.Lock()
	defer i.mu.Unlock()

	facts := i.scopeFor(scope)
	before := facts.count
	keys := i.file(facts, scope, fact, now)
	i.total += facts.count - before
	i.enforceCaps(ctx, scope, facts)
	return keys
}

// file writes one fact into the structures it can serve.
func (i *FactIndex) file(facts *scopeFacts, scope factScope, fact AuthorFact, now time.Time) []factWaiterKey {
	switch {
	case strings.EqualFold(fact.Verb, deleteCollectionVerb):
		facts.putCollection(newIndexedCollection(fact, now, i.nextSeq()))
		return []factWaiterKey{{scope: scope, kind: factKindCollection, value: fact.Namespace}}
	case fact.UID != "":
		var keys []factWaiterKey
		if fact.ResourceVersion != "" {
			facts.putExact(fact.UID, fact.ResourceVersion, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
			keys = append(
				keys,
				factWaiterKey{
					scope: scope,
					kind:  factKindExact,
					value: exactWaiterValue(fact.UID, fact.ResourceVersion),
				},
			)
		}
		facts.putLatest(fact.UID, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
		return append(keys, factWaiterKey{scope: scope, kind: factKindLatest, value: fact.UID})
	case fact.ResourceVersion != "":
		facts.putRV(fact.ResourceVersion, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
		return []factWaiterKey{{scope: scope, kind: factKindRV, value: fact.ResourceVersion}}
	default:
		// A fact with neither a uid nor a resourceVersion can never be joined. The publish side does
		// not produce one; storing it anyway would only fill the index with entries no query reaches.
		return nil
	}
}

// scopeFor returns the scope's structures, creating them on first use.
func (i *FactIndex) scopeFor(scope factScope) *scopeFacts {
	facts, ok := i.scopes[scope]
	if !ok {
		facts = newScopeFacts()
		i.scopes[scope] = facts
	}
	return facts
}

// nextSeq hands out the sequence number that distinguishes a live entry from a stale eviction
// reference to the entry it replaced.
func (i *FactIndex) nextSeq() uint64 {
	i.seq++
	return i.seq
}

// enforceCaps evicts oldest-first until the scope and the index are both back within their caps.
// Every eviction is COUNTED: an attribution that was dropped because the index was full has to look
// different in the metrics from one that was never published, or the index silently absorbs the
// bursts it was bounded for.
func (i *FactIndex) enforceCaps(ctx context.Context, scope factScope, facts *scopeFacts) {
	for facts.count > i.maxPerType {
		if !facts.evictOldest() {
			break
		}
		i.total--
		recordFactIndexEviction(ctx, evictionReasonPerType)
	}
	if facts.empty() {
		delete(i.scopes, scope)
	}
	for i.total > i.maxTotal {
		if !i.evictFromLargestScope(ctx) {
			break
		}
	}
}

// evictFromLargestScope takes one entry from whichever type is holding the most, which puts the
// pressure of a global overflow on whatever caused it.
func (i *FactIndex) evictFromLargestScope(ctx context.Context) bool {
	var largest *scopeFacts
	var largestScope factScope
	for scope, facts := range i.scopes {
		if largest == nil || facts.count > largest.count {
			largest, largestScope = facts, scope
		}
	}
	if largest == nil || !largest.evictOldest() {
		return false
	}
	i.total--
	recordFactIndexEviction(ctx, evictionReasonTotal)
	if largest.empty() {
		delete(i.scopes, largestScope)
	}
	return true
}

// reportGaps counts and names every stream this follower was trimmed past. A gap is the one loss
// this transport can see, and it is a real degradation: the facts it covers are gone, so the commits
// that needed them will be authored unresolved. Reporting it is why the transport is a log with
// positions rather than fire-and-forget publish and subscribe.
func (i *FactIndex) reportGaps(ctx context.Context, gaps []FactStreamGap) {
	for _, gap := range gaps {
		if telemetry.AttributionFactStreamGapsTotal != nil {
			telemetry.AttributionFactStreamGapsTotal.Add(ctx, 1,
				metric.WithAttributes(attribute.String("stream", gap.Key.String())))
		}
		i.log.Info("attribution fact stream was trimmed past this follower; the facts in the gap are "+
			"lost and the commits that needed them are authored unresolved",
			"stream", gap.Key.String(), "cursor", gap.Cursor, "firstSurviving", gap.FirstSurviving)
	}
}

// scope is the partition this query resolves within.
func (q FactQuery) scope() factScope {
	return factScope{
		route:         q.AuditRoute,
		groupResource: groupResourceKey(q.GroupResource.Group, q.GroupResource.Resource),
	}
}

// waiterKeys are the candidates this query could resolve through — exactly the tiers Lookup tries,
// so a fact that would satisfy the lookup always wakes the waiter, and no fact that would not
// wakes it for nothing.
func (q FactQuery) waiterKeys() []factWaiterKey {
	scope := q.scope()
	var keys []factWaiterKey
	if q.UID != "" && q.ResourceVersion != "" {
		keys = append(keys, factWaiterKey{scope: scope, kind: factKindExact,
			value: exactWaiterValue(q.UID, q.ResourceVersion)})
	}
	if !q.ExactCapable {
		if q.UID != "" {
			keys = append(keys, factWaiterKey{scope: scope, kind: factKindLatest, value: q.UID})
		}
		keys = append(keys, factWaiterKey{scope: scope, kind: factKindCollection, value: q.Namespace})
	}
	if q.ResourceVersion != "" {
		keys = append(keys, factWaiterKey{scope: scope, kind: factKindRV, value: q.ResourceVersion})
	}
	return keys
}

// exactWaiterValue renders the exact tier's (uid, rv) pair as one waiter value. The separator is a
// byte neither half can contain.
func exactWaiterValue(uid, rv string) string {
	return uid + "\x00" + rv
}

// recordFactIndexEviction counts one evicted entry under its bounded reason.
func recordFactIndexEviction(ctx context.Context, reason string) {
	if telemetry.AttributionFactIndexEvictionsTotal == nil {
		return
	}
	telemetry.AttributionFactIndexEvictionsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("reason", reason)))
}
