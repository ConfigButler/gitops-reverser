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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// matRecorder is a test MaterializationObserver that captures every event in order.
type matRecorder struct{ events []MaterializationEvent }

func (r *matRecorder) observe(ev MaterializationEvent) { r.events = append(r.events, ev) }

func (r *matRecorder) count(k MaterializationEventKind) int {
	n := 0
	for _, ev := range r.events {
		if ev.Kind == k {
			n++
		}
	}
	return n
}

func (r *matRecorder) last(k MaterializationEventKind) MaterializationEvent {
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Kind == k {
			return r.events[i]
		}
	}
	return MaterializationEvent{}
}

// lc builds a minimal LifecycleEvent — the Materializer only reads Kind and GVR.
func lc(kind EventKind, gvr schema.GroupVersionResource) LifecycleEvent {
	return LifecycleEvent{Kind: kind, GVR: gvr}
}

func depGVR() schema.GroupVersionResource    { return deploymentObs().Identity.GVR }
func widgetGVR() schema.GroupVersionResource { return widgetObs().Identity.GVR }

const aRef GitTargetRef = "git-target-a"

// syncedFixture drives a single claimed, activated type all the way to Synced at "R1",
// the common precondition for the sweep/wobble/release tests.
func syncedFixture(t *testing.T, clock *fakeClock, gvr schema.GroupVersionResource) *Materializer {
	t.Helper()
	m := newMaterializer(clock.now)
	m.Declare(aRef, []schema.GroupVersionResource{gvr})
	m.OnLifecycleEvent(lc(TypeActivated, gvr))
	if !m.BeginSync(gvr) {
		t.Fatalf("setup: BeginSync should start the first sync")
	}
	m.SyncSucceeded(gvr, "R1")
	if ph, _ := m.Phase(gvr); ph != PhaseSynced {
		t.Fatalf("setup: phase = %q, want Synced", ph)
	}
	return m
}

// TestMaterializer_ClaimDrivesSyncRegardlessOfOrder proves DEC-L3/L9: a claim on a
// not-yet-followable type is recorded silently and drives the first sync the moment the
// type becomes followable — and the reverse order (activate, then claim) converges to the
// same Requested phase.
func TestMaterializer_ClaimDrivesSyncRegardlessOfOrder(t *testing.T) {
	t.Run("claim before followable", func(t *testing.T) {
		clock := &fakeClock{t: time.Unix(1_000, 0)}
		m := newMaterializer(clock.now)
		rec := &matRecorder{}
		m.Subscribe(rec.observe)

		// Claim a type that is not yet followable: recorded, no phase change, no event.
		m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
		if ph, ok := m.Phase(depGVR()); !ok || ph != PhaseDormant {
			t.Fatalf("a claim on a not-yet-followable type must stay Dormant, got %q (ok=%v)", ph, ok)
		}
		if rec.count(SyncRequested) != 0 {
			t.Fatalf("claiming an unfollowable type must not request a sync, got %d", rec.count(SyncRequested))
		}
		if got := m.Claimants(depGVR()); len(got) != 1 || got[0] != aRef {
			t.Fatalf("the claim must be recorded/surfaced, got %v", got)
		}

		// It becomes followable: the standing claim now drives the sync with no new message.
		m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
		if ph, _ := m.Phase(depGVR()); ph != PhaseRequested {
			t.Fatalf("becoming followable must move the claimed type to Requested, got %q", ph)
		}
		if rec.count(SyncRequested) != 1 {
			t.Errorf("becoming followable must emit one SyncRequested, got %d", rec.count(SyncRequested))
		}
	})

	t.Run("followable before claim", func(t *testing.T) {
		clock := &fakeClock{t: time.Unix(2_000, 0)}
		m := newMaterializer(clock.now)
		rec := &matRecorder{}
		m.Subscribe(rec.observe)

		// Activated but unclaimed: stays Dormant, no request (materialize only the claimed).
		m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
		if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
			t.Fatalf("an unclaimed followable type must stay Dormant, got %q", ph)
		}
		if rec.count(SyncRequested) != 0 {
			t.Fatalf("an unclaimed followable type must not request a sync, got %d", rec.count(SyncRequested))
		}

		// The claim arrives: now Requested.
		m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
		if ph, _ := m.Phase(depGVR()); ph != PhaseRequested {
			t.Fatalf("a claim on a followable type must move it to Requested, got %q", ph)
		}
		if rec.count(SyncRequested) != 1 {
			t.Errorf("the claim must emit one SyncRequested, got %d", rec.count(SyncRequested))
		}
	})
}

// TestMaterializer_FirstSyncOkReachesSynced walks the happy path Requested -> Syncing ->
// Synced and checks the checkpoint becomes serviceable at the reported rv.
func TestMaterializer_FirstSyncOkReachesSynced(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))

	// Nothing to serve until the sync completes.
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Fatal("a Requested type must not be serviceable")
	}

	if !m.BeginSync(depGVR()) {
		t.Fatal("BeginSync must start the first sync")
	}
	if ph, _ := m.Phase(depGVR()); ph != PhaseSyncing {
		t.Fatalf("after BeginSync the phase = %q, want Syncing", ph)
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Fatal("a Syncing type has nothing to serve yet (L4 hold)")
	}

	m.SyncSucceeded(depGVR(), "1000")
	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Fatalf("after SyncSucceeded the phase = %q, want Synced", ph)
	}
	rv, ok := m.Checkpoint(depGVR())
	if !ok || rv != "1000" {
		t.Errorf("a Synced type must serve its rv: rv=%q ok=%v", rv, ok)
	}
	if rec.count(SyncStarted) != 1 || rec.count(TypeSynced) != 1 {
		t.Errorf("expected one SyncStarted and one TypeSynced, got %d/%d",
			rec.count(SyncStarted), rec.count(TypeSynced))
	}
	if ev := rec.last(TypeSynced); ev.RV != "1000" || ev.Phase != PhaseSynced {
		t.Errorf("TypeSynced event = %+v, want rv=1000 phase=Synced", ev)
	}
}

// TestMaterializer_FirstSyncFailsThenRetries proves the Failing -> retry loop: a failed
// first sync serves nothing and re-surfaces in PendingSyncs, and a retry can succeed.
func TestMaterializer_FirstSyncFailsThenRetries(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	m.BeginSync(depGVR())
	m.SyncFailed(depGVR())

	if ph, _ := m.Phase(depGVR()); ph != PhaseFailing {
		t.Fatalf("a failed first sync must be Failing, got %q", ph)
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Fatal("a first-sync failure has no checkpoint to serve (consumers hold)")
	}
	if pend := m.PendingSyncs(); len(pend) != 1 || pend[0] != depGVR() {
		t.Fatalf("a Failing type must surface for retry, got %v", pend)
	}

	// Backoff retry succeeds.
	if !m.BeginSync(depGVR()) {
		t.Fatal("BeginSync must restart a Failing first sync")
	}
	if ph, _ := m.Phase(depGVR()); ph != PhaseSyncing {
		t.Fatalf("a Failing retry without a prior checkpoint must re-enter Syncing, got %q", ph)
	}
	m.SyncSucceeded(depGVR(), "1500")
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "1500" {
		t.Errorf("after the retry the checkpoint = %q (ok=%v), want 1500", rv, ok)
	}
	if rec.count(SyncFailed) != 1 || rec.count(SyncStarted) != 2 || rec.count(TypeSynced) != 1 {
		t.Errorf("event counts: failed=%d started=%d synced=%d, want 1/2/1",
			rec.count(SyncFailed), rec.count(SyncStarted), rec.count(TypeSynced))
	}
}

// TestMaterializer_ReAnchorKeepsPriorCheckpointServing is the fail-closed refresh (L5): a
// re-anchor — and even a re-anchor that fails — never drops the currently-served
// checkpoint; the prior rv keeps serving until the new one swaps in.
func TestMaterializer_ReAnchorKeepsPriorCheckpointServing(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	// A sweep with a live claim flags the type for a periodic re-anchor.
	clock.add(time.Second)
	m.Sweep()
	if pend := m.PendingSyncs(); len(pend) != 1 {
		t.Fatalf("a still-claimed Synced type must be flagged for re-anchor, got %v", pend)
	}
	if rec.count(SyncRequested) != 1 {
		t.Fatalf("the sweep must request the re-anchor, got %d", rec.count(SyncRequested))
	}

	// The re-anchor LIST starts: Synced -> Resyncing, prior checkpoint still served.
	if !m.BeginSync(depGVR()) {
		t.Fatal("BeginSync must start the re-anchor")
	}
	if ph, _ := m.Phase(depGVR()); ph != PhaseResyncing {
		t.Fatalf("re-anchor phase = %q, want Resyncing", ph)
	}
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "R1" {
		t.Errorf("during a re-anchor the PRIOR checkpoint must keep serving: rv=%q ok=%v", rv, ok)
	}

	// The re-anchor LIST fails: still Failing on the prior checkpoint (never unserved).
	m.SyncFailed(depGVR())
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "R1" {
		t.Errorf("a failed re-anchor must keep serving the prior checkpoint: rv=%q ok=%v", rv, ok)
	}

	// Retry the re-anchor (prior checkpoint present -> Resyncing, not Syncing) and succeed.
	if !m.BeginSync(depGVR()) {
		t.Fatal("BeginSync must restart the failed re-anchor")
	}
	if ph, _ := m.Phase(depGVR()); ph != PhaseResyncing {
		t.Fatalf("a Failing retry WITH a prior checkpoint must re-enter Resyncing, got %q", ph)
	}
	m.SyncSucceeded(depGVR(), "R2")
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "R2" {
		t.Errorf("the new checkpoint must swap in: rv=%q ok=%v, want R2", rv, ok)
	}
}

// TestMaterializer_DemandStopsReleasedAtSweep proves the demand GC (DEC-L5): while the
// claim is renewed the checkpoint is kept (re-anchored); once renewal stops it is released
// at the sweep where the last renewal predates the previous sweep.
func TestMaterializer_DemandStopsReleasedAtSweep(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	// A live claim survives a sweep (kept, re-anchor requested) — not released.
	clock.add(time.Second)
	m.Sweep()
	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Fatalf("a live-claimed type must be kept by the sweep, got %q", ph)
	}
	if rec.count(Released) != 0 {
		t.Fatalf("a live-claimed type must not be released, got %d", rec.count(Released))
	}

	// Demand stops (no further Declare). The next sweep finds the last renewal predates the
	// previous sweep, so the lease is GC'd and the checkpoint released.
	clock.add(time.Second)
	m.Sweep()
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("after demand stops the sweep must release to Dormant, got %q", ph)
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Error("a released type must serve no checkpoint")
	}
	if got := m.Claimants(depGVR()); len(got) != 0 {
		t.Errorf("the withdrawn lease must be GC'd, got %v", got)
	}
	if rec.count(Released) != 1 {
		t.Errorf("the release must emit one Released, got %d", rec.count(Released))
	}
}

// TestMaterializer_RenewalKeepsCheckpointAcrossSweeps proves a healthy consumer that
// re-declares every interval is never released.
func TestMaterializer_RenewalKeepsCheckpointAcrossSweeps(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())

	for i := range 5 {
		clock.add(time.Second)
		m.Declare(aRef, []schema.GroupVersionResource{depGVR()}) // renew
		clock.add(time.Second)
		m.Sweep()
		if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
			t.Fatalf("iteration %d: a renewed claim must keep the type Synced, got %q", i, ph)
		}
	}
}

// TestMaterializer_RequestedWithdrawnBeforeSyncIsReleased covers the §3 "Requested ->
// Dormant: claim withdrawn before first sync" edge.
func TestMaterializer_RequestedWithdrawnBeforeSyncIsReleased(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseRequested {
		t.Fatalf("setup: phase = %q, want Requested", ph)
	}

	// First sweep keeps it (claim still live); second sweep, after renewal stops, releases.
	clock.add(time.Second)
	m.Sweep()
	clock.add(time.Second)
	m.Sweep()
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("a Requested type whose claim is withdrawn must release to Dormant, got %q", ph)
	}
	if rec.count(Released) != 1 {
		t.Errorf("the withdrawal must emit one Released, got %d", rec.count(Released))
	}
}

// TestMaterializer_WobbleFreezesAndRecoverUnfreezes proves DEC-L4's freeze: a wobble
// suspends sync and sweep but keeps the existing checkpoint served; recovery resumes.
func TestMaterializer_WobbleFreezesAndRecoverUnfreezes(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	// Wobble: freeze. The checkpoint keeps serving.
	m.OnLifecycleEvent(lc(TypeWobbling, depGVR()))
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "R1" {
		t.Fatalf("a wobbling type must keep serving its checkpoint: rv=%q ok=%v", rv, ok)
	}

	// Frozen: the sweep neither re-anchors nor releases, nothing is pending, no LIST starts.
	clock.add(time.Second)
	m.Sweep()
	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Fatalf("the sweep must leave a frozen type untouched, got %q", ph)
	}
	if len(m.PendingSyncs()) != 0 {
		t.Errorf("a frozen type must not be pending, got %v", m.PendingSyncs())
	}
	if m.BeginSync(depGVR()) {
		t.Error("BeginSync must not start a sync against a frozen type")
	}
	if rec.count(SyncRequested) != 0 || rec.count(Released) != 0 {
		t.Errorf("a frozen type must emit neither SyncRequested nor Released, got %d/%d",
			rec.count(SyncRequested), rec.count(Released))
	}

	// Recover: unfreeze. The next sweep resumes the re-anchor schedule.
	m.OnLifecycleEvent(lc(TypeRecovered, depGVR()))
	clock.add(time.Second)
	m.Declare(aRef, []schema.GroupVersionResource{depGVR()}) // healthy consumer still renews
	clock.add(time.Second)
	m.Sweep()
	if m.PendingSyncs() == nil {
		t.Error("after recovery a live-claimed type must resume re-anchoring")
	}
}

// TestMaterializer_RemovedForceReleasesButClaimSurvives proves DEC-L4's force-release:
// TypeRemoved drops the checkpoint but the claim survives, so a reappearance re-syncs.
func TestMaterializer_RemovedForceReleasesButClaimSurvives(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.OnLifecycleEvent(lc(TypeRemoved, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("TypeRemoved must force-release to Dormant, got %q", ph)
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Error("a force-released type must serve no checkpoint")
	}
	if rec.count(Released) != 1 {
		t.Fatalf("the force-release must emit one Released, got %d", rec.count(Released))
	}
	if got := m.Claimants(depGVR()); len(got) != 1 || got[0] != aRef {
		t.Fatalf("the claim must survive a force-release, got %v", got)
	}

	// The type reappears: the standing claim drives a fresh sync with no new Declare.
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseRequested {
		t.Fatalf("a reappearance must re-sync from the surviving claim, got %q", ph)
	}
}

// TestMaterializer_RefusedForceReleases proves a permanent refusal releases like a removal.
func TestMaterializer_RefusedForceReleases(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := syncedFixture(t, clock, depGVR())
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.OnLifecycleEvent(lc(TypeRefused, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("TypeRefused must release to Dormant, got %q", ph)
	}
	if rec.count(Released) != 1 {
		t.Errorf("the refusal must emit one Released, got %d", rec.count(Released))
	}
	// The claim-vs-refused mismatch is preserved for status (L10).
	if got := m.Claimants(depGVR()); len(got) != 1 {
		t.Errorf("a claim on a refused type must remain visible, got %v", got)
	}
}

// TestMaterializer_ForceReleaseOnDormantIsNoOp proves a followability loss on a type that
// holds no checkpoint (claimed but never synced) emits nothing — there is nothing to drop.
func TestMaterializer_ForceReleaseOnDormantIsNoOp(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	// A claim is recorded against a type that never becomes followable: it stays Dormant.
	m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("setup: phase = %q, want Dormant", ph)
	}

	m.OnLifecycleEvent(lc(TypeRefused, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseDormant {
		t.Fatalf("a refusal on a Dormant type must leave it Dormant, got %q", ph)
	}
	if rec.count(Released) != 0 {
		t.Errorf("force-releasing a type with no checkpoint must emit no Released, got %d", rec.count(Released))
	}
}

// TestMaterializer_PendingSyncsSorted proves the driver's "what needs a (re)sync?" query
// returns every pending type in deterministic GVR order.
func TestMaterializer_PendingSyncsSorted(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)

	// Claim and activate two types: both land in Requested, both pending.
	m.Declare(aRef, []schema.GroupVersionResource{widgetGVR(), depGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, widgetGVR()))
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))

	pend := m.PendingSyncs()
	if len(pend) != 2 {
		t.Fatalf("both Requested types must be pending, got %v", pend)
	}
	// apps/v1 deployments sorts before example.com/v1 widgets.
	if pend[0].Resource != "deployments" || pend[1].Resource != "widgets" {
		t.Errorf("PendingSyncs not in deterministic GVR order: %v", pend)
	}
}

// TestMaterializer_NoOpTransitionsFromWrongPhase pins the phase machine's guards: the
// driver hooks are no-ops unless the current phase admits them, so a stray or duplicated
// call cannot corrupt state.
func TestMaterializer_NoOpTransitionsFromWrongPhase(t *testing.T) {
	tests := []struct {
		name string
		// arrange leaves the type in a chosen phase, then act runs one hook; want is the
		// phase afterwards and whether the hook should have been a no-op.
		arrange func(m *Materializer)
		act     func(m *Materializer)
		want    Phase
	}{
		{
			name:    "SyncSucceeded on Requested (never began) is ignored",
			arrange: func(_ *Materializer) {},
			act:     func(m *Materializer) { m.SyncSucceeded(depGVR(), "x") },
			want:    PhaseRequested,
		},
		{
			name:    "SyncFailed on Requested (never began) is ignored",
			arrange: func(_ *Materializer) {},
			act:     func(m *Materializer) { m.SyncFailed(depGVR()) },
			want:    PhaseRequested,
		},
		{
			name:    "BeginSync on Synced without a pending re-anchor is ignored",
			arrange: func(m *Materializer) { m.BeginSync(depGVR()); m.SyncSucceeded(depGVR(), "x") },
			act:     func(m *Materializer) { m.BeginSync(depGVR()) },
			want:    PhaseSynced,
		},
		{
			name:    "SyncFailed on Synced is ignored",
			arrange: func(m *Materializer) { m.BeginSync(depGVR()); m.SyncSucceeded(depGVR(), "x") },
			act:     func(m *Materializer) { m.SyncFailed(depGVR()) },
			want:    PhaseSynced,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(1_000, 0)}
			m := newMaterializer(clock.now)
			m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
			m.OnLifecycleEvent(lc(TypeActivated, depGVR())) // -> Requested
			tc.arrange(m)
			tc.act(m)
			if ph, _ := m.Phase(depGVR()); ph != tc.want {
				t.Errorf("phase = %q, want %q", ph, tc.want)
			}
		})
	}
}

// TestMaterializer_BeginSyncOnUnknownType is a no-op and never panics.
func TestMaterializer_BeginSyncOnUnknownType(t *testing.T) {
	m := NewMaterializer()
	if m.BeginSync(depGVR()) {
		t.Error("BeginSync on an unknown type must report no sync started")
	}
	m.SyncSucceeded(depGVR(), "x") // must not panic
	m.SyncFailed(depGVR())         // must not panic
	if _, ok := m.Phase(depGVR()); ok {
		t.Error("an untouched type must not be tracked")
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Error("an untouched type serves no checkpoint")
	}
}

// TestMaterializer_DeterministicOrderAndFanout mirrors the registry's multi-subscriber
// determinism: two types requested in one Declare emit events in GVR order to every
// subscriber, and a nil subscriber is a safe no-op.
func TestMaterializer_DeterministicOrderAndFanout(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	a, b := &matRecorder{}, &matRecorder{}
	m.Subscribe(a.observe)
	m.Subscribe(b.observe)
	m.Subscribe(nil) // must not panic

	// Both types are followable first, then claimed together in one Declare.
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	m.OnLifecycleEvent(lc(TypeActivated, widgetGVR()))
	m.Declare(aRef, []schema.GroupVersionResource{widgetGVR(), depGVR()})

	if a.count(SyncRequested) != 2 || b.count(SyncRequested) != 2 {
		t.Fatalf("both subscribers must see both requests: a=%d b=%d",
			a.count(SyncRequested), b.count(SyncRequested))
	}
	// Deterministic: apps/v1 deployments sorts before example.com/v1 widgets despite the
	// reversed Declare order.
	if a.events[0].GVR.Resource != "deployments" || a.events[1].GVR.Resource != "widgets" {
		t.Errorf("events not in deterministic GVR order: %+v", a.events)
	}
}

// TestSortMaterializationEvents_GVRThenKind pins the deterministic dispatch order: events
// sort by GVR first, then by kind as a stable tiebreaker for same-GVR events.
func TestSortMaterializationEvents_GVRThenKind(t *testing.T) {
	events := []MaterializationEvent{
		{Kind: TypeSynced, GVR: widgetGVR()},
		{Kind: SyncStarted, GVR: depGVR()},
		// Two events on the same GVR: the kind tiebreaker decides ("Released" < "SyncStarted").
		{Kind: SyncStarted, GVR: depGVR()},
		{Kind: Released, GVR: depGVR()},
	}
	sortMaterializationEvents(events)

	want := []MaterializationEventKind{Released, SyncStarted, SyncStarted, TypeSynced}
	for i, ev := range events {
		if ev.Kind != want[i] {
			t.Fatalf("events[%d].Kind = %q, want %q (full order %+v)", i, ev.Kind, want[i], events)
		}
	}
	if events[3].GVR.Resource != "widgets" {
		t.Errorf("the widget event must sort last by GVR, got %q", events[3].GVR.Resource)
	}
}

// TestMaterializer_PerTypeIsolation proves L6: one type's sync state never bleeds into a
// sibling's. A failing deployment leaves a synced widget serviceable.
func TestMaterializer_PerTypeIsolation(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	m.Declare(aRef, []schema.GroupVersionResource{depGVR(), widgetGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	m.OnLifecycleEvent(lc(TypeActivated, widgetGVR()))

	// Widget syncs; deployment fails its first sync.
	m.BeginSync(widgetGVR())
	m.SyncSucceeded(widgetGVR(), "W1")
	m.BeginSync(depGVR())
	m.SyncFailed(depGVR())

	if rv, ok := m.Checkpoint(widgetGVR()); !ok || rv != "W1" {
		t.Errorf("the synced sibling must stay serviceable: rv=%q ok=%v", rv, ok)
	}
	if _, ok := m.Checkpoint(depGVR()); ok {
		t.Error("the failing type must serve nothing")
	}
	if ph, _ := m.Phase(widgetGVR()); ph != PhaseSynced {
		t.Errorf("the synced sibling phase = %q, want Synced", ph)
	}
}

// TestMaterializer_RestoreSyncedResumesWithoutFill proves the DEC-L6 boot rebuild: replaying a
// durable checkpoint marks the type Synced at rv with no fill, and a later activation of an
// already-Synced type does not request one.
func TestMaterializer_RestoreSyncedResumesWithoutFill(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)
	rec := &matRecorder{}
	m.Subscribe(rec.observe)

	m.RestoreSynced(depGVR(), "R7")
	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Fatalf("RestoreSynced must land Synced, got %q", ph)
	}
	if rv, ok := m.Checkpoint(depGVR()); !ok || rv != "R7" {
		t.Fatalf("restored checkpoint rv = %q (ok=%v), want R7", rv, ok)
	}
	if rec.count(TypeSynced) != 0 {
		t.Errorf("RestoreSynced is a silent boot restore, must emit no event, got %d", rec.count(TypeSynced))
	}

	// A subsequent activation for an already-Synced type is a no-op — no re-fill requested.
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Errorf("activation of a restored Synced type must stay Synced, got %q", ph)
	}
	if rec.count(SyncRequested) != 0 {
		t.Errorf("a restored Synced type must not request a fill on activation, got %d", rec.count(SyncRequested))
	}

	// An empty rv is ignored (nothing durable to restore).
	m.RestoreSynced(widgetGVR(), "")
	if _, ok := m.Phase(widgetGVR()); ok {
		t.Error("RestoreSynced with empty rv must not create state")
	}
}

// TestMaterializer_InventoryReportsPerTypeStatus proves the L-6 visibility query: every tracked
// type's phase, checkpoint rv, followability, and claimants — including a claim on a refused
// type (the claim-vs-refused mismatch), surfaced sorted by GVR.
func TestMaterializer_InventoryReportsPerTypeStatus(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)

	m.Declare(aRef, []schema.GroupVersionResource{depGVR(), widgetGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	m.BeginSync(depGVR())
	m.SyncSucceeded(depGVR(), "R9")
	m.OnLifecycleEvent(lc(TypeRefused, widgetGVR())) // claimed but refused -> mismatch

	inv := m.Inventory()
	if len(inv) != 2 {
		t.Fatalf("inventory must cover both tracked types, got %d (%+v)", len(inv), inv)
	}
	dep := inv[0] // apps/v1 deployments sorts before example.com/v1 widgets
	if dep.GVR != depGVR() || dep.Phase != PhaseSynced || dep.CheckpointRV != "R9" || !dep.Followable {
		t.Errorf("dep status = %+v, want deployments Synced@R9 followable", dep)
	}
	if len(dep.Claimants) != 1 || dep.Claimants[0] != aRef {
		t.Errorf("dep claimants = %v, want [%s]", dep.Claimants, aRef)
	}
	wid := inv[1]
	if wid.GVR != widgetGVR() || wid.Followable {
		t.Errorf("widget must be claimed-but-not-followable (mismatch), got %+v", wid)
	}
	if len(wid.Claimants) != 1 || wid.Claimants[0] != aRef {
		t.Errorf("a claim on a refused type must remain visible, got %v", wid.Claimants)
	}
}

// TestMaterializer_MultiClaimantLeaseGC proves the lease table tracks claimants
// independently and a sweep keeps a checkpoint alive while any one claimant still renews.
func TestMaterializer_MultiClaimantLeaseGC(t *testing.T) {
	const bRef GitTargetRef = "git-target-b"
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	m := newMaterializer(clock.now)

	m.Declare(aRef, []schema.GroupVersionResource{depGVR()})
	m.Declare(bRef, []schema.GroupVersionResource{depGVR()})
	m.OnLifecycleEvent(lc(TypeActivated, depGVR()))
	m.BeginSync(depGVR())
	m.SyncSucceeded(depGVR(), "R1")
	if got := m.Claimants(depGVR()); len(got) != 2 {
		t.Fatalf("both GitTargets must hold a claim, got %v", got)
	}

	// Only B keeps renewing; A goes silent. The first sweep still sees A's lease as live
	// (renewed at or after the previous sweep); the second sweep finds it stale and GCs it.
	clock.add(time.Second)
	m.Declare(bRef, []schema.GroupVersionResource{depGVR()})
	clock.add(time.Second)
	m.Sweep()
	clock.add(time.Second)
	m.Declare(bRef, []schema.GroupVersionResource{depGVR()})
	clock.add(time.Second)
	m.Sweep()

	if ph, _ := m.Phase(depGVR()); ph != PhaseSynced {
		t.Fatalf("a checkpoint with one live claimant must stay Synced, got %q", ph)
	}
	if got := m.Claimants(depGVR()); len(got) != 1 || got[0] != bRef {
		t.Errorf("A's stale lease must be GC'd, leaving only B: %v", got)
	}
}
