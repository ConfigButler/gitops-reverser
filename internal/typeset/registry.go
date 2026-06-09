/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package typeset

import (
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RemovalGrace is how long a previously-live type that stops being observed is held
// as retained before it leaves the live set. It is product safety, not tuning, so it
// is a fixed constant: it stops a short discovery blink from turning into a large Git
// sweep. See the "Live set and the 60-second grace" section of the design.
const RemovalGrace = 60 * time.Second

// Registry is the single decision surface: it turns observations plus the live-set
// grace into one TypeRecord per known type, and answers the lookups every consumer
// reads. It owns identity, the live set, and the removal grace; consumers never
// recompute followability, they read it.
//
// Additions are fast and removals are slow: a newly observed followable type enters
// the live set immediately, while a previously-live type that stops being observed is
// held as retained for RemovalGrace before it drops. The clock is injectable so the
// grace is deterministic in tests.
//
// Registry is safe for concurrent readers and a single updater.
type Registry struct {
	// dispatchMu serializes whole Updates and the lifecycle dispatch that follows each, so
	// event batches are delivered to observers in generation order and never interleave. It
	// is taken before mu; observers run after mu is released (so they may read the registry).
	dispatchMu sync.Mutex
	mu         sync.RWMutex
	grace      time.Duration
	settle     time.Duration
	now        func() time.Time

	entries    map[recordKey]entry
	byGVK      map[schema.GroupVersionKind][]recordKey
	byGVR      map[schema.GroupVersionResource]recordKey
	observers  []Observer
	generation uint64
	// revision is the registry's own change-of-decision signal: it bumps whenever the
	// followable membership changes (a type appears, drops after the grace, or flips
	// followable<->refused) or the backing scan generation moves. Consumers that cache
	// a projection of the registry (the per-GitTarget watched-type set) gate on this,
	// not on the catalog generation — so a retention-grace drop at a stable generation
	// still invalidates their cache. See docs/.../discovery-catalog-typeset-boundary.md.
	revision uint64
	ready    bool
}

// recordKey is a record's stable identity for the live set: the (GVK, GVR, scope)
// triple. Two resources serving the same kind are distinct records under the same
// GVK index entry, which is exactly the gvk-not-unique case.
type recordKey struct {
	gvk   schema.GroupVersionKind
	gvr   schema.GroupVersionResource
	scope Scope
}

// entry is one record plus the facts, grace, and settle bookkeeping needed to re-judge it
// when it stops being observed and to debounce its activation.
type entry struct {
	obs         Observation
	record      TypeRecord
	absentSince time.Time // zero while currently observed
	// followableSince marks the start of the current continuous Followable streak (zero when
	// not Followable); activated records whether TypeActivated has already been emitted for
	// that streak. Together they implement the settle window and its flap coalescing.
	followableSince time.Time
	activated       bool
}

// NewRegistry builds an empty registry with the fixed removal grace and a real clock.
func NewRegistry() *Registry {
	return newRegistry(time.Now)
}

// newRegistry is the test seam: it injects the clock so the grace is deterministic.
// The grace itself is always the fixed RemovalGrace — it is product safety, not
// tuning.
func newRegistry(now func() time.Time) *Registry {
	return &Registry{
		grace:   RemovalGrace,
		settle:  SettleWindow,
		now:     now,
		entries: map[recordKey]entry{},
		byGVK:   map[schema.GroupVersionKind][]recordKey{},
		byGVR:   map[schema.GroupVersionResource]recordKey{},
	}
}

// Update replaces the observation set for a new catalog generation and applies the
// live-set grace. Every observation becomes a record at this generation; a
// previously-live type missing from the set is re-judged as retained (within the
// grace) or dropped (once the grace elapses). The first Update marks the registry
// ready.
func (r *Registry) Update(observations []Observation, generation uint64) {
	// dispatchMu serializes the whole Update plus its post-publish dispatch, so concurrent
	// updaters cannot interleave event batches and observers see transitions in generation
	// order. The records themselves are still guarded by mu for concurrent readers.
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()

	r.mu.Lock()
	now := r.now()
	prevFollowable := r.followableKeysLocked()
	prevGeneration := r.generation
	wasReady := r.ready

	next := make(map[recordKey]entry, len(observations))
	for _, obs := range observations {
		key := observationKey(obs)
		rec := recordFromObservation(obs, generation)
		next[key] = entry{obs: obs, record: rec}
	}

	r.retainAbsentLocked(next, now, generation)

	// Compute the lifecycle transitions while r.entries still holds the previous records, and
	// finalize each next entry's settle bookkeeping, before publishing next.
	events := r.computeLifecycleLocked(next, now, generation)

	r.entries = next
	r.rebuildIndexesLocked()
	r.generation = generation
	r.ready = true

	// Bump the change-of-decision signal when the followable set changes (covers the
	// time-based grace drop at a stable generation) or the scan generation moves.
	if !wasReady || generation != prevGeneration || !sameKeySet(prevFollowable, r.followableKeysLocked()) {
		r.revision++
	}
	observers := r.observers
	r.mu.Unlock()

	// Dispatch outside mu (so an observer may read the registry) but still under dispatchMu
	// (so batches stay ordered). Observers must not block — a real consumer enqueues and returns.
	dispatchLifecycle(observers, events)
}

// followableKeysLocked returns the identity keys of the records that are currently
// followable. Caller holds r.mu.
func (r *Registry) followableKeysLocked() map[recordKey]struct{} {
	out := make(map[recordKey]struct{}, len(r.entries))
	for key, e := range r.entries {
		if e.record.Followable() {
			out[key] = struct{}{}
		}
	}
	return out
}

func sameKeySet(a, b map[recordKey]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}

// retainAbsentLocked folds previously-live types missing from the next set back in as
// retained records, until the removal grace elapses. A type that was already refused
// (never live) is not retained — it simply drops with its observation.
func (r *Registry) retainAbsentLocked(next map[recordKey]entry, now time.Time, generation uint64) {
	for key, prev := range r.entries {
		if _, present := next[key]; present {
			continue // freshly observed; the new record wins
		}
		if !prev.record.Followable() {
			continue // was not live, nothing to hold
		}
		absentSince := prev.absentSince
		if absentSince.IsZero() {
			absentSince = now
		}
		expired := now.Sub(absentSince) >= r.grace
		obs := prev.obs
		obs.Served = false
		obs.AbsenceExpired = expired
		rec := recordFromObservation(obs, generation)
		if !rec.Followable() {
			continue // grace elapsed; the absence is trusted, let it drop
		}
		next[key] = entry{obs: obs, record: rec, absentSince: absentSince}
	}
}

func (r *Registry) rebuildIndexesLocked() {
	r.byGVK = make(map[schema.GroupVersionKind][]recordKey, len(r.entries))
	r.byGVR = make(map[schema.GroupVersionResource]recordKey, len(r.entries))
	for key := range r.entries {
		r.byGVK[key.gvk] = append(r.byGVK[key.gvk], key)
		r.byGVR[key.gvr] = key
	}
	for gvk := range r.byGVK {
		sort.Slice(r.byGVK[gvk], func(i, j int) bool {
			return r.byGVK[gvk][i].gvr.String() < r.byGVK[gvk][j].gvr.String()
		})
	}
}

// Ready reports whether the registry has accepted any observation set.
func (r *Registry) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

// Generation reports the catalog generation the current records were resolved at.
func (r *Registry) Generation() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// Revision reports the registry's change-of-decision counter. It bumps whenever the
// followable membership changes or the scan generation moves, so a consumer that caches
// a projection of the registry can gate its rebuild on this value and still react to a
// retention-grace drop that happens without any discovery change.
func (r *Registry) Revision() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

// ByGVK returns the record for a kind. The bool reports whether the kind is known to
// the registry at all; callers gate behaviour on record.Followable(). When a kind is
// served by more than one resource every such record is refused with gvk-not-unique,
// and the deterministic first (by GVR) is returned.
func (r *Registry) ByGVK(gvk schema.GroupVersionKind) (TypeRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := r.byGVK[gvk]
	if len(keys) == 0 {
		return TypeRecord{}, false
	}
	return r.entries[keys[0]].record, true
}

// ByGVR returns the record for a resource. The bool reports whether the resource is
// known to the registry at all.
func (r *Registry) ByGVR(gvr schema.GroupVersionResource) (TypeRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.byGVR[gvr]
	if !ok {
		return TypeRecord{}, false
	}
	return r.entries[key].record, true
}

// Followable returns every live record (verdict followable or retained), sorted by
// identity. It is the inventory the informer set and snapshot scope derive from.
func (r *Registry) Followable() []TypeRecord {
	return r.records(func(rec TypeRecord) bool { return rec.Followable() })
}

// All returns every known record — followable, retained, and refused — for inventory
// and "why not" views.
func (r *Registry) All() []TypeRecord {
	return r.records(func(TypeRecord) bool { return true })
}

func (r *Registry) records(keep func(TypeRecord) bool) []TypeRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TypeRecord, 0, len(r.entries))
	for _, e := range r.entries {
		if keep(e.record) {
			out = append(out, e.record)
		}
	}
	sortRecords(out)
	return out
}

func sortRecords(records []TypeRecord) {
	sort.Slice(records, func(i, j int) bool {
		return identitySortKey(records[i].Identity) < identitySortKey(records[j].Identity)
	})
}

func identitySortKey(id Identity) string {
	return id.GVK.Group + "|" + id.GVK.Version + "|" + id.GVK.Kind + "|" + id.GVR.Resource
}

func observationKey(obs Observation) recordKey {
	return recordKey{gvk: obs.Identity.GVK, gvr: obs.Identity.GVR, scope: obs.Identity.Scope}
}

// recordFromObservation evaluates an observation into a full record at a generation.
func recordFromObservation(obs Observation, generation uint64) TypeRecord {
	return TypeRecord{
		Identity:      obs.Identity,
		Origin:        obs.Origin,
		Preferred:     obs.Preferred,
		Verbs:         obs.Verbs,
		Subresources:  obs.Subresources,
		Sensitive:     obs.Sensitive,
		Followability: Evaluate(obs),
		Generation:    generation,
	}
}
