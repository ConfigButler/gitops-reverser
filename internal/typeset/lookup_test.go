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

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNewSnapshotRegistry_NotReadyIsStructureOnly(t *testing.T) {
	r := NewSnapshotRegistry(Snapshot{NotReady: true, Entries: []Entry{
		mkEntry("apps", "Deployment", "deployments"),
	}})
	if r.Ready() {
		t.Fatal("a NotReady snapshot must yield an un-ready (structure-only) registry")
	}
	if _, ok := r.ByGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}); ok {
		t.Error("a structure-only registry must know no kind")
	}
}

func TestNewSnapshotRegistry_ResolvesAndAssumesVerbs(t *testing.T) {
	// The entry sets Allowed but no Verbs: the snapshot assumes followable verbs.
	r := NewSnapshotRegistry(Snapshot{
		Generation: 7,
		Entries: []Entry{{
			GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Namespaced: true,
			Allowed:    true,
		}},
	})
	if !r.Ready() || r.Generation() != 7 {
		t.Fatalf("ready=%v generation=%d, want ready at 7", r.Ready(), r.Generation())
	}
	rec, ok := r.ByGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if !ok || !rec.Followable() {
		t.Fatalf(
			"a snapshot entry with no verbs should be assumed followable, got ok=%v rec=%+v",
			ok,
			rec.Followability,
		)
	}
}

func TestNewSnapshotRegistry_VerbPoorEntryStaysRefused(t *testing.T) {
	// An explicit verb-poor entry (missing watch) is honored, not assumed followable.
	r := NewSnapshotRegistry(Snapshot{Entries: []Entry{{
		GVK:        schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		GVR:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespaced: true,
		Allowed:    true,
		Verbs:      []string{"get", "list"},
	}}})
	rec, ok := r.ByGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if !ok || rec.Followable() {
		t.Fatalf("a verb-poor entry must stay refused, got ok=%v followable=%v", ok, rec.Followable())
	}
	check, _ := rec.Followability.Check(RequirementVerbs)
	if check.Reason != ReasonMissingVerb || check.Detail != "watch" {
		t.Errorf("verbs check = %+v, want missing-verb: watch", check)
	}
}

func TestNewSnapshotRegistry_DegradedMarksUntrusted(t *testing.T) {
	gv := schema.GroupVersion{Group: "shop.example.com", Version: "v1"}
	r := NewSnapshotRegistry(Snapshot{
		DegradedGroupVersions: []schema.GroupVersion{gv},
		Entries: []Entry{
			mkEntry("shop.example.com", "Widget", "widgets"),
		},
	})
	rec, ok := r.ByGVK(schema.GroupVersionKind{Group: "shop.example.com", Version: "v1", Kind: "Widget"})
	if !ok {
		t.Fatal("the widget should be known")
	}
	// A degraded group/version makes trusted fail; within grace that is retained.
	if rec.Followability.Verdict != VerdictRetained {
		t.Errorf("a degraded type should be retained, got %q", rec.Followability.Verdict)
	}
	trusted, _ := rec.Followability.Check(RequirementTrusted)
	if trusted.Reason != ReasonDiscoveryDegraded {
		t.Errorf("trusted check = %+v, want discovery-degraded", trusted)
	}
}
