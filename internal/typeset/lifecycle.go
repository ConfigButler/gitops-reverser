// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SettleWindow is how long a type must stay continuously followable before the registry
// emits a TypeActivated for it. It governs ACTIVATION, not removal: a flapping or
// just-appeared type does not drive a per-type reconcile on a state that is about to change
// again. Like RemovalGrace it is product safety, not tuning, so it is a fixed constant — but
// it is deliberately short where the grace is long. See
// docs/spec/type-lifecycle-events-and-wobble-settling.md (Proposal 2).
const SettleWindow = 5 * time.Second

// EventKind names a per-type lifecycle transition the registry emits. The events are
// transitions between the existing verdicts (no new verdict vocabulary): the registry is the
// single component that owns the decision, so it computes the transition once and names it,
// instead of every consumer re-detecting the same edge by diffing tables.
type EventKind string

const (
	// TypeActivated fires when a type has been continuously Followable for the settle
	// window: it is healthy and stable, so M12 may schedule its (re)reconcile.
	TypeActivated EventKind = "TypeActivated"
	// TypeWobbling fires on Followable -> Retained: a transient unserved blip. Do NOT sweep;
	// postpone the type's reconcile and keep its informers up until it settles or drops.
	TypeWobbling EventKind = "TypeWobbling"
	// TypeRecovered fires on Retained -> Followable: the wobble resolved. It collapses into
	// a fresh TypeActivated once the settle window elapses again.
	TypeRecovered EventKind = "TypeRecovered"
	// TypeRemoved fires when a previously-live type leaves the live set because its removal
	// grace elapsed (absence-expired): it is genuinely gone, so M12 sweeps THIS type only.
	TypeRemoved EventKind = "TypeRemoved"
	// TypeRefused fires when a previously-live type fails a permanent check: never watch it,
	// drop its informers, surface it in status.
	TypeRefused EventKind = "TypeRefused"
)

// LifecycleEvent is one named transition between verdicts for a single type. It carries the
// identity, the verdicts it crossed, the single machine-readable reason for a failure, the
// scan generation the transition was computed at, and the time it was observed.
type LifecycleEvent struct {
	Kind       EventKind
	GVK        schema.GroupVersionKind
	GVR        schema.GroupVersionResource
	From       Verdict
	To         Verdict
	Reason     Reason
	Generation uint64
	At         time.Time
}

// Observer receives lifecycle events from the registry. It is invoked by Update after the new
// records are published, in generation order and serialized with other updates, so an
// observer may read the registry but must NOT block the updater (a real consumer enqueues the
// event and returns). See Registry.Subscribe.
type Observer func(LifecycleEvent)

// Subscribe registers an observer for every lifecycle event from subsequent Updates. Register
// before the first Update to observe cold-start activations. Observers are invoked outside the
// registry's read/write lock (so they may read the registry) but under the updater's
// serialization, so a slow observer stalls the updater — keep them non-blocking.
func (r *Registry) Subscribe(obs Observer) {
	if obs == nil {
		return
	}
	r.mu.Lock()
	r.observers = append(r.observers, obs)
	r.mu.Unlock()
}

// computeLifecycleLocked sets the settle bookkeeping on every next entry by comparing it with
// its prior verdict, and returns the lifecycle transitions this Update crosses. It runs while
// r.entries still holds the PREVIOUS records, before next is published, so the diff is against
// what each consumer last saw. The caller holds r.mu.
//
// Cold start and always-refused types are silent: an event is emitted only for a type that was
// live (or is entering the live set), never a spurious TypeRemoved/TypeRefused for a type the
// registry never followed. TypeActivated is gated on the settle window, so a fresh or flapping
// type does not activate until it has been stably Followable.
func (r *Registry) computeLifecycleLocked(next map[recordKey]entry, now time.Time, generation uint64) []LifecycleEvent {
	var events []LifecycleEvent
	for key, e := range next {
		prev, had := r.entries[key]
		newVerdict := e.record.Followability.Verdict

		// Settle bookkeeping: a continuous Followable streak carries its start time and its
		// activated flag; any other verdict (or a gap) resets the streak so the window restarts.
		if newVerdict == VerdictFollowable {
			if had && prev.record.Followability.Verdict == VerdictFollowable {
				e.followableSince = prev.followableSince
				e.activated = prev.activated
			} else {
				e.followableSince = now
				e.activated = false
			}
		}

		events = appendTransitionEvent(events, had, prev, e, now, generation)

		// Activation: emitted once per Followable streak, only after the settle window, so the
		// first per-type reconcile only ever runs against a type that has been stably healthy.
		if newVerdict == VerdictFollowable && !e.activated &&
			!e.followableSince.IsZero() && now.Sub(e.followableSince) >= r.settle {
			e.activated = true
			events = append(events,
				lifecycleEvent(TypeActivated, e.record, fromVerdict(had, prev), VerdictFollowable, "", now, generation))
		}

		next[key] = e
	}

	// Removals: a previously-live key absent from next has dropped past the grace. A
	// never-live (refused) key that disappears is not a lifecycle removal — no consumer was
	// acting on it.
	for key, prev := range r.entries {
		if _, present := next[key]; present {
			continue
		}
		if !prev.record.Followable() {
			continue
		}
		events = append(events, lifecycleEvent(
			TypeRemoved, prev.record, prev.record.Followability.Verdict, VerdictRefused,
			ReasonAbsenceExpired, now, generation))
	}

	sortLifecycleEvents(events)
	return events
}

// appendTransitionEvent appends the named transition (if any) between an entry's prior and new
// verdict. A newly observed type produces no transition here — its activation is decided by the
// settle window and a brand-new refused type is silent. TypeActivated and TypeRemoved are
// handled by the caller (they depend on the settle window and on absence, not on a verdict diff).
func appendTransitionEvent(
	events []LifecycleEvent,
	had bool,
	prev, cur entry,
	now time.Time,
	generation uint64,
) []LifecycleEvent {
	if !had {
		return events
	}
	from := prev.record.Followability.Verdict
	to := cur.record.Followability.Verdict
	if from == to {
		return events
	}
	switch {
	case from == VerdictFollowable && to == VerdictRetained:
		return append(events, lifecycleEvent(TypeWobbling, cur.record, from, to, reasonOf(cur.record), now, generation))
	case from == VerdictRetained && to == VerdictFollowable:
		return append(events, lifecycleEvent(TypeRecovered, cur.record, from, to, "", now, generation))
	case to == VerdictRefused && wasLive(from):
		return append(events, lifecycleEvent(TypeRefused, cur.record, from, to, reasonOf(cur.record), now, generation))
	}
	return events
}

// dispatchLifecycle delivers each event to every observer in order. It runs after Update has
// released r.mu, so an observer may read the registry; it stays serialized with other updates
// by the registry's dispatch mutex so events never interleave or reorder across generations.
func dispatchLifecycle(observers []Observer, events []LifecycleEvent) {
	for _, ev := range events {
		for _, obs := range observers {
			obs(ev)
		}
	}
}

// sortLifecycleEvents orders a generation's events deterministically (by GVR then kind) so two
// consumers — and the tests — see the same sequence regardless of map iteration order.
func sortLifecycleEvents(events []LifecycleEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].GVR.String() != events[j].GVR.String() {
			return events[i].GVR.String() < events[j].GVR.String()
		}
		return events[i].Kind < events[j].Kind
	})
}

// lifecycleEvent builds a LifecycleEvent from a record and the verdicts it crossed.
func lifecycleEvent(
	kind EventKind,
	rec TypeRecord,
	from, to Verdict,
	reason Reason,
	at time.Time,
	generation uint64,
) LifecycleEvent {
	return LifecycleEvent{
		Kind:       kind,
		GVK:        rec.Identity.GVK,
		GVR:        rec.Identity.GVR,
		From:       from,
		To:         to,
		Reason:     reason,
		Generation: generation,
		At:         at,
	}
}

// fromVerdict returns the prior verdict for an event, or the empty verdict when there was no
// prior record (a cold-start activation).
func fromVerdict(had bool, prev entry) Verdict {
	if had {
		return prev.record.Followability.Verdict
	}
	return ""
}

// reasonOf returns the single machine-readable reason of a record's first failed check, or the
// empty reason when nothing failed.
func reasonOf(rec TypeRecord) Reason {
	if c, ok := rec.Followability.FirstFailure(); ok {
		return c.Reason
	}
	return ""
}

// wasLive reports whether a verdict means the type was in the live set (followable or held
// under the grace).
func wasLive(v Verdict) bool {
	return v == VerdictFollowable || v == VerdictRetained
}
