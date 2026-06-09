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

package manifestanalyzer

import (
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// inScopeGroupResource matches one type's (group, resource) — the per-type sweep predicate
// the M12 watch layer builds from a removed/activated GVR.
func inScopeGroupResource(group, resource string) func(types.ResourceIdentifier) bool {
	return func(ri types.ResourceIdentifier) bool {
		return ri.Group == group && ri.Resource == resource
	}
}

// TestBuildScopedPlan_SweepsOnlyTargetType is the per-type sweep: an empty desired set scoped
// to apps/deployments drops the Deployment only — the two ConfigMaps, a different type, are
// left untouched even though they too have no desired counterpart.
func TestBuildScopedPlan_SweepsOnlyTargetType(t *testing.T) {
	store := planStore(t)
	plan := BuildScopedPlan(store, planFiles(), nil, Policy{}, inScopeGroupResource("apps", "deployments"))

	counts := plan.Counts()
	if counts[PlanDropOrphan] != 1 || len(plan.Actions) != 1 {
		t.Fatalf("counts=%v actions=%d, want exactly one drop (the Deployment)", counts, len(plan.Actions))
	}
	drop := findAction(t, plan, "deploy.yaml")
	wantRI := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
	if drop.Kind != PlanDropOrphan || drop.Resource != wantRI {
		t.Errorf("deploy.yaml#0 = %+v, want drop-orphan with %+v", drop, wantRI)
	}
	for _, a := range plan.Actions {
		if a.Ref.FilePath == "cm.yaml" {
			t.Errorf("an out-of-scope ConfigMap must never be swept by a deployments-scoped plan: %+v", a)
		}
	}
}

// TestBuildScopedPlan_ReconcileDropsInScopeOrphanKeepsSiblings is the per-type reconcile: the
// desired set holds only ConfigMap "a", scoped to v1/configmaps. ConfigMap "b" (in scope, not
// desired) is dropped; ConfigMap "a" is in sync (no action); the Deployment (out of scope) is
// never touched despite also being absent from desired.
func TestBuildScopedPlan_ReconcileDropsInScopeOrphanKeepsSiblings(t *testing.T) {
	store := planStore(t)
	desired := []DesiredResource{desiredConfigMap("a")}
	plan := BuildScopedPlan(store, planFiles(), desired, Policy{}, inScopeGroupResource("", "configmaps"))

	if got := plan.Counts()[PlanDropOrphan]; got != 1 {
		t.Fatalf("want exactly one drop (ConfigMap b), got %d (actions=%+v)", got, plan.Actions)
	}
	for _, a := range plan.Actions {
		if a.Kind == PlanDropOrphan && a.Resource.Resource != "configmaps" {
			t.Errorf("a configmaps-scoped reconcile must only drop configmaps, dropped: %+v", a)
		}
		if a.Resource.Resource == "deployments" {
			t.Errorf("the out-of-scope Deployment must produce no action: %+v", a)
		}
	}
}

// TestBuildScopedPlan_AllInScopeEqualsBuildPlan proves the refactor is behaviour-preserving:
// BuildScopedPlan with the always-true predicate is byte-identical to BuildPlan (the
// whole-folder mark-and-sweep), so both share one set of safety guarantees.
func TestBuildScopedPlan_AllInScopeEqualsBuildPlan(t *testing.T) {
	store := planStore(t)
	full := BuildPlan(store, planFiles(), nil, Policy{})
	scoped := BuildScopedPlan(planStore(t), planFiles(), nil, Policy{}, allInScope)

	if len(full.Actions) != len(scoped.Actions) {
		t.Fatalf("action count differs: BuildPlan=%d allInScope=%d", len(full.Actions), len(scoped.Actions))
	}
	if full.Counts()[PlanDropOrphan] != scoped.Counts()[PlanDropOrphan] {
		t.Errorf("drop-orphan count differs: BuildPlan=%d allInScope=%d",
			full.Counts()[PlanDropOrphan], scoped.Counts()[PlanDropOrphan])
	}
}
