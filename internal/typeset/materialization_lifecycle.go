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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MaterializationEventKind names a per-type transition the Materializer emits on the
// materialization axis. It mirrors the followability EventKind vocabulary
// (lifecycle.go) but is a distinct, second axis: these events report demand-driven
// checkpoint progress, never a followability decision. The Materializer owns the
// transition, computes it once, and names it, so a driver never re-detects the edge by
// diffing phase tables.
type MaterializationEventKind string

const (
	// SyncRequested fires when a claimed, followable type needs a (re)sync the driver
	// should pick up: Dormant -> Requested on the first claim (T1), or a still-claimed
	// Synced type flagged for a periodic re-anchor by the sweep (T4). The driver learns
	// what to sync from this event or from PendingSyncs.
	SyncRequested MaterializationEventKind = "SyncRequested"
	// SyncStarted fires when the driver begins a sync — a streaming-list watch, LIST only
	// as fallback: Requested -> Syncing (first sync) or Synced -> Resyncing (re-anchor). A
	// re-anchor keeps serving the prior checkpoint until it swaps in (DEC-L2 / L5).
	SyncStarted MaterializationEventKind = "SyncStarted"
	// TypeSynced fires when a checkpoint lands: Syncing/Resyncing -> Synced at rv R. It is
	// the completion handshake (L4) — the driver wakes every GitTarget claiming the type.
	TypeSynced MaterializationEventKind = "TypeSynced"
	// SyncFailed fires when a sync errors: Syncing/Resyncing -> Failing. A first-sync
	// failure leaves no checkpoint (consumers hold); a re-anchor failure keeps the prior
	// checkpoint served (L5). Per-type isolation (L6): siblings are unaffected.
	SyncFailed MaterializationEventKind = "SyncFailed"
	// Released fires when a checkpoint is dropped: Synced/Requested/Failing -> Dormant.
	// Either the sweep found no live claim (demand GC, T5) or a followability event
	// force-released the type (TypeRemoved/TypeRefused). The claim itself may survive.
	Released MaterializationEventKind = "Released"
	// Unclaimed fires when a type's LAST claim is withdrawn — the sweep's lease GC removed the
	// final claimant (>=1 -> 0). It is the demand-gate CLOSE edge: the driver maps it to
	// gate.Unrequire, so a type stops being mirrored once no GitTarget wants it. It is deliberately
	// distinct from Released, which is a CHECKPOINT drop: a followability wobble (TypeRemoved) force-
	// releases the checkpoint while the claim survives, and such a type must keep being mirrored — so
	// the gate flag tracks the claim (Unclaimed), never the checkpoint (Released). The open edge has
	// no event: the watch layer Requires synchronously on Declare (see DeclareForGitTarget).
	Unclaimed MaterializationEventKind = "Unclaimed"
)

// MaterializationEvent is one named transition on the materialization axis for a single
// type. It mirrors LifecycleEvent's shape: identity, the new phase it lands in, the
// checkpoint revision it now serves (set on TypeSynced, otherwise the prior checkpoint
// or empty), and the time it was observed.
type MaterializationEvent struct {
	Kind  MaterializationEventKind
	GVR   schema.GroupVersionResource
	Phase Phase
	// RV is the checkpoint revision the type serves after the transition: the new rv on
	// TypeSynced, the prior (still-served) rv during a re-anchor, or empty when no
	// checkpoint exists.
	RV string
	At time.Time
}

// MaterializationObserver receives materialization events from the Materializer. Like a
// followability Observer it is invoked after the new phase is published, serialized with
// other operations, so an observer may read the Materializer but must NOT block (a real
// consumer enqueues and returns). See Materializer.Subscribe.
type MaterializationObserver func(MaterializationEvent)

// Subscribe registers an observer for every materialization event from subsequent
// operations. Observers are invoked outside the Materializer's read/write lock (so they
// may read it) but under its dispatch serialization, so a slow observer stalls the
// caller — keep them non-blocking.
func (m *Materializer) Subscribe(obs MaterializationObserver) {
	if obs == nil {
		return
	}
	m.mu.Lock()
	m.observers = append(m.observers, obs)
	m.mu.Unlock()
}

// dispatchMaterialization delivers each event to every observer in order. It runs after
// the mutating operation has released m.mu, so an observer may read the Materializer; it
// stays serialized by m.dispatchMu so events never interleave or reorder across
// operations.
func dispatchMaterialization(observers []MaterializationObserver, events []MaterializationEvent) {
	for _, ev := range events {
		for _, obs := range observers {
			obs(ev)
		}
	}
}

// sortMaterializationEvents orders one operation's events deterministically (by GVR then
// kind) so two consumers — and the tests — see the same sequence regardless of map
// iteration order. It mirrors sortLifecycleEvents.
func sortMaterializationEvents(events []MaterializationEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].GVR.String() != events[j].GVR.String() {
			return events[i].GVR.String() < events[j].GVR.String()
		}
		return events[i].Kind < events[j].Kind
	})
}
