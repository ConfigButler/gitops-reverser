// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"time"

	"k8s.io/apimachinery/pkg/labels"
)

// factKind names one of the four match structures a fact can land in. The set is closed: a fact
// that fits none of them is not stored, because a fact nothing can ever join is only memory.
type factKind uint8

const (
	// factKindExact is (uid, rv), the only exact-capable join, serving ADDED and MODIFIED.
	factKindExact factKind = iota
	// factKindLatest is (uid), last-writer-wins, serving removals whose rv never matches.
	factKindLatest
	// factKindRV is (rv), the escape hatch for a fact with an rv but no uid.
	factKindRV
	// factKindCollection is (namespace), time-bounded, serving removals caused by a
	// deletecollection.
	factKindCollection
)

// factScope is the partition every key in the index leads with: the audit route the facts arrived
// under and the group/resource they are about.
//
// The route is not decoration. The index is one per process while the streams are one per (route,
// group/resource), so an index keyed on the type alone would pool two clusters' facts in one map
// and hand a watch event on cluster B an author from cluster A. The rv-only tier is where that
// bites hardest, because a resourceVersion is opaque and not unique across clusters, and the
// collection tier is where it bites most quietly, because a namespace name says nothing about which
// cluster it is in.
type factScope struct {
	route         string
	groupResource string
}

// exactFactKey is the (uid, rv) pair of the exact tier.
type exactFactKey struct {
	uid string
	rv  string
}

// indexedFact is one stored fact plus the insertion time the TTL sweep reads and the sequence
// number that tells a stale eviction reference from a live entry.
type indexedFact struct {
	fact AuthorFact
	at   time.Time
	seq  uint64
}

// indexedCollection is one deletecollection fact: the actor, the scope it named, the selector it
// expressed, and the set of uids it covered when the API server sent a body.
//
// The selector is parsed here, on the applying goroutine, rather than at lookup: a lookup runs once
// per removal event, so parsing there would parse one collection's selector N times on the watch
// shard's blocking path. What stays at lookup time is the DECISION — try uid membership, fall back
// to scope — not the parsing.
type indexedCollection struct {
	fact  AuthorFact
	at    time.Time
	seq   uint64
	stage time.Time
	// selector is nil when the fact carried none, which matches every object of the type in the
	// namespace, because that is what --all means. invalidSelector is set when the fact carried one
	// that would not parse: such a fact is never scope-matched, since treating it as match-all would
	// name an author over a wider scope than the actor asked for.
	selector        labels.Selector
	invalidSelector bool
	// uids is the set the collection covered, or nil when the body was absent or the set was
	// dropped past its cap. Nil is not empty: it means fall back to scope matching, which is the
	// floor that must work on its own.
	uids map[string]struct{}
}

// newIndexedCollection reduces one collection fact to what the join needs: the parsed selector, the
// uid set as a set, and the delete-request time the window is measured from.
func newIndexedCollection(fact AuthorFact, now time.Time, seq uint64) *indexedCollection {
	entry := &indexedCollection{fact: fact, at: now, seq: seq, stage: parseStageTimestamp(fact.StageTimestamp)}
	if fact.LabelSelector != "" {
		selector, err := labels.Parse(fact.LabelSelector)
		if err != nil {
			entry.invalidSelector = true
		} else {
			entry.selector = selector
		}
	}
	if len(fact.UIDs) > 0 {
		entry.uids = make(map[string]struct{}, len(fact.UIDs))
		for _, uid := range fact.UIDs {
			entry.uids[uid] = struct{}{}
		}
	}
	return entry
}

// parseStageTimestamp reads a fact's stage timestamp, returning the zero time when it carries none
// or carries one that will not parse. The caller then falls back to the insertion time, which is
// later and therefore only ever narrows the window.
func parseStageTimestamp(stamp string) time.Time {
	if stamp == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}
	}
	return at
}

// factRef locates one stored entry for eviction without holding a pointer into a map. The sequence
// number is what makes it safe to keep a stale reference: an entry overwritten since the reference
// was taken has a different sequence, so the reference is skipped rather than removing the newer
// entry that took its place.
type factRef struct {
	kind factKind
	uid  string
	rv   string
	seq  uint64
}

// scopeFacts is one (route, group/resource)'s four match structures, its oldest-first insertion
// order, and its entry count. Bounding per scope rather than globally is what keeps a burst on one
// noisy type — a deletecollection over ten thousand objects, a large rollout — from evicting every
// other type's facts.
type scopeFacts struct {
	exact       map[exactFactKey]*indexedFact
	latest      map[string]*indexedFact
	rvOnly      map[string]*indexedFact
	collections []*indexedCollection
	// order is every live entry's reference in insertion order, oldest first. It may hold stale
	// references, which remove skips.
	order []factRef
	count int
}

func newScopeFacts() *scopeFacts {
	return &scopeFacts{
		exact:  map[exactFactKey]*indexedFact{},
		latest: map[string]*indexedFact{},
		rvOnly: map[string]*indexedFact{},
	}
}

// putExact stores the immutable (uid, rv) fact.
func (s *scopeFacts) putExact(uid, rv string, entry *indexedFact) {
	key := exactFactKey{uid: uid, rv: rv}
	if _, ok := s.exact[key]; !ok {
		s.count++
	}
	s.exact[key] = entry
	s.order = append(s.order, factRef{kind: factKindExact, uid: uid, rv: rv, seq: entry.seq})
}

// putLatest stores the last-writer-wins pointer for an object. Entries are applied in delivery
// order, which is what makes "last writer" mean the last fact appended rather than whichever
// goroutine got there first.
func (s *scopeFacts) putLatest(uid string, entry *indexedFact) {
	if _, ok := s.latest[uid]; !ok {
		s.count++
	}
	s.latest[uid] = entry
	s.order = append(s.order, factRef{kind: factKindLatest, uid: uid, seq: entry.seq})
}

// putRV stores the rv-only escape hatch.
func (s *scopeFacts) putRV(rv string, entry *indexedFact) {
	if _, ok := s.rvOnly[rv]; !ok {
		s.count++
	}
	s.rvOnly[rv] = entry
	s.order = append(s.order, factRef{kind: factKindRV, rv: rv, seq: entry.seq})
}

// putCollection appends one collection fact. Collection facts are not keyed one per namespace:
// two actors may delete collections in one namespace within the same window, and each removal must
// be able to find the one that covered it.
func (s *scopeFacts) putCollection(entry *indexedCollection) {
	s.collections = append(s.collections, entry)
	s.order = append(s.order, factRef{kind: factKindCollection, seq: entry.seq})
	s.count++
}

// lookupExact reads the exact tier.
func (s *scopeFacts) lookupExact(uid, rv string, cutoff time.Time) (AuthorFact, bool) {
	return liveFact(s.exact[exactFactKey{uid: uid, rv: rv}], cutoff)
}

// lookupLatest reads the last-writer-wins tier.
func (s *scopeFacts) lookupLatest(uid string, cutoff time.Time) (AuthorFact, bool) {
	return liveFact(s.latest[uid], cutoff)
}

// lookupRV reads the rv-only escape hatch.
func (s *scopeFacts) lookupRV(rv string, cutoff time.Time) (AuthorFact, bool) {
	return liveFact(s.rvOnly[rv], cutoff)
}

// matchCollectionUID resolves a removal against a collection fact that NAMED this object: the API
// server returned the set it deleted, and this uid was in it. There is no over-attribution risk in
// that — either the object was in the set or it was not — which is why it outranks the latest tier.
//
// It carries no window check, deliberately. The uid set is a statement about this exact object
// rather than about a span of time, so the fact's own TTL is the only bound it needs; a window would
// only discard evidence that cannot be wrong.
func (s *scopeFacts) matchCollectionUID(q FactQuery, cutoff time.Time) (AuthorFact, bool) {
	for i := len(s.collections) - 1; i >= 0; i-- {
		entry := s.collections[i]
		if !entry.covers(q, cutoff) || entry.uids == nil {
			continue
		}
		if _, ok := entry.uids[q.UID]; ok {
			return entry.fact, true
		}
	}
	return AuthorFact{}, false
}

// matchCollectionScope resolves a removal against a collection fact by scope alone: same type and
// namespace, the request's selector accepting this object's labels, within the collection window.
// It is the weakest evidence the join has and the only tier that can name the wrong human, so it
// runs last — after the object's own facts have all missed.
func (s *scopeFacts) matchCollectionScope(
	q FactQuery,
	now, cutoff time.Time,
	window time.Duration,
) (AuthorFact, bool) {
	for i := len(s.collections) - 1; i >= 0; i-- {
		entry := s.collections[i]
		if !entry.covers(q, cutoff) || !entry.inWindow(now, window) || !entry.selects(q.Labels) {
			continue
		}
		return entry.fact, true
	}
	return AuthorFact{}, false
}

// sweep drops every entry inserted before the cutoff and reports how many went. It also compacts
// the eviction order, so a scope that has aged out entirely leaves nothing behind.
func (s *scopeFacts) sweep(cutoff time.Time) int {
	removed := 0
	for key, entry := range s.exact {
		if entry.at.Before(cutoff) {
			delete(s.exact, key)
			removed++
		}
	}
	for key, entry := range s.latest {
		if entry.at.Before(cutoff) {
			delete(s.latest, key)
			removed++
		}
	}
	for key, entry := range s.rvOnly {
		if entry.at.Before(cutoff) {
			delete(s.rvOnly, key)
			removed++
		}
	}
	kept := s.collections[:0]
	for _, entry := range s.collections {
		if entry.at.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	s.collections = kept
	s.count -= removed
	s.compact()
	return removed
}

// empty reports whether the scope holds nothing, so the index can forget it.
func (s *scopeFacts) empty() bool {
	return s.count == 0
}

// evictOldest removes the oldest live entry, reporting whether it found one. Eviction is
// oldest-first across all four structures of the scope: the eviction order is insertion order, and
// which structure an entry landed in says nothing about how likely it still is to be joined.
func (s *scopeFacts) evictOldest() bool {
	for len(s.order) > 0 {
		ref := s.order[0]
		s.order = s.order[1:]
		if s.remove(ref) {
			s.count--
			return true
		}
	}
	return false
}

// remove deletes the entry a reference names, reporting whether it was still live. A reference
// whose sequence no longer matches is stale — the entry was overwritten by a later fact, which has
// its own reference further along the order — and removing the newer entry for it would evict a
// fact that had only just arrived.
func (s *scopeFacts) remove(ref factRef) bool {
	switch ref.kind {
	case factKindExact:
		key := exactFactKey{uid: ref.uid, rv: ref.rv}
		if entry, ok := s.exact[key]; ok && entry.seq == ref.seq {
			delete(s.exact, key)
			return true
		}
	case factKindLatest:
		if entry, ok := s.latest[ref.uid]; ok && entry.seq == ref.seq {
			delete(s.latest, ref.uid)
			return true
		}
	case factKindRV:
		if entry, ok := s.rvOnly[ref.rv]; ok && entry.seq == ref.seq {
			delete(s.rvOnly, ref.rv)
			return true
		}
	case factKindCollection:
		for i, entry := range s.collections {
			if entry.seq == ref.seq {
				s.collections = append(s.collections[:i], s.collections[i+1:]...)
				return true
			}
		}
	}
	return false
}

// compact drops eviction references whose entries are gone, so the order does not grow with every
// swept entry.
func (s *scopeFacts) compact() {
	kept := s.order[:0]
	for _, ref := range s.order {
		if s.live(ref) {
			kept = append(kept, ref)
		}
	}
	s.order = kept
}

// live reports whether a reference still names the entry it was taken for.
func (s *scopeFacts) live(ref factRef) bool {
	switch ref.kind {
	case factKindExact:
		entry, ok := s.exact[exactFactKey{uid: ref.uid, rv: ref.rv}]
		return ok && entry.seq == ref.seq
	case factKindLatest:
		entry, ok := s.latest[ref.uid]
		return ok && entry.seq == ref.seq
	case factKindRV:
		entry, ok := s.rvOnly[ref.rv]
		return ok && entry.seq == ref.seq
	case factKindCollection:
		for _, entry := range s.collections {
			if entry.seq == ref.seq {
				return true
			}
		}
	}
	return false
}

// covers reports whether a collection fact is in scope for a query at all: same namespace, and not
// yet aged out. The namespace is part of the collection key, so it binds in both passes — uid
// membership included, because the uid set of a collection in another namespace has no business
// naming an author here.
func (c *indexedCollection) covers(q FactQuery, cutoff time.Time) bool {
	return !c.at.Before(cutoff) && c.fact.Namespace == q.Namespace
}

// inWindow reports whether the collection was requested recently enough for a removal to be
// credited to it. The clock that matters is the audit event's own stageTimestamp, i.e. delete
// REQUEST time: under the deletion-as-intent rule the removal being attributed happens when
// deletionTimestamp is set, so finalizers do not stretch this window.
func (c *indexedCollection) inWindow(now time.Time, window time.Duration) bool {
	at := c.stage
	if at.IsZero() {
		at = c.at
	}
	return !now.After(at.Add(window))
}

// selects reports whether the collection's selector accepts an object's labels.
func (c *indexedCollection) selects(objectLabels map[string]string) bool {
	if c.invalidSelector {
		return false
	}
	if c.selector == nil {
		return true
	}
	return c.selector.Matches(labels.Set(objectLabels))
}

// liveFact returns a stored fact when it is present and has not aged out. Expiry is checked on read
// as well as swept, so a fact is never joined past its TTL just because the sweep has not run.
func liveFact(entry *indexedFact, cutoff time.Time) (AuthorFact, bool) {
	if entry == nil || entry.at.Before(cutoff) {
		return AuthorFact{}, false
	}
	return entry.fact, true
}
