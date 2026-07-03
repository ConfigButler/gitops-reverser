// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"testing"
	"time"
)

// eventRecorder is a test Observer that captures every lifecycle event in order.
type eventRecorder struct{ events []LifecycleEvent }

func (e *eventRecorder) observe(ev LifecycleEvent) { e.events = append(e.events, ev) }

func (e *eventRecorder) count(k EventKind) int {
	n := 0
	for _, ev := range e.events {
		if ev.Kind == k {
			n++
		}
	}
	return n
}

func (e *eventRecorder) last(k EventKind) LifecycleEvent {
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].Kind == k {
			return e.events[i]
		}
	}
	return LifecycleEvent{}
}

func (e *eventRecorder) reset() { e.events = nil }

func TestRegistry_NoActivationOnFirstObserveThenActivatesAfterWindow(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	// A just-appeared followable type must NOT activate on first observation — it has not
	// yet been stably followable for the settle window.
	r.Update([]Observation{deploymentObs()}, 1)
	if got := rec.count(TypeActivated); got != 0 {
		t.Fatalf("first observe must not activate, got %d TypeActivated", got)
	}

	// Still inside the window: no activation.
	clock.add(SettleWindow - time.Second)
	r.Update([]Observation{deploymentObs()}, 2)
	if got := rec.count(TypeActivated); got != 0 {
		t.Fatalf("within the settle window must not activate, got %d", got)
	}

	// Window elapsed: exactly one activation, carrying the type identity and From="" (cold).
	clock.add(2 * time.Second)
	r.Update([]Observation{deploymentObs()}, 3)
	if got := rec.count(TypeActivated); got != 1 {
		t.Fatalf("after the settle window must activate once, got %d", got)
	}
	ev := rec.last(TypeActivated)
	if ev.To != VerdictFollowable || ev.GVR != deploymentObs().Identity.GVR {
		t.Errorf("activation event = %+v, want followable deployments", ev)
	}

	// A subsequent stable Update must NOT re-activate (one per streak).
	clock.add(10 * time.Second)
	r.Update([]Observation{deploymentObs()}, 4)
	if got := rec.count(TypeActivated); got != 1 {
		t.Errorf("a stable followable streak must activate once, got %d", got)
	}
}

func TestRegistry_WobbleThenRecoverThenReactivate(t *testing.T) {
	clock := &fakeClock{t: time.Unix(2_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	// Settle the type first.
	r.Update([]Observation{deploymentObs()}, 1)
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs()}, 2)
	if rec.count(TypeActivated) != 1 {
		t.Fatalf("setup: expected one activation, got %d", rec.count(TypeActivated))
	}
	rec.reset()

	// Vanishes within the grace -> retained -> TypeWobbling, never TypeRemoved.
	clock.add(5 * time.Second)
	r.Update(nil, 3)
	if rec.count(TypeWobbling) != 1 || rec.count(TypeRemoved) != 0 {
		t.Fatalf("wobble: wobbling=%d removed=%d, want 1/0", rec.count(TypeWobbling), rec.count(TypeRemoved))
	}

	// Reappears -> TypeRecovered, but not yet re-activated (settle restarts).
	clock.add(2 * time.Second)
	r.Update([]Observation{deploymentObs()}, 4)
	if rec.count(TypeRecovered) != 1 {
		t.Fatalf("recover: recovered=%d, want 1", rec.count(TypeRecovered))
	}
	if rec.count(TypeActivated) != 0 {
		t.Fatalf("recover must not immediately re-activate, got %d", rec.count(TypeActivated))
	}

	// After the window from recovery -> a fresh TypeActivated.
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs()}, 5)
	if rec.count(TypeActivated) != 1 {
		t.Errorf("re-activation after recovery: got %d, want 1", rec.count(TypeActivated))
	}
}

func TestRegistry_FlapInsideWindowActivatesOnce(t *testing.T) {
	clock := &fakeClock{t: time.Unix(3_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	// Appears.
	r.Update([]Observation{deploymentObs()}, 1)
	// Flap inside the window: wobble then recover, all before the settle window elapses.
	clock.add(time.Second)
	r.Update(nil, 2) // retained
	clock.add(time.Second)
	r.Update([]Observation{deploymentObs()}, 3) // recovered, settle restarts here
	if rec.count(TypeActivated) != 0 {
		t.Fatalf("a flap inside the window must not activate, got %d", rec.count(TypeActivated))
	}

	// Settle from the recovery point -> exactly one activation despite the churn.
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs()}, 4)
	if got := rec.count(TypeActivated); got != 1 {
		t.Errorf("flapping then settling must activate exactly once, got %d", got)
	}
}

func TestRegistry_GraceExpiryEmitsTypeRemoved(t *testing.T) {
	clock := &fakeClock{t: time.Unix(4_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	r.Update([]Observation{deploymentObs()}, 1)
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs()}, 2) // activated
	rec.reset()

	// Vanishes; within grace it is retained (a wobble), not removed.
	clock.add(10 * time.Second)
	r.Update(nil, 3)
	if rec.count(TypeRemoved) != 0 {
		t.Fatalf("within grace must not remove, got %d", rec.count(TypeRemoved))
	}

	// Grace elapses (absence began at +SettleWindow+1+10; advance past 60s of absence).
	clock.add(RemovalGrace)
	r.Update(nil, 4)
	if got := rec.count(TypeRemoved); got != 1 {
		t.Fatalf("after the grace must emit one TypeRemoved, got %d", got)
	}
	ev := rec.last(TypeRemoved)
	if ev.Reason != ReasonAbsenceExpired || ev.To != VerdictRefused {
		t.Errorf("removal event = %+v, want absence-expired -> refused", ev)
	}
}

func TestRegistry_LivePermanentRefuseEmitsTypeRefused(t *testing.T) {
	clock := &fakeClock{t: time.Unix(5_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	r.Update([]Observation{deploymentObs()}, 1)
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs()}, 2) // activated
	rec.reset()

	// A second resource starts serving the same GVK: the kind becomes gvk-not-unique, a
	// PERMANENT refusal, for the previously-followable deployments record.
	dep := deploymentObs()
	dep.GVKUnique = false
	dep.GVKConflictDetail = "deployments, deploymentz"
	other := deploymentObs()
	other.Identity.GVR.Resource = "deploymentz"
	other.GVKUnique = false
	other.GVKConflictDetail = "deployments, deploymentz"

	clock.add(time.Second)
	r.Update([]Observation{dep, other}, 3)
	if got := rec.count(TypeRefused); got != 1 {
		t.Fatalf("a live type failing a permanent check must emit one TypeRefused, got %d", got)
	}
	ev := rec.last(TypeRefused)
	if ev.Reason != ReasonGVKNotUnique || ev.GVR.Resource != "deployments" {
		t.Errorf("refused event = %+v, want gvk-not-unique on deployments", ev)
	}
	// The brand-new deploymentz record, born refused, must be silent.
	if rec.count(TypeActivated) != 0 {
		t.Errorf("a born-refused type must not activate, got %d", rec.count(TypeActivated))
	}
}

func TestRegistry_BrandNewRefusedIsSilent(t *testing.T) {
	clock := &fakeClock{t: time.Unix(6_000, 0)}
	r := newRegistry(clock.now)
	rec := &eventRecorder{}
	r.Subscribe(rec.observe)

	denied := deploymentObs()
	denied.Denied = true
	denied.DenyDetail = "excluded by default policy"
	r.Update([]Observation{denied}, 1)
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{denied}, 2)

	if len(rec.events) != 0 {
		t.Errorf("a type that is refused from birth must emit no lifecycle events, got %+v", rec.events)
	}
}

func TestRegistry_MultipleSubscribersAndDeterministicOrder(t *testing.T) {
	clock := &fakeClock{t: time.Unix(7_000, 0)}
	r := newRegistry(clock.now)
	a, b := &eventRecorder{}, &eventRecorder{}
	r.Subscribe(a.observe)
	r.Subscribe(b.observe)
	r.Subscribe(nil) // no-op, must not panic

	// Two followable types appear together and settle together.
	r.Update([]Observation{deploymentObs(), widgetObs()}, 1)
	clock.add(SettleWindow + time.Second)
	r.Update([]Observation{deploymentObs(), widgetObs()}, 2)

	gotA, gotB := a.count(TypeActivated), b.count(TypeActivated)
	if gotA != 2 || gotB != 2 {
		t.Fatalf("both subscribers must see both activations: a=%d b=%d", gotA, gotB)
	}
	// Deterministic order: sorted by GVR string, so apps/v1 deployments precedes
	// example.com/v1 widgets.
	if a.events[0].GVR.Resource != "deployments" || a.events[1].GVR.Resource != "widgets" {
		t.Errorf("events not in deterministic GVR order: %+v", a.events)
	}
}
