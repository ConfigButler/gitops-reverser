// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeClock is a manually advanced clock for deterministic grace tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func deploymentObs() Observation {
	return followableObservation()
}

func widgetObs() Observation {
	obs := Observation{
		Identity: Identity{
			GVK:   schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"},
			GVR:   schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
			Scope: ScopeNamespaced,
		},
		Origin:       Origin{Kind: OriginCRD, Confidence: ConfidenceObserved, Evidence: "widgets.example.com"},
		Verbs:        []string{"get", "list", "watch", "patch"},
		Served:       true,
		Trusted:      true,
		CatalogReady: true,
		GVKUnique:    true,
		GVRUnique:    true,
	}
	return obs
}

func TestRegistry_EmptyIsNotReady(t *testing.T) {
	r := NewRegistry()
	if r.Ready() {
		t.Error("a fresh registry must not be ready")
	}
	if _, ok := r.ByGVK(deploymentObs().Identity.GVK); ok {
		t.Error("empty registry should not know any kind")
	}
}

func TestRegistry_UpdateMakesReadyAndLooksUp(t *testing.T) {
	r := NewRegistry()
	r.Update([]Observation{deploymentObs()}, 1)
	if !r.Ready() {
		t.Fatal("registry should be ready after Update")
	}
	if r.Generation() != 1 {
		t.Errorf("generation = %d, want 1", r.Generation())
	}
	rec, ok := r.ByGVK(deploymentObs().Identity.GVK)
	if !ok || !rec.Followable() {
		t.Fatalf("deployment should be followable: ok=%v rec=%+v", ok, rec.Followability)
	}
	byGVR, ok := r.ByGVR(deploymentObs().Identity.GVR)
	if !ok || byGVR.Identity.GVK != rec.Identity.GVK {
		t.Errorf("ByGVR should round-trip to the same record")
	}
}

func TestRegistry_FollowableAndAll(t *testing.T) {
	denied := deploymentObs()
	denied.Identity.GVK.Kind = "Pod"
	denied.Identity.GVR.Resource = "pods"
	denied.Denied = true
	denied.DenyDetail = "excluded by default policy"

	r := NewRegistry()
	r.Update([]Observation{deploymentObs(), denied}, 1)

	if got := len(r.Followable()); got != 1 {
		t.Errorf("Followable() = %d records, want 1 (deployment only)", got)
	}
	if got := len(r.All()); got != 2 {
		t.Errorf("All() = %d records, want 2 (deployment + refused pod)", got)
	}
	pod, ok := r.ByGVR(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "pods"})
	if !ok || pod.Followable() {
		t.Errorf("refused pod should be known but not followable: ok=%v", ok)
	}
}

func TestRegistry_AmbiguousGVKRefused(t *testing.T) {
	// Two resources serve the same kind; both observations carry GVKUnique=false.
	a := widgetObs()
	a.GVKUnique = false
	a.GVKConflictDetail = "widgets, widgetz"
	b := widgetObs()
	b.Identity.GVR.Resource = "widgetz"
	b.GVKUnique = false
	b.GVKConflictDetail = "widgets, widgetz"

	r := NewRegistry()
	r.Update([]Observation{a, b}, 1)

	rec, ok := r.ByGVK(a.Identity.GVK)
	if !ok {
		t.Fatal("ambiguous kind should still be known")
	}
	if rec.Followable() {
		t.Error("ambiguous kind must not be followable")
	}
	check, _ := rec.Followability.Check(RequirementIdentity)
	if check.Reason != ReasonGVKNotUnique {
		t.Errorf("identity reason = %q, want gvk-not-unique", check.Reason)
	}
	// Deterministic: ByGVK returns the first resource by GVR sort ("widgets" < "widgetz").
	if rec.Identity.GVR.Resource != "widgets" {
		t.Errorf("ByGVK returned %q, want the sorted-first widgets", rec.Identity.GVR.Resource)
	}
}

// TestRegistry_ByGroupResource covers the version-less index (S1 of
// typeset-owns-discovery-grace): all served versions of a (group, resource) pair come
// back sorted, a retained-under-grace record is still listed (the wobble-friendly
// answer a stream-key resolution needs), and an unknown pair returns nothing.
func TestRegistry_ByGroupResource(t *testing.T) {
	v1 := widgetObs()
	v2 := widgetObs()
	v2.Identity.GVK.Version = "v2"
	v2.Identity.GVR.Version = "v2"
	v2.Preferred = true

	clock := &fakeClock{t: time.Unix(1_000, 0)}
	r := newRegistry(clock.now)
	r.Update([]Observation{v1, v2}, 1)

	recs := r.ByGroupResource("example.com", "widgets")
	if len(recs) != 2 {
		t.Fatalf("ByGroupResource = %d records, want both served versions", len(recs))
	}
	if recs[0].Identity.GVR.Version != "v1" || recs[1].Identity.GVR.Version != "v2" {
		t.Errorf("records must be GVR-sorted, got %q then %q",
			recs[0].Identity.GVR.Version, recs[1].Identity.GVR.Version)
	}
	if !recs[1].Preferred {
		t.Error("the preferred fact must ride the record unchanged")
	}

	// One version vanishes: within the grace it is retained and still listed.
	clock.add(10 * time.Second)
	r.Update([]Observation{v2}, 2)
	recs = r.ByGroupResource("example.com", "widgets")
	if len(recs) != 2 {
		t.Fatalf("a retained version must still be listed, got %d records", len(recs))
	}
	if recs[0].Followability.Verdict != VerdictRetained {
		t.Errorf("vanished version verdict = %q, want retained", recs[0].Followability.Verdict)
	}

	if got := r.ByGroupResource("example.com", "nonexistent"); len(got) != 0 {
		t.Errorf("unknown pair must return no records, got %d", len(got))
	}
}

func TestRegistry_GraceRetainsThenDrops(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	r := newRegistry(clock.now)

	// Generation 1: deployment is live.
	r.Update([]Observation{deploymentObs()}, 1)
	gvk := deploymentObs().Identity.GVK

	// Generation 2: deployment vanishes. Within the grace it is retained and still live.
	clock.add(10 * time.Second)
	r.Update(nil, 2)
	rec, ok := r.ByGVK(gvk)
	if !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("within grace: verdict = %q, want retained (ok=%v)", rec.Followability.Verdict, ok)
	}
	if !rec.Followable() {
		t.Error("a retained type must still be followable")
	}
	if got := len(r.Followable()); got != 1 {
		t.Errorf("retained type should still count as followable, got %d", got)
	}

	// Still absent, still within grace: stays retained, absentSince does not reset.
	clock.add(40 * time.Second)
	r.Update(nil, 3)
	if rec, ok := r.ByGVK(gvk); !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("still within grace: verdict = %q, want retained", rec.Followability.Verdict)
	}

	// Grace elapses: absence began at +10s, so +40s +25s = 65s of absence >= 60s and
	// the type drops entirely.
	clock.add(25 * time.Second)
	r.Update(nil, 4)
	if _, ok := r.ByGVK(gvk); ok {
		t.Error("after the grace the absent type must drop from the registry")
	}
	if got := len(r.All()); got != 0 {
		t.Errorf("All() = %d, want 0 after drop", got)
	}
}

// TestRegistry_RevisionBumpsOnGraceDropAtStableGeneration is the regression that
// motivated Revision(): a type whose retention grace elapses leaves the followable set
// without any discovery change, so the catalog generation does not move. A consumer
// gated on the generation would never notice; the revision must bump so it re-projects.
func TestRegistry_RevisionBumpsOnGraceDropAtStableGeneration(t *testing.T) {
	clock := &fakeClock{t: time.Unix(5_000, 0)}
	r := newRegistry(clock.now)

	// Generation stays 5 for the whole sequence — only time passes.
	r.Update([]Observation{deploymentObs()}, 5)
	afterFirst := r.Revision()
	if afterFirst == 0 {
		t.Fatal("the first ready Update must bump the revision")
	}

	// Within the grace: retained, same generation, followable set unchanged -> no bump.
	clock.add(10 * time.Second)
	r.Update(nil, 5)
	clock.add(20 * time.Second)
	r.Update(nil, 5)
	if r.Revision() != afterFirst {
		t.Errorf("a retained type within grace must not move the revision: %d != %d", r.Revision(), afterFirst)
	}

	// Grace elapses (70s absence >= 60s): the type drops at the SAME generation, so only
	// the followable-set change can move the revision.
	clock.add(40 * time.Second)
	r.Update(nil, 5)
	if r.Generation() != 5 {
		t.Fatalf("generation must be stable at 5, got %d", r.Generation())
	}
	if r.Revision() <= afterFirst {
		t.Errorf("the grace drop must bump the revision even at a stable generation: %d <= %d",
			r.Revision(), afterFirst)
	}
}

func TestRegistry_ReappearanceClearsGrace(t *testing.T) {
	clock := &fakeClock{t: time.Unix(2_000, 0)}
	r := newRegistry(clock.now)
	gvk := deploymentObs().Identity.GVK

	r.Update([]Observation{deploymentObs()}, 1)
	clock.add(30 * time.Second)
	r.Update(nil, 2) // absent, retained
	clock.add(5 * time.Second)
	r.Update([]Observation{deploymentObs()}, 3) // reappears

	rec, ok := r.ByGVK(gvk)
	if !ok || rec.Followability.Verdict != VerdictFollowable {
		t.Fatalf("reappeared type should be followable again, got %q", rec.Followability.Verdict)
	}

	// And it should not immediately drop on the next absence — the grace restarts.
	clock.add(40 * time.Second)
	r.Update(nil, 4)
	if rec, ok := r.ByGVK(gvk); !ok || rec.Followability.Verdict != VerdictRetained {
		t.Errorf("grace should restart after reappearance, got %q (ok=%v)", rec.Followability.Verdict, ok)
	}
}

func TestRegistry_RefusedTypeNotRetained(t *testing.T) {
	clock := &fakeClock{t: time.Unix(3_000, 0)}
	r := newRegistry(clock.now)

	denied := deploymentObs()
	denied.Denied = true
	r.Update([]Observation{denied}, 1)

	// It was never live, so its disappearance is immediate — no grace hold.
	clock.add(1 * time.Second)
	r.Update(nil, 2)
	if _, ok := r.ByGVK(denied.Identity.GVK); ok {
		t.Error("a refused (never-live) type should drop immediately, not be retained")
	}
}
