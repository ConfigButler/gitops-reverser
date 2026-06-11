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

// The Materializer is the second axis from
// docs/design/stream/demand-driven-type-materialization-lifecycle.md: a sibling state
// machine to the Registry, in the same leaf package. The Registry answers the supply
// question ("can we follow this type?"); the Materializer answers the demand-met
// question ("has anyone claimed it, and have we listed it?"). A type is materialized —
// holds a checkpoint — iff it is Followable AND Claimed. Nothing outside that
// intersection ever holds one.
//
// It stays a leaf exactly like the Registry: it depends only on apimachinery schema and
// an injected clock. Followability enters as consumed LifecycleEvents (DEC-L4), not as a
// client; the async fill that actually builds a checkpoint is a future driver (L-2/L-3)
// that calls BeginSync / SyncSucceeded / SyncFailed — this file owns only the phase
// machine and the lease GC, not the fill itself.
//
// "The sync" in this file means that fill, and the modern path is a streaming-list WATCH,
// NOT a plain LIST: the driver (internal/watch's StreamSnapshotForType /
// StreamClusterSnapshotForGitDest) opens a WATCH with sendInitialEvents=true,
// resourceVersionMatch=NotOlderThan, allowWatchBookmarks=true, folds the initial ADDED
// events, and reads to the initial-events-end bookmark — that bookmark's resourceVersion
// is the rv handed to SyncSucceeded. A consistent LIST is the per-type FALLBACK only, for
// a server that cannot stream (e.g. an aggregated apiserver that rejects
// sendInitialEvents). See docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md.

// Phase is where a type sits on the materialization axis. It is orthogonal to the
// followability Verdict: a Followable type may be Dormant (unclaimed) and a claimed type
// may be Dormant (not yet followable). See the §3 table of the design doc.
type Phase string

const (
	// PhaseDormant is the resting state: no live claim, or not yet followable. No
	// checkpoint, not reconcile-serviceable.
	PhaseDormant Phase = "Dormant"
	// PhaseRequested has ≥1 claim and is followable, queued for a first sync that has not
	// started. No checkpoint yet.
	PhaseRequested Phase = "Requested"
	// PhaseSyncing has its first checkpoint sync (a streaming-list watch) in flight. Still
	// nothing to serve, so consumers hold (L4).
	PhaseSyncing Phase = "Syncing"
	// PhaseSynced has a checkpoint at rv R and is reconcile-serviceable.
	PhaseSynced Phase = "Synced"
	// PhaseResyncing has a periodic re-anchor sync in flight; the PRIOR checkpoint is
	// still served until the new one swaps in (L5).
	PhaseResyncing Phase = "Resyncing"
	// PhaseFailing had its last sync error and is awaiting a backoff retry. A prior
	// checkpoint (if any) keeps serving (L5/L6).
	PhaseFailing Phase = "Failing"
)

// GitTargetRef identifies the GitTarget that asserts demand. The Materializer treats it
// as an opaque key — whether the caller passes a UID, a namespaced name, or a resolved
// internal id is its choice (the L-1 open question); the leaf never interprets it.
type GitTargetRef string

// Materializer owns the claim table keyed (GitTargetRef, GVR) and the per-type
// materialization phase. It is safe for concurrent readers and callers: mu guards the
// state, dispatchMu serializes whole operations and their event dispatch so observers
// see transitions in order and never interleaved — the same discipline the Registry
// uses.
type Materializer struct {
	// dispatchMu serializes a whole mutating operation and the dispatch that follows it,
	// so concurrent callers cannot interleave event batches. It is taken before mu;
	// observers run after mu is released so they may read the Materializer.
	dispatchMu sync.Mutex
	mu         sync.RWMutex
	now        func() time.Time

	// claims is the demand table: per type, the set of GitTargets claiming it and the
	// time each last renewed. A claim is a self-renewing lease (DEC-L3) — Declare renews,
	// the sweep GCs whatever was not renewed since the previous sweep.
	claims map[schema.GroupVersionResource]map[GitTargetRef]time.Time
	// types is the per-type phase plus the followability gate state derived from consumed
	// LifecycleEvents. An entry exists for every type that is claimed or has ever been
	// followable; it is bounded by the catalog (~hundreds), never by demand history.
	types map[schema.GroupVersionResource]*typeState

	observers []MaterializationObserver
	// lastSweepAt is when the previous sweep ran. A claim is "live" at a sweep iff it was
	// renewed at or after this instant — so the release grace IS the sweep interval, with
	// no dedicated constant (DEC-L5).
	lastSweepAt time.Time
}

// typeState is one type's materialization phase plus the followability gate the consumed
// LifecycleEvents drive. followable and frozen are the gate (DEC-L4): only a followable,
// unfrozen type ever drives or refreshes a sync.
type typeState struct {
	phase Phase
	// followable mirrors the registry verdict as seen through lifecycle events: true after
	// TypeActivated/TypeRecovered, false after TypeRemoved/TypeRefused. It is persisted so
	// a claim arriving after activation still knows the type is syncable (DEC-L9).
	followable bool
	// frozen is set by TypeWobbling and cleared by TypeRecovered: a wobble suspends sync
	// and sweep but keeps the existing checkpoint served (DEC-L4).
	frozen bool
	// pendingResync is set by the sweep when a still-claimed Synced type is due a periodic
	// re-anchor; BeginSync consumes it to move Synced -> Resyncing.
	pendingResync bool
	// checkpointRV is the revision of the currently-served checkpoint, empty when none
	// exists. It survives a Resyncing/Failing transition so the prior checkpoint keeps
	// serving (L5); it is cleared only on release.
	checkpointRV string
}

// NewMaterializer builds an empty Materializer with a real clock.
func NewMaterializer() *Materializer { return newMaterializer(time.Now) }

// newMaterializer is the test seam: it injects the clock so the lease GC is deterministic,
// mirroring newRegistry. lastSweepAt starts at construction time, so the first sweep
// treats every claim made since construction as live.
func newMaterializer(now func() time.Time) *Materializer {
	return &Materializer{
		now:         now,
		claims:      map[schema.GroupVersionResource]map[GitTargetRef]time.Time{},
		types:       map[schema.GroupVersionResource]*typeState{},
		lastSweepAt: now(),
	}
}

// Declare records a GitTarget's entire desired type-set as a self-renewing lease
// (DEC-L3). It is claim + renew + implicit withdrawal in one idempotent call: every GVR
// in desired is (re)claimed at now, and any GVR the GitTarget previously declared but
// omits this time is simply left un-renewed and ages out at the next sweep. Re-sending
// the same set is a no-op beyond renewal, so it is safe to call every reconcile.
//
// A claim is recorded regardless of followability (DEC-L9): claiming a refused or
// not-yet-discovered type is allowed and drives a sync the moment it becomes followable.
func (m *Materializer) Declare(ref GitTargetRef, desired []schema.GroupVersionResource) {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	for _, gvr := range desired {
		inner := m.claims[gvr]
		if inner == nil {
			inner = map[GitTargetRef]time.Time{}
			m.claims[gvr] = inner
		}
		inner[ref] = now
		st := m.stateLocked(gvr)
		events = m.maybeRequestLocked(gvr, st, now, events)
	}
	sortMaterializationEvents(events)
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
}

// OnLifecycleEvent is the followability gate (DEC-L4): it is an Observer the future
// driver wires onto Registry.Subscribe, so the Materializer consumes the same lifecycle
// vocabulary instead of inventing its own. It never re-derives followability — it only
// translates a transition into its effect on the materialization axis.
func (m *Materializer) OnLifecycleEvent(ev LifecycleEvent) {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	switch ev.Kind {
	case TypeActivated, TypeRecovered:
		// Activated (or recovered) + claimed -> drive a sync; unclaimed -> stay Dormant.
		st := m.stateLocked(ev.GVR)
		st.followable = true
		st.frozen = false
		events = m.maybeRequestLocked(ev.GVR, st, now, events)
		// A type that is already Synced when (re)activated — most notably a checkpoint restored
		// from durable state on boot (DEC-L6), whose original TypeSynced was consumed before any
		// consumer existed, or a wobbled type recovering — re-announces TypeSynced so the driver
		// re-establishes its per-type reconcile and its audit-replay tail. Both consumer actions are
		// idempotent, so re-announcing is safe; without it, a restored checkpoint would serve but
		// never wake a consumer (no replay), which is the post-restart "mirror goes silent" bug.
		//
		// Gate on a live CLAIM: an unclaimed restored checkpoint (a type a prior, now-deleted
		// GitTarget materialized — a boot restores every durable checkpoint, dozens of them) has no
		// consumer to wake, and waking one would start a per-type audit tail — a parked blocking Redis
		// read — for a type nobody follows. Dozens of those exhaust the shared connection pool and
		// starve the mirror's writes (the per-type streams stop filling). A claim that lands AFTER
		// activation re-announces via the Declare path (the watch layer starts the tail for a
		// newly-claimed already-Synced type), so both boot orderings still converge.
		if st.phase == PhaseSynced && len(m.claims[ev.GVR]) > 0 {
			events = append(events, m.event(TypeSynced, ev.GVR, st, now))
		}
	case TypeWobbling:
		// Freeze: suspend re-anchor and sweep, keep the existing checkpoint served.
		if st, ok := m.types[ev.GVR]; ok {
			st.frozen = true
		}
	case TypeRemoved, TypeRefused:
		// Force-release the checkpoint; the claim survives so a reappearance re-syncs.
		if st, ok := m.types[ev.GVR]; ok {
			st.followable = false
			events = m.forceReleaseLocked(ev.GVR, st, now, events)
		}
	}
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
}

// BeginSync advances a type into a sync the driver is starting (T2/T4): Requested ->
// Syncing for a first sync, Synced (with a pending re-anchor) -> Resyncing, or a Failing
// retry into whichever of the two its checkpoint state implies. It reports whether a
// sync actually started, so the driver only opens its streaming-list watch when the phase
// agreed. A frozen (wobbling) type never starts a sync — a fill against an unserved type
// is untrustworthy (DEC-L4).
func (m *Materializer) BeginSync(gvr schema.GroupVersionResource) bool {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	started := false
	if st, ok := m.types[gvr]; ok && !st.frozen {
		switch st.phase {
		case PhaseRequested:
			st.phase = PhaseSyncing
			started = true
		case PhaseSynced:
			if st.pendingResync {
				st.pendingResync = false
				st.phase = PhaseResyncing
				started = true
			}
		case PhaseFailing:
			// Retry: re-anchor if a prior checkpoint is still served, else a first sync.
			if st.checkpointRV != "" {
				st.phase = PhaseResyncing
			} else {
				st.phase = PhaseSyncing
			}
			started = true
		case PhaseDormant, PhaseSyncing, PhaseResyncing:
			// Nothing to (re)start: not yet claimed-and-followable, or a sync is in flight.
		}
		if started {
			events = append(events, m.event(SyncStarted, gvr, st, now))
		}
	}
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
	return started
}

// SyncSucceeded lands a checkpoint at rv: Syncing/Resyncing -> Synced. rv is the sync's
// pinned revision — the initial-events-end bookmark resourceVersion of the streaming-list
// watch (or the LIST revision on the fallback path). On a re-anchor it swaps the served
// revision to rv (L5). It is a no-op unless a sync was in flight.
func (m *Materializer) SyncSucceeded(gvr schema.GroupVersionResource, rv string) {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	if st, ok := m.types[gvr]; ok && (st.phase == PhaseSyncing || st.phase == PhaseResyncing) {
		st.phase = PhaseSynced
		st.checkpointRV = rv
		st.pendingResync = false
		events = append(events, m.event(TypeSynced, gvr, st, now))
	}
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
}

// SyncFailed records a sync error: Syncing/Resyncing -> Failing. The checkpointRV is left
// untouched, so a first-sync failure serves nothing (consumers hold) while a re-anchor
// failure keeps serving the prior checkpoint (L5). The type re-surfaces in PendingSyncs
// for the driver to retry after its backoff. It is a no-op unless a sync was in flight.
func (m *Materializer) SyncFailed(gvr schema.GroupVersionResource) {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	if st, ok := m.types[gvr]; ok && (st.phase == PhaseSyncing || st.phase == PhaseResyncing) {
		st.phase = PhaseFailing
		events = append(events, m.event(SyncFailed, gvr, st, now))
	}
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
}

// RestoreSynced rebuilds a type's materialization phase from the durable checkpoint state on
// boot (DEC-L6): it marks the type Synced at rv WITHOUT a fill, so a restart resumes serving a
// standing checkpoint instead of re-listing the world. It is the in-memory half of the HA seam
// — the authoritative phase/rv lives in Redis (:objects:state); the watch layer reads it and
// replays it here, keeping this leaf free of any client. followable is set true so a later
// periodic re-anchor can run and a subsequent TypeActivated for an already-Synced type is a
// no-op. It emits no event (a silent boot restore is not a transition a driver should act on)
// and is a no-op for an empty rv. The caller must invoke it at boot, before the first
// followability Update and before the sweep/driver start, so it never races a live transition.
func (m *Materializer) RestoreSynced(gvr schema.GroupVersionResource, rv string) {
	if rv == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(gvr)
	st.phase = PhaseSynced
	st.checkpointRV = rv
	st.followable = true
	st.frozen = false
	st.pendingResync = false
}

// RequestResync flags a claimed, Synced, unfrozen type for an immediate re-anchor and
// emits SyncRequested — the same transition the periodic sweep applies, available on
// demand. The ingestion layer uses it as the late-event nudge: an audit event whose RV
// arrived below its type stream's high-water is diverted to the diagnostic late lane
// and never replayed, so only a fresh checkpoint folds its effect in; without the nudge
// the next periodic sweep (~1h) is the backstop and the mirror serves stale state until
// then. It reports whether a resync was actually requested; any other phase is a no-op
// (a sync already in flight or pending re-anchor will land at a revision at or above
// the late event's, which already covers it).
func (m *Materializer) RequestResync(gvr schema.GroupVersionResource) bool {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent
	requested := false
	if st, ok := m.types[gvr]; ok && !st.frozen &&
		st.phase == PhaseSynced && !st.pendingResync && len(m.claims[gvr]) > 0 {
		st.pendingResync = true
		requested = true
		events = append(events, m.event(SyncRequested, gvr, st, now))
	}
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
	return requested
}

// Sweep is the one periodic pass that does both jobs (DEC-L5): it first GCs leases that
// were not renewed since the previous sweep, then for each type branches on whether a
// live claim remains — re-anchor the still-wanted, release the no-longer-wanted. The
// caller drives the cadence (the ~1h interval), so the release grace is exactly one
// interval with no dedicated constant. A frozen (wobbling) type is swept over: nothing is
// re-synced or released against an unserved type.
func (m *Materializer) Sweep() {
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()

	m.mu.Lock()
	now := m.now()
	var events []MaterializationEvent

	// Lease GC first, so a non-empty claim set for a GVR afterwards means a live claim.
	m.gcLeasesLocked(m.lastSweepAt)
	for gvr, st := range m.types {
		events = m.sweepTypeLocked(gvr, st, now, events)
	}
	m.lastSweepAt = now
	sortMaterializationEvents(events)
	observers := m.observers
	m.mu.Unlock()

	dispatchMaterialization(observers, events)
}

// gcLeasesLocked drops every claim not renewed since the previous sweep — that is
// withdrawn demand (DEC-L3). A GVR whose last claimant ages out is removed entirely.
// Caller holds m.mu.
func (m *Materializer) gcLeasesLocked(previousSweepAt time.Time) {
	for gvr, refs := range m.claims {
		for ref, renewedAt := range refs {
			if renewedAt.Before(previousSweepAt) {
				delete(refs, ref)
			}
		}
		if len(refs) == 0 {
			delete(m.claims, gvr)
		}
	}
}

// sweepTypeLocked runs the per-type live-claim branch of one sweep pass: re-anchor a
// still-claimed Synced type, release an unclaimed one (or a pending first sync whose claim
// was withdrawn). It runs after gcLeasesLocked, so a non-empty claim set means live demand.
// A frozen (wobbling) type is left untouched (DEC-L4). Caller holds m.mu.
func (m *Materializer) sweepTypeLocked(
	gvr schema.GroupVersionResource,
	st *typeState,
	now time.Time,
	events []MaterializationEvent,
) []MaterializationEvent {
	if st.frozen {
		return events
	}
	live := len(m.claims[gvr]) > 0
	switch st.phase {
	case PhaseSynced:
		if !live {
			return append(events, m.releaseLocked(gvr, st, now))
		}
		if !st.pendingResync {
			st.pendingResync = true
			events = append(events, m.event(SyncRequested, gvr, st, now))
		}
	case PhaseRequested, PhaseFailing:
		// Claim withdrawn before (or after a failed) sync: drop the pending work.
		if !live {
			return append(events, m.releaseLocked(gvr, st, now))
		}
	case PhaseDormant, PhaseSyncing, PhaseResyncing:
		// Dormant: nothing materialized. Syncing/Resyncing: a sync is in flight; let it
		// complete and be released, if still unwanted, at a later sweep.
	}
	return events
}

// stateLocked returns the type's state, creating a Dormant entry on first reference.
// Caller holds m.mu.
func (m *Materializer) stateLocked(gvr schema.GroupVersionResource) *typeState {
	st := m.types[gvr]
	if st == nil {
		st = &typeState{phase: PhaseDormant}
		m.types[gvr] = st
	}
	return st
}

// maybeRequestLocked moves a followable, unfrozen, claimed Dormant type to Requested and
// returns the SyncRequested event (T1 / DEC-L9). It is the single choke point through
// which both a fresh claim (Declare) and a fresh activation (OnLifecycleEvent) drive the
// first sync, so the two orderings converge. Caller holds m.mu.
func (m *Materializer) maybeRequestLocked(
	gvr schema.GroupVersionResource,
	st *typeState,
	now time.Time,
	events []MaterializationEvent,
) []MaterializationEvent {
	if st.followable && !st.frozen && st.phase == PhaseDormant && len(m.claims[gvr]) > 0 {
		st.phase = PhaseRequested
		events = append(events, m.event(SyncRequested, gvr, st, now))
	}
	return events
}

// forceReleaseLocked drops the checkpoint when followability is lost (TypeRemoved /
// TypeRefused). Unlike the sweep release it does NOT touch the claim — a reappearance
// re-syncs from the standing claim (DEC-L4). It is a no-op (no event) for an already
// Dormant type. Caller holds m.mu.
func (m *Materializer) forceReleaseLocked(
	gvr schema.GroupVersionResource,
	st *typeState,
	now time.Time,
	events []MaterializationEvent,
) []MaterializationEvent {
	if st.phase == PhaseDormant {
		return events
	}
	return append(events, m.releaseLocked(gvr, st, now))
}

// releaseLocked resets a type to Dormant, dropping its checkpoint, and returns the
// Released event. It does not touch the claim table; the caller decides whether the claim
// survives (force-release) or was already GC'd (sweep). Caller holds m.mu.
func (m *Materializer) releaseLocked(
	gvr schema.GroupVersionResource,
	st *typeState,
	now time.Time,
) MaterializationEvent {
	st.phase = PhaseDormant
	st.checkpointRV = ""
	st.pendingResync = false
	return m.event(Released, gvr, st, now)
}

// event builds a MaterializationEvent from a type's current state.
func (m *Materializer) event(
	kind MaterializationEventKind,
	gvr schema.GroupVersionResource,
	st *typeState,
	now time.Time,
) MaterializationEvent {
	return MaterializationEvent{Kind: kind, GVR: gvr, Phase: st.phase, RV: st.checkpointRV, At: now}
}

// Phase reports a type's current materialization phase. The bool is false for a type the
// Materializer has never seen a claim or lifecycle event for (implicitly Dormant).
func (m *Materializer) Phase(gvr schema.GroupVersionResource) (Phase, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.types[gvr]
	if !ok {
		return PhaseDormant, false
	}
	return st.phase, true
}

// Checkpoint reports the revision of the checkpoint a type currently serves and whether
// one exists. A Synced type serves its current rv; a Resyncing or Failing type still
// serves the prior rv (L5); every other phase serves nothing.
func (m *Materializer) Checkpoint(gvr schema.GroupVersionResource) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.types[gvr]
	if !ok {
		return "", false
	}
	return st.checkpointRV, st.checkpointRV != ""
}

// PendingSyncs returns, sorted, the types that need the driver to (re)start a sync: a
// Requested first sync, a Failing retry, or a Synced type the sweep flagged for a
// re-anchor. A frozen (wobbling) type is never pending. This is the driver's "what needs
// a (re)sync?" query (L4), the pull complement to the SyncRequested push.
func (m *Materializer) PendingSyncs() []schema.GroupVersionResource {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []schema.GroupVersionResource
	for gvr, st := range m.types {
		if st.frozen {
			continue
		}
		switch st.phase {
		case PhaseRequested, PhaseFailing:
			out = append(out, gvr)
		case PhaseSynced:
			if st.pendingResync {
				out = append(out, gvr)
			}
		case PhaseDormant, PhaseSyncing, PhaseResyncing:
			// Not awaiting the driver: resting, or a sync is already in flight.
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Claimants returns, sorted, the GitTargets that currently hold a claim on a type —
// including stale (un-renewed) claims not yet GC'd by a sweep. It is the per-type demand
// view (L10) and the seam the L-6 visibility step builds on.
func (m *Materializer) Claimants(gvr schema.GroupVersionResource) []GitTargetRef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	refs := m.claims[gvr]
	out := make([]GitTargetRef, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TypeMaterialization is one type's materialization status for the visibility surface (L10):
// its phase, the checkpoint revision it serves (empty when none), whether it is currently
// followable, and the GitTargets claiming it (stale claims included, so a claim on a
// non-followable type is visible as the claim-vs-refused mismatch).
type TypeMaterialization struct {
	GVR          schema.GroupVersionResource
	Phase        Phase
	CheckpointRV string
	Followable   bool
	Claimants    []GitTargetRef
}

// Inventory returns, sorted by GVR, the materialization status of every type the Materializer
// tracks (claimed or ever-followable). It is the per-type visibility query (L10) the watch
// layer turns into metrics and a bounded per-GitTarget status roll-up. It is bounded by the
// catalog (~hundreds), never by demand history.
func (m *Materializer) Inventory() []TypeMaterialization {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TypeMaterialization, 0, len(m.types))
	for gvr, st := range m.types {
		refs := m.claims[gvr]
		claimants := make([]GitTargetRef, 0, len(refs))
		for ref := range refs {
			claimants = append(claimants, ref)
		}
		sort.Slice(claimants, func(i, j int) bool { return claimants[i] < claimants[j] })
		out = append(out, TypeMaterialization{
			GVR:          gvr,
			Phase:        st.phase,
			CheckpointRV: st.checkpointRV,
			Followable:   st.followable,
			Claimants:    claimants,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GVR.String() < out[j].GVR.String() })
	return out
}
