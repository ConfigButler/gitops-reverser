// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"slices"
	"strings"
	"sync"
)

// FactStreamSet is the reference-counted union of the (audit route, group/resource) pairs the
// watches running in this process cover. It is what makes the per-type fan-out mean anything: the
// process follows a type while at least one watch needs it and stops following it when the last one
// goes away, so facts for a type nobody watches are written and never received.
//
// Reference counting rather than a plain set is the point. Several WatchRules, and several
// GitTargets, routinely cover one type; the type must stay followed while ANY of them does, and a
// set that had forgotten how many watches added it would unfollow on the first one to stop.
//
// It is deliberately independent of the index and of the transport: the watch side acquires and
// releases, and whoever is following the streams observes the union.
type FactStreamSet struct {
	mu       sync.Mutex
	counts   map[FactStreamKey]int
	observer func([]FactStreamKey)
}

// NewFactStreamSet builds an empty subscription set.
func NewFactStreamSet() *FactStreamSet {
	return &FactStreamSet{counts: map[FactStreamKey]int{}}
}

// Acquire takes one reference on a stream and returns the release for it. The returned release is
// idempotent, so a caller that releases twice — a watch torn down on both an error path and its
// deferred cleanup — cannot unfollow a type another watch still needs.
func (s *FactStreamSet) Acquire(key FactStreamKey) func() {
	s.mu.Lock()
	s.counts[key]++
	changed := s.counts[key] == 1
	s.notifyLocked(changed)
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { s.release(key) })
	}
}

// Keys returns the followed set in a stable order, which is what a follower is given.
func (s *FactStreamSet) Keys() []FactStreamKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keysLocked()
}

// Len reports how many distinct streams are followed.
func (s *FactStreamSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.counts)
}

// Observe installs the callback that receives the followed set whenever it changes, and hands it
// the current set immediately so the follower and the set never start out disagreeing. A nil
// callback detaches. Only one observer is supported: one process follows one subscription.
func (s *FactStreamSet) Observe(observe func([]FactStreamKey)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = observe
	s.notifyLocked(observe != nil)
}

// release drops one reference, unfollowing the stream when it was the last.
func (s *FactStreamSet) release(key FactStreamKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count, ok := s.counts[key]
	if !ok {
		return
	}
	count--
	if count > 0 {
		s.counts[key] = count
		s.notifyLocked(false)
		return
	}
	delete(s.counts, key)
	s.notifyLocked(true)
}

// notifyLocked hands the observer the current set when the set itself changed. It runs under the
// lock so two concurrent changes cannot deliver out of order, which would leave the follower
// following a set that no longer exists.
func (s *FactStreamSet) notifyLocked(changed bool) {
	if !changed || s.observer == nil {
		return
	}
	s.observer(s.keysLocked())
}

// keysLocked renders the followed set in the order a follower reads it.
func (s *FactStreamSet) keysLocked() []FactStreamKey {
	keys := make([]FactStreamKey, 0, len(s.counts))
	for key := range s.counts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b FactStreamKey) int {
		if c := strings.Compare(a.AuditRoute, b.AuditRoute); c != 0 {
			return c
		}
		return strings.Compare(a.groupResource(), b.groupResource())
	})
	return keys
}
