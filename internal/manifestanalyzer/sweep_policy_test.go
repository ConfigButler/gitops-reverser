// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// convergingPolicy is the planning policy that MODELS FULL CONVERGENCE: it drops orphans.
//
// Every pre-existing planner test uses it, because the mark-and-sweep is what they were written
// to pin. It is named rather than inlined so those tests state which prune policy they model —
// the planner's own default retains, and a test that silently inherited that default would keep
// passing while asserting nothing about the sweep.
func convergingPolicy() Policy { return Policy{Sweep: SweepDropOrphans} }

// TestSweepMode_OnlyDropExplicitlyDrops pins the direction of the zero value. It is the whole
// safety property in one assertion: a caller that forgets the field retains, and so does a value
// this build does not recognize.
func TestSweepMode_OnlyDropExplicitlyDrops(t *testing.T) {
	for _, tc := range []struct {
		mode SweepMode
		want bool
	}{
		{SweepDropOrphans, true},
		{SweepRetainOrphans, false},
		{SweepUnspecified, false},
		{SweepMode("shred-everything"), false},
	} {
		if got := tc.mode.DropsOrphans(); got != tc.want {
			t.Errorf("SweepMode(%q).DropsOrphans() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestBuildPlan_RetainDoesNotPlanTheDrop is the planner half of PR 5: with a retaining policy,
// the managed drops the same store produces under convergence are not merely filtered out of the
// applied actions — they are never planned. The assertion is deliberately on the WHOLE plan, not
// on a count of drop actions: a suppressed drop must not reach the action list, so it cannot
// reach the plan hash or the commit path either.
func TestBuildPlan_RetainDoesNotPlanTheDrop(t *testing.T) {
	store := planStore(t)

	converging := BuildPlan(store, planFiles(), nil, convergingPolicy())
	if converging.Counts()[PlanDropOrphan] != 3 {
		t.Fatalf("fixture drift: want 3 drops under convergence, got %v", converging.Counts())
	}

	retaining := BuildPlan(store, planFiles(), nil, Policy{Sweep: SweepRetainOrphans})
	if got := retaining.Counts()[PlanDropOrphan]; got != 0 {
		t.Errorf("drop-orphan actions under a retaining policy = %d, want 0", got)
	}
	if len(retaining.Actions) != 0 {
		t.Errorf("a retaining plan over an empty desired set must be empty, got %+v", retaining.Actions)
	}
	if retaining.RetainedOrphans != 3 {
		t.Errorf("RetainedOrphans = %d, want 3 — the suppressed drops must still be counted",
			retaining.RetainedOrphans)
	}
}

// TestBuildScopedPlan_RetainKeepsTheInScopeOrphan proves the gate applies to the SCOPED planner
// too, which is the one production actually reaches: every resync the watch layer issues carries
// a non-nil scope. Without this, gating only BuildPlan would leave the live sweep untouched.
func TestBuildScopedPlan_RetainKeepsTheInScopeOrphan(t *testing.T) {
	store := planStore(t)
	inScope := inScopeGroupResource("apps", "deployments")

	converging := BuildScopedPlan(store, planFiles(), nil, convergingPolicy(), inScope)
	if converging.Counts()[PlanDropOrphan] != 1 {
		t.Fatalf("fixture drift: want the Deployment dropped under convergence, got %v", converging.Counts())
	}

	retaining := BuildScopedPlan(store, planFiles(), nil, Policy{Sweep: SweepRetainOrphans}, inScope)
	if len(retaining.Actions) != 0 {
		t.Errorf("a retaining scoped plan must plan nothing, got %+v", retaining.Actions)
	}
	if retaining.RetainedOrphans != 1 {
		t.Errorf("RetainedOrphans = %d, want 1", retaining.RetainedOrphans)
	}
}

// TestBuildPlan_RetainStillUpsertsAndSkips proves retention is scoped to DELETIONS and nothing
// else. A retaining policy that also suppressed creates or patches would quietly stop mirroring,
// which is a far worse failure than the stale document it is trying to avoid — and it would be
// invisible, because both look like "no commit".
func TestBuildPlan_RetainStillUpsertsAndSkips(t *testing.T) {
	store := planStore(t)
	// One in-sync resource, one drifted, one absent from Git, and the encrypted Secret.
	desired := []DesiredResource{desiredConfigMap("a"), desiredDeployWeb(9), desiredSecret()}

	plan := BuildPlan(store, planFiles(), desired, Policy{Sweep: SweepRetainOrphans})

	counts := plan.Counts()
	if counts[PlanDropOrphan] != 0 {
		t.Errorf("drop-orphan actions = %d, want 0 under retention", counts[PlanDropOrphan])
	}
	if counts[PlanPatch]+counts[PlanReplace] == 0 {
		t.Errorf("a drifted resource must still be edited under retention: %v", counts)
	}
	if counts[PlanSkip] == 0 {
		t.Errorf("an encrypted document must still be reported as a skip under retention: %v", counts)
	}
	// The one ConfigMap the desired set does not name is retained, not dropped.
	if plan.RetainedOrphans != 1 {
		t.Errorf("RetainedOrphans = %d, want 1 (configmap b)", plan.RetainedOrphans)
	}
}

// TestFolderScanPlanPolicy_Converges pins the dry-run's deliberate choice. A scan has no
// GitTarget and therefore no prune policy to read; reporting the folder's orphans is the
// analysis the tool exists to produce, and nothing it does can delete anything.
func TestFolderScanPlanPolicy_Converges(t *testing.T) {
	if !FolderScanPlanPolicy().Sweep.DropsOrphans() {
		t.Error("an offline folder scan must report managed drops, not silently omit them")
	}
}

// TestBuildScopedPlan_RetainIsNotAnEmptyScope distinguishes the two gates that both end up
// suppressing a drop. inScope answers "is this document any of my business"; Sweep answers "may I
// delete the ones that are". Implementing retention as an empty scope predicate would be tempting
// and would pass the drop assertions above — but it erases the distinction, and with it the
// operator's only signal: an out-of-scope document is not this plan's concern and is counted
// nowhere, while a RETAINED one is a document this plan owns, considered, and deliberately kept.
func TestBuildScopedPlan_RetainIsNotAnEmptyScope(t *testing.T) {
	store := planStore(t)
	all := func(types.ResourceIdentifier) bool { return true }

	retaining := BuildScopedPlan(store, planFiles(), nil, Policy{Sweep: SweepRetainOrphans}, all)
	if retaining.RetainedOrphans == 0 {
		t.Error("a document in scope and deliberately kept must be counted as retained")
	}

	outOfScope := BuildScopedPlan(store, planFiles(), nil, convergingPolicy(),
		func(types.ResourceIdentifier) bool { return false })
	if len(outOfScope.Actions) != 0 {
		t.Errorf("an empty scope reports nothing at all, got %+v", outOfScope.Actions)
	}
	if outOfScope.RetainedOrphans != 0 {
		t.Errorf("an out-of-scope document is not a retained orphan; RetainedOrphans = %d",
			outOfScope.RetainedOrphans)
	}
}
