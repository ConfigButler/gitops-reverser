// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func scanEntry(group, kind, resource string) Entry {
	return Entry{
		GVK:        schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind},
		GVR:        schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource},
		Namespaced: true,
		Verbs:      []string{"get", "list", "watch"},
		Allowed:    true,
	}
}

func gvOf(e Entry) schema.GroupVersion { return e.GVR.GroupVersion() }

// fullScan builds a complete scan serving exactly the given entries.
func fullScan(generation uint64, entries ...Entry) Scan {
	scanned := map[schema.GroupVersion]struct{}{}
	scan := Scan{Complete: true, Generation: generation}
	for _, e := range entries {
		scan.Entries = append(scan.Entries, e)
		if _, ok := scanned[gvOf(e)]; !ok {
			scanned[gvOf(e)] = struct{}{}
			scan.ScannedGroupVersions = append(scan.ScannedGroupVersions, gvOf(e))
		}
	}
	return scan
}

// TestUpdateFromScan_RetainOnErrorOutlivesTheGrace is the catalog's
// retain-on-failure semantic, relocated (S2): a failed group/version's records keep
// serving with last-known facts, marked untrusted/Retained, for as long as the error
// persists — with NO absence clock, so even far beyond RemovalGrace nothing drops.
// A clean scan recovers them to Followable.
func TestUpdateFromScan_RetainOnErrorOutlivesTheGrace(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000, 0)}
	r := newRegistry(clock.now)
	dep := scanEntry("apps", "Deployment", "deployments")
	cm := scanEntry("", "ConfigMap", "configmaps")

	r.UpdateFromScan(fullScan(1, dep, cm))
	if rec, ok := r.ByGVR(dep.GVR); !ok || rec.Followability.Verdict != VerdictFollowable {
		t.Fatalf("seed: deployments should be followable, ok=%v", ok)
	}

	// apps/v1 errors: its records are carried untrusted, core stays fresh.
	failedScan := Scan{
		Entries:              []Entry{cm},
		ScannedGroupVersions: []schema.GroupVersion{gvOf(cm)},
		FailedGroupVersions:  []schema.GroupVersion{gvOf(dep)},
		Complete:             false,
		Generation:           2,
	}
	clock.add(10 * time.Second)
	r.UpdateFromScan(failedScan)
	rec, ok := r.ByGVR(dep.GVR)
	if !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("failed GV: verdict = %q, want retained (ok=%v)", rec.Followability.Verdict, ok)
	}
	if check, _ := rec.Followability.Check(RequirementTrusted); check.Reason != ReasonDiscoveryDegraded {
		t.Errorf("trusted reason = %q, want discovery-degraded", check.Reason)
	}

	// Far beyond the removal grace, still failing: retained — errors have no clock.
	clock.add(10 * RemovalGrace)
	failedScan.Generation = 2 // unchanged facts, stable generation
	r.UpdateFromScan(failedScan)
	if rec, ok = r.ByGVR(dep.GVR); !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("long-failing GV must stay retained, verdict = %q ok=%v", rec.Followability.Verdict, ok)
	}

	// Recovery: a clean scan restores Followable.
	r.UpdateFromScan(fullScan(3, dep, cm))
	if rec, ok = r.ByGVR(dep.GVR); !ok || rec.Followability.Verdict != VerdictFollowable {
		t.Fatalf("recovered GV must be followable again, verdict = %q ok=%v", rec.Followability.Verdict, ok)
	}
}

// TestUpdateFromScan_CompleteOmissionRidesTheGrace is the wobble fix (S2): a complete
// scan that simply no longer lists a group/version does NOT prune its records — they
// go absent and ride the existing RemovalGrace (Retained, still followable), dropping
// only once the grace elapses. A reappearance inside the grace fully recovers.
func TestUpdateFromScan_CompleteOmissionRidesTheGrace(t *testing.T) {
	clock := &fakeClock{t: time.Unix(2_000, 0)}
	r := newRegistry(clock.now)
	ice := scanEntry("shop.example.com", "IceCreamOrder", "icecreamorders")
	cm := scanEntry("", "ConfigMap", "configmaps")

	r.UpdateFromScan(fullScan(1, ice, cm))

	// The CRD group blinks out of a complete scan: retained, still followable.
	clock.add(10 * time.Second)
	r.UpdateFromScan(fullScan(2, cm))
	rec, ok := r.ByGVR(ice.GVR)
	if !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("omitted GV within grace: verdict = %q, want retained (ok=%v)", rec.Followability.Verdict, ok)
	}
	if !rec.Followable() {
		t.Fatal("a graced omission must keep the type followable")
	}

	// Reappears inside the grace: followable again, clock cleared.
	clock.add(20 * time.Second)
	r.UpdateFromScan(fullScan(3, ice, cm))
	if rec, ok = r.ByGVR(ice.GVR); !ok || rec.Followability.Verdict != VerdictFollowable {
		t.Fatalf("reappeared GV must be followable, verdict = %q ok=%v", rec.Followability.Verdict, ok)
	}

	// Gone again, and this time the grace elapses: the record drops.
	clock.add(10 * time.Second)
	r.UpdateFromScan(fullScan(4, cm))
	clock.add(RemovalGrace + time.Second)
	r.UpdateFromScan(fullScan(4, cm))
	if _, ok = r.ByGVR(ice.GVR); ok {
		t.Fatal("a confirmed disappearance must drop after the grace")
	}
}

// TestUpdateFromScan_IncompleteScanJudgesOnlyWhatItSaw: on an incomplete scan, an
// unscanned, non-failed group/version's records are carried unchanged (no judgement),
// while a record missing from a group/version the scan DID list is meaningfully
// absent and starts its grace even though the scan was incomplete.
func TestUpdateFromScan_IncompleteScanJudgesOnlyWhatItSaw(t *testing.T) {
	clock := &fakeClock{t: time.Unix(3_000, 0)}
	r := newRegistry(clock.now)
	dep := scanEntry("apps", "Deployment", "deployments")
	cm := scanEntry("", "ConfigMap", "configmaps")
	secret := scanEntry("", "Secret", "secrets")
	wardle := schema.GroupVersion{Group: "wardle.example.com", Version: "v1alpha1"}

	r.UpdateFromScan(fullScan(1, dep, cm, secret))

	// Incomplete scan: wardle fails, apps is not scanned at all, core is scanned but
	// no longer lists secrets.
	clock.add(10 * time.Second)
	r.UpdateFromScan(Scan{
		Entries:              []Entry{cm},
		ScannedGroupVersions: []schema.GroupVersion{gvOf(cm)},
		FailedGroupVersions:  []schema.GroupVersion{wardle},
		Complete:             false,
		Generation:           2,
	})

	// apps (unscanned, not failed): carried unchanged — still fully followable.
	if rec, ok := r.ByGVR(dep.GVR); !ok || rec.Followability.Verdict != VerdictFollowable {
		t.Fatalf("unscanned GV on incomplete scan must stay followable, verdict = %q ok=%v",
			rec.Followability.Verdict, ok)
	}
	// secrets (scanned core GV, missing): meaningfully absent — retained under grace.
	if rec, ok := r.ByGVR(secret.GVR); !ok || rec.Followability.Verdict != VerdictRetained {
		t.Fatalf("missing record of a scanned GV must be retained, verdict = %q ok=%v",
			rec.Followability.Verdict, ok)
	}
}
