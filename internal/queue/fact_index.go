// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"math"
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

// factOpWritten and factOpMatched are the bounded ops on the fact lifecycle counter: one fact
// appended to the log, and one joined by a watch event. Read together they say how much of what is
// published is ever used — the ratio that decides whether a type is worth following at all.
const (
	factOpWritten = "written"
	factOpMatched = "matched"
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
	// Name serves the name tier only, the floor reached when a fact carries neither a uid nor a
	// resourceVersion. The watch side always knows it; only the audit side can be missing it.
	Name string
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
// A fact ages from when it was APPENDED, not from when this process happened to read it. The two
// differ by more than a hair in the case that matters most: the follower replays the whole retention
// window on start, so stamping those entries with the read time would hand every one of them a
// second full TTL and let a restart resurrect facts the horizon had already retired. A follower that
// falls behind, or a transport that hands back an entry its own retention should have dropped, lands
// in the same place. Reading the append time off the entry's position makes the TTL mean the same
// thing on both transports and on every delivery path, which is what SweepInterval bounding memory
// rather than correctness depends on.
func (i *FactIndex) Apply(ctx context.Context, entry FactEntry) {
	scope := factScope{route: entry.Key.AuditRoute, groupResource: entry.Key.groupResource()}
	at := entryAppendTime(entry.ID, time.Now())
	for _, fact := range entry.Facts {
		i.waiters.wake(i.store(ctx, scope, fact, at))
	}
}

// entryAppendTime reads the append time out of a transport position. Stream IDs are millisecond
// timestamps in both implementations, so a position IS a time and needs no side channel.
//
// It falls back to now for a position it cannot read, and clamps a future one: a fact must never
// age SLOWER than the clock because a transport handed back a malformed or skewed ID, which is the
// one direction that would extend a fact's life beyond its TTL rather than shorten it.
func entryAppendTime(id string, now time.Time) time.Time {
	millis, _ := parseStreamID(id)
	// A position past the int64 millisecond range is not a time this code can read, and neither is
	// zero. Both fall back to now, which ages the fact from this moment rather than from never.
	if millis == 0 || millis > math.MaxInt64 {
		return now
	}
	at := time.UnixMilli(int64(millis))
	if at.After(now) {
		return now
	}
	return at
}

// Await resolves a watch event, waiting up to grace for a fact that has not been delivered yet. It
// returns an AttributionAbsent resolution when nothing matched in time; it never blocks longer than
// the grace and never returns an error path.
//
// The order of the first two statements is the design, not a detail. The waiter is registered
// BEFORE the index is read, so a fact applied in the gap between the two signals a waiter that is
// already listening. Checking first and registering after loses exactly that fact — the race the
// poll loop used to paper over by looking again.
//
// A match does not always end the wait. For a REMOVAL, the strongest fact present early is often
// the object's last WRITE, which says who edited it and nothing about who deleted it — and the
// watch event reliably beats the audit batch that carries the delete, which is the entire reason
// the grace window exists. Returning on that first match answered "who deleted this" with "who last
// edited it", every time an object was touched by someone else before being removed. Such a match
// is held as a FALLBACK instead: the wait continues for evidence about the deletion itself, and the
// fallback is returned only when the grace expires without any arriving. Attribution is never lost
// by waiting — the worst case returns exactly what returning early would have.
func (i *FactIndex) Await(ctx context.Context, query FactQuery, grace time.Duration) AuthorResolution {
	waiter := i.waiters.register(query.waiterKeys())
	defer i.waiters.unregister(waiter)

	fallback := AuthorResolution{Result: AttributionAbsent}
	if resolution := i.Lookup(query); resolution.Result != AttributionAbsent {
		if !query.awaitsBetterEvidence(resolution) {
			recordFactEvent(ctx, factOpMatched)
			return resolution
		}
		fallback = resolution
	}
	if grace <= 0 {
		return i.settle(ctx, fallback)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return i.settle(ctx, fallback)
		case <-timer.C:
			return i.settle(ctx, fallback)
		case <-waiter.ch:
			resolution := i.Lookup(query)
			if resolution.Result == AttributionAbsent {
				continue
			}
			if !query.awaitsBetterEvidence(resolution) {
				recordFactEvent(ctx, factOpMatched)
				return resolution
			}
			fallback = resolution
		}
	}
}

// settle returns the fallback a removal was holding, counting it as the match it is. Waiting is
// never allowed to turn an attribution into an absence: a removal whose only evidence is the last
// write still names that writer once nothing better has arrived.
func (i *FactIndex) settle(ctx context.Context, fallback AuthorResolution) AuthorResolution {
	if fallback.Result != AttributionAbsent {
		recordFactEvent(ctx, factOpMatched)
	}
	return fallback
}

// awaitsBetterEvidence reports whether a match should be held as a fallback rather than returned.
//
// It is true for exactly one shape: a REMOVAL matched to a fact that is not about a removal. The
// sticky removal pointer is about the deletion by construction — only a removal fact is ever filed
// there — so a match on it ends the wait at once. The
// per-object tiers are last-writer-wins, so for a collection member — whose delete files one fact
// about the collection rather than one per object — they hold whoever edited it last. Both
// collection tiers are about the deletion itself and end the wait, as does a per-object fact whose
// own verb is a delete: that is the object's own removal fact, which is the strongest thing a
// removal can hope for.
func (q FactQuery) awaitsBetterEvidence(resolution AuthorResolution) bool {
	if q.ExactCapable || resolution.Result == AttributionAbsent {
		return false
	}
	switch resolution.Result {
	case AttributionDeleteSticky, AttributionDeleteCollectionBodyUID, AttributionDeleteCollectionScope:
		return false
	case AttributionExact, AttributionLatest, AttributionResourceVersion,
		AttributionName, AttributionAbsent:
	}
	return !isRemovalVerb(resolution.Fact.Verb)
}

// isRemovalVerb reports whether a fact describes a deletion rather than a write.
func isRemovalVerb(verb string) bool {
	return strings.EqualFold(verb, "delete") || strings.EqualFold(verb, deleteCollectionVerb)
}

// Lookup reads the index once, trying the tiers strongest-first:
//
//  1. the sticky removal pointer for that uid, for a removal only;
//  2. the exact (uid, rv) fact, the only exact-capable join;
//  3. a collection fact whose uid set contains this object;
//  4. the last-writer-wins fact for that uid, for a removal whose rv never matches;
//  5. a collection fact whose scope, selector, and window cover it;
//  6. the rv-only escape hatch;
//  7. the (namespace, name) floor.
//
// The name tier is last because it is the weakest per-object evidence here: a name is reused after a
// delete and recreate, so it can name the author of a previous object that held it, where a uid
// cannot and an rv identifies one specific write. Nothing that carries a uid or an rv ever reaches
// it, so ranking it last costs the stronger tiers nothing and only picks up what they cannot express.
//
// Precedence is the correctness argument for the collection tiers, and the two of them sit on
// OPPOSITE sides of the latest tier on purpose.
//
// Uid membership outranks it because the two tiers answer different questions. The latest tier says
// who last WROTE an object; a removal asks who DELETED it. For a single-object delete those coincide,
// because the delete files its own fact under that uid — but a collection delete files one fact about
// the collection, so the uid's latest entry is left holding whoever happened to write the object last.
// Ranking it above the collection's uid set credited a removal to the previous editor and never
// reached the actor who actually ran the delete, which is the one thing the deleted expander did get
// right: it overwrote that entry per object. Uid membership is the API server stating that THIS
// request deleted THIS object, so nothing weaker may answer ahead of it.
//
// Scope matching stays below, because it is the weakest evidence here and can name the wrong human:
// an unrelated delete by another actor during the same window is claimed by its own fact at tier 3
// and never reaches tier 4.
func (i *FactIndex) Lookup(query FactQuery) AuthorResolution {
	now := time.Now()
	cutoff := now.Add(-i.ttl)

	i.mu.Lock()
	defer i.mu.Unlock()
	facts, ok := i.scopes[query.scope()]
	if !ok {
		return AuthorResolution{Result: AttributionAbsent}
	}

	// The removal pointer is consulted BEFORE the exact tier, and only for an event that is not
	// exact-capable. That ordering is half of the fix and useless without the other half: the slot is
	// what preserves the deleter's fact, and being asked first is what makes it reachable. A removal's
	// own resourceVersion is the one the DELETION stamped, so the exact tier can hold a fact about the
	// finalizer patch that carries the very same version — a stronger-looking answer to a question it
	// is not about. An exact-capable event never reaches here, so a create or update still resolves at
	// the exact tier exactly as before.
	if !query.ExactCapable {
		if resolution := stickyRemoval(facts, query); resolution.Result != AttributionAbsent {
			return resolution
		}
	}
	if query.UID != "" && query.ResourceVersion != "" {
		if fact, found := facts.lookupExact(query.UID, query.ResourceVersion, cutoff); found {
			return AuthorResolution{Fact: fact, Result: AttributionExact}
		}
	}
	if !query.ExactCapable {
		if resolution := i.lookupRemoval(facts, query, now, cutoff); resolution.Result != AttributionAbsent {
			return resolution
		}
	}
	if query.ResourceVersion != "" {
		if fact, found := facts.lookupRV(query.ResourceVersion, cutoff); found {
			return AuthorResolution{Fact: fact, Result: AttributionResourceVersion}
		}
	}
	if query.Name != "" {
		if fact, found := facts.lookupName(query.Namespace, query.Name, cutoff); found {
			return AuthorResolution{Fact: fact, Result: AttributionName}
		}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

// stickyRemoval reads the head of the removal ladder: the uid-keyed slot holding the fact that this
// object was DELETED, which no later fact about a write may overwrite. It is the one tier consulted
// before the exact tier, and only for an event that is not exact-capable.
func stickyRemoval(facts *scopeFacts, query FactQuery) AuthorResolution {
	if query.UID == "" {
		return AuthorResolution{Result: AttributionAbsent}
	}
	if fact, found := facts.lookupRemovalPointer(query.UID); found {
		return AuthorResolution{Fact: fact, Result: AttributionDeleteSticky}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

// lookupRemoval reads the tiers only a removal may consult, in the order they rank: the uid set of
// a collection that named this object, the object's own delete fact (by uid, then by name), its
// last-writer fact, and finally a collection whose scope covers it. See Lookup for why uid
// membership and scope matching sit on opposite sides of the latest tier.
//
// The ordering rule inside it is the one the whole removal path turns on: a fact about the DELETION
// outranks a fact about a write, whichever key each happens to be filed under. A write fact answers
// "who last edited this", which is not the question a removal asks.
func (i *FactIndex) lookupRemoval(
	facts *scopeFacts,
	query FactQuery,
	now, cutoff time.Time,
) AuthorResolution {
	var writeFallback AuthorResolution
	haveWriteFallback := false
	if query.UID != "" {
		if fact, found := facts.matchCollectionUID(query, cutoff); found {
			return AuthorResolution{Fact: fact, Result: AttributionDeleteCollectionBodyUID}
		}
		if fact, found := facts.lookupLatest(query.UID, cutoff); found {
			resolution := AuthorResolution{Fact: fact, Result: AttributionLatest}
			// The object's own delete fact: the strongest thing a removal can hope for below uid
			// membership, so it ends the search here.
			if isRemovalVerb(fact.Verb) {
				return resolution
			}
			// A WRITE fact says who last edited the object, not who deleted it. Hold it and keep
			// looking for evidence about the deletion rather than answering with it.
			writeFallback, haveWriteFallback = resolution, true
		}
	}
	// The object's own delete fact again, this time keyed by NAME, which is the only key it has when
	// the API server answered the delete with a Status rather than the object: there is then no uid
	// to recover from the body (measured in corpus configmap/owner-ref-cascade, where the parent's
	// delete returns Status, against configmap/finalizer-delete, where it returns the ConfigMap).
	//
	// It has to be reachable HERE, above the write fallback, or it is not reachable at all for a
	// removal: returning the uid tier's write fact ends the lookup, and the caller then holds that
	// fact and waits out the whole grace for delete evidence that was sitting in this tier the entire
	// time. That wait is not free — it blocks the watch shard's serial goroutine, so every later
	// event for the type waits behind it.
	if query.Name != "" {
		if fact, found := facts.lookupName(query.Namespace, query.Name, cutoff); found && isRemovalVerb(fact.Verb) {
			return AuthorResolution{Fact: fact, Result: AttributionName}
		}
	}
	if haveWriteFallback {
		return writeFallback
	}
	if fact, found := facts.matchCollectionScope(query, now, cutoff, i.collectionWindow); found {
		return AuthorResolution{Fact: fact, Result: AttributionDeleteCollectionScope}
	}
	return AuthorResolution{Result: AttributionAbsent}
}

// Run follows the subscription set until the context ends, applying what it reads and reporting
// what it lost. It returns only when the context ends: a transport failure is retried, because a
// follower that gave up would leave attribution silently dead for the life of the process.
func (i *FactIndex) Run(ctx context.Context, follower FactFollower) error {
	transport := follower.TransportKind()
	recordTransportInfo(ctx, transport)
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
			recordFollowerError(ctx, transport)
			i.log.Error(err, "attribution fact follower failed; retrying")
			if waitErr := waitBlock(ctx, factFollowErrorBackoff); waitErr != nil {
				return nil
			}
			continue
		}
		// An idle round counts as success: the question the gauge answers is whether the follower is
		// READING, and a block period that elapsed with nothing on any stream is a healthy read. Only
		// advancing it on a non-empty delivery would make a quiet cluster look like a wedged follower.
		recordFollowerSuccess(ctx)
		for _, entry := range delivery.Entries {
			i.Apply(ctx, entry)
		}
		i.reportGaps(ctx, delivery.Gaps)
		if now := time.Now(); now.Sub(lastSweep) >= i.sweepInterval {
			i.Sweep(now)
			lastSweep = now
			i.recordSize(ctx)
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
		if isRemovalVerb(fact.Verb) {
			// A fact about a DELETION also takes the sticky removal slot, which a later fact about a
			// WRITE may not overwrite. The ordinary filing above still happens: the slot answers the
			// removal question, and the tiers keep answering the ones they always did.
			//
			// It needs no waiter key of its own, because a fact reaching here always fills the latest
			// tier too, and a removal query already waits on that key.
			facts.putRemoval(fact.UID, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
		}
		return append(keys, factWaiterKey{scope: scope, kind: factKindLatest, value: fact.UID})
	case fact.ResourceVersion != "":
		facts.putRV(fact.ResourceVersion, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
		return []factWaiterKey{{scope: scope, kind: factKindRV, value: fact.ResourceVersion}}
	case fact.Name != "":
		// The floor: no uid and no resourceVersion, so only the name can reach it. This is the
		// aggregated-API write — the API server proxied the request and never decoded the response,
		// so the objectRef holds the name from the URL path and nothing else. Such a fact used to be
		// published and then dropped here as unjoinable, which is why an aggregated update or single
		// delete shipped committer-authored no matter who ran it.
		facts.putName(fact.Namespace, fact.Name, &indexedFact{fact: fact, at: now, seq: i.nextSeq()})
		return []factWaiterKey{{
			scope: scope, kind: factKindName, value: nameWaiterValue(fact.Namespace, fact.Name),
		}}
	default:
		// A fact with no uid, no resourceVersion and no name can never be joined: nothing about it
		// identifies an object. An aggregated CREATE is the case that lands here, because the API
		// server assigns the name and the objectRef carries none — though the publish side rejects it
		// at the name gate before it reaches this far.
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
	if q.Name != "" {
		keys = append(keys, factWaiterKey{
			scope: scope, kind: factKindName, value: nameWaiterValue(q.Namespace, q.Name),
		})
	}
	return keys
}

// exactWaiterValue renders the exact tier's (uid, rv) pair as one waiter value. The separator is a
// byte neither half can contain.
func exactWaiterValue(uid, rv string) string {
	return uid + "\x00" + rv
}

// nameWaiterValue renders the name tier's (namespace, name) pair as one waiter value, on the same
// separator and for the same reason.
func nameWaiterValue(namespace, name string) string {
	return namespace + "\x00" + name
}

// recordSize publishes how much the index holds. It is sampled on the sweep rather than on every
// applied fact: the number is a memory reading, and the sweep is when it has just changed most.
// The v1 gauge cost a Redis SCAN of the whole fact keyspace to produce this; it is now a field read.
func (i *FactIndex) recordSize(ctx context.Context) {
	if telemetry.AttributionFactIndexEntries == nil {
		return
	}
	telemetry.AttributionFactIndexEntries.Record(ctx, int64(i.Len()))
}

// recordFactEvent counts one fact lifecycle event under its bounded op.
func recordFactEvent(ctx context.Context, op string) {
	if telemetry.AttributionFactsTotal == nil {
		return
	}
	telemetry.AttributionFactsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
}

// recordTransportInfo publishes which transport is carrying the facts, as an info gauge whose value
// is always 1. It is recorded from the follower rather than from the wiring because this is the one
// goroutine that runs for the life of the process and holds the transport.
func recordTransportInfo(ctx context.Context, transport FactTransportKind) {
	if telemetry.AttributionTransportInfo == nil {
		return
	}
	telemetry.AttributionTransportInfo.Record(ctx, 1,
		metric.WithAttributes(attribute.String("transport", string(transport))))
}

// recordFollowerError counts one failed follower read. The follower retries rather than returning,
// so without this the failures are a log line and nothing else.
func recordFollowerError(ctx context.Context, transport FactTransportKind) {
	if telemetry.AttributionFactFollowerErrorsTotal == nil {
		return
	}
	telemetry.AttributionFactFollowerErrorsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("transport", string(transport))))
}

// recordFollowerSuccess stamps the time of the last successful read. It matters more than the error
// counter beside it: a counter says errors are happening, and only this separates "erroring
// occasionally while making progress" from "has read nothing in ten minutes" — the second of which
// is attribution degrading to committer-authored cluster-wide.
func recordFollowerSuccess(ctx context.Context) {
	if telemetry.AttributionFactFollowerLastSuccessTimestampSeconds == nil {
		return
	}
	telemetry.AttributionFactFollowerLastSuccessTimestampSeconds.Record(ctx, time.Now().Unix())
}

// recordFactIndexEviction counts one evicted entry under its bounded reason.
func recordFactIndexEviction(ctx context.Context, reason string) {
	if telemetry.AttributionFactIndexEvictionsTotal == nil {
		return
	}
	telemetry.AttributionFactIndexEvictionsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("reason", reason)))
}
