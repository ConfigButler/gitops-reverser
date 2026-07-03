// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// mkEntry builds a namespaced, policy-allowed, followable-verbed served entry at v1 —
// the common fixture shape. Tests needing a cluster-scoped, denied, or other-version
// entry build it inline.
func mkEntry(group, kind, resource string) Entry {
	return Entry{
		GVK:        schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind},
		GVR:        schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource},
		Namespaced: true,
		Verbs:      []string{"get", "list", "watch"},
		Allowed:    true,
	}
}

func observationByGVR(obs []Observation, resource string) (Observation, bool) {
	for _, o := range obs {
		if o.Identity.GVR.Resource == resource {
			return o, true
		}
	}
	return Observation{}, false
}

func TestObservationsFromEntries_BuiltinAndCRDAndPolicy(t *testing.T) {
	entries := []Entry{
		mkEntry("apps", "Deployment", "deployments"),
		mkEntry("shop.example.com", "Widget", "widgets"),
		{
			GVK:          schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
			GVR:          schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			Namespaced:   true,
			Verbs:        []string{"get", "list", "watch"},
			Allowed:      false,
			PolicyReason: "excluded by default policy",
		},
	}
	obs := ObservationsFromEntries(entries, true)
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3", len(obs))
	}

	dep, _ := observationByGVR(obs, "deployments")
	if dep.Origin.Kind != OriginBuiltin {
		t.Errorf("apps Deployment origin = %q, want builtin", dep.Origin.Kind)
	}
	if dep.Identity.Scope != ScopeNamespaced || !dep.Served || !dep.Trusted || !dep.CatalogReady {
		t.Errorf("deployment observation facts wrong: %+v", dep)
	}
	if Evaluate(dep).Verdict != VerdictFollowable {
		t.Errorf("deployment should evaluate followable, got %q", Evaluate(dep).Verdict)
	}

	widget, _ := observationByGVR(obs, "widgets")
	if widget.Origin.Kind != OriginCRD {
		t.Errorf("widget origin = %q, want crd", widget.Origin.Kind)
	}

	pod, _ := observationByGVR(obs, "pods")
	if !pod.Denied || pod.DenyDetail != "excluded by default policy" {
		t.Errorf("pod should carry the deny reason, got %+v", pod)
	}
}

func TestObservationsFromEntries_AmbiguousGVK(t *testing.T) {
	entries := []Entry{
		mkEntry("shop.example.com", "Widget", "widgets"),
		mkEntry("shop.example.com", "Widget", "widgetz"),
	}
	obs := ObservationsFromEntries(entries, true)
	for _, o := range obs {
		if o.GVKUnique {
			t.Errorf("%s should be marked non-unique", o.Identity.GVR.Resource)
		}
		if o.GVKConflictDetail != "widgets, widgetz" {
			t.Errorf("conflict detail = %q, want 'widgets, widgetz'", o.GVKConflictDetail)
		}
	}
}

func TestObservationsFromEntries_AmbiguousGVR(t *testing.T) {
	// One resource resolving to two Kinds is the reverse half of the bijection. Real
	// discovery keeps a resource name unique per group/version, so this only arises on
	// a malformed or hand-crafted surface — the model must still refuse it rather than
	// silently pick a winner, so two manifest Kinds cannot resolve to one identity.
	gvr := schema.GroupVersionResource{Group: "shop.example.com", Version: "v1", Resource: "things"}
	entries := []Entry{
		{
			GVK: schema.GroupVersionKind{Group: "shop.example.com", Version: "v1", Kind: "Thing"},
			GVR: gvr, Namespaced: true, Verbs: []string{"get", "list", "watch"}, Allowed: true,
		},
		{
			GVK: schema.GroupVersionKind{Group: "shop.example.com", Version: "v1", Kind: "Gadget"},
			GVR: gvr, Namespaced: true, Verbs: []string{"get", "list", "watch"}, Allowed: true,
		},
	}
	obs := ObservationsFromEntries(entries, true)
	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2", len(obs))
	}
	for _, o := range obs {
		if o.GVRUnique {
			t.Errorf("%s should be marked GVR-non-unique", o.Identity.GVK.Kind)
		}
		if o.GVRConflictDetail != "Gadget, Thing" {
			t.Errorf("gvr conflict detail = %q, want 'Gadget, Thing'", o.GVRConflictDetail)
		}
		// GVK uniqueness is unaffected: each kind still has exactly one resource.
		if !o.GVKUnique {
			t.Errorf("%s should remain GVK-unique (one resource per kind)", o.Identity.GVK.Kind)
		}
		if Evaluate(o).Verdict != VerdictRefused {
			t.Errorf("an ambiguous-GVR type must be refused, got %q", Evaluate(o).Verdict)
		}
		check, _ := Evaluate(o).Check(RequirementIdentity)
		if check.Reason != ReasonGVRNotUnique {
			t.Errorf("identity reason = %q, want gvr-not-unique", check.Reason)
		}
	}
}

func TestObservationsFromEntries_ExactDuplicateIsNotAConflict(t *testing.T) {
	// The same resource listed twice (identical GVR+GVK) must collapse, not be mistaken
	// for an ambiguity in either direction.
	e := mkEntry("apps", "Deployment", "deployments")
	for _, o := range ObservationsFromEntries([]Entry{e, e}, true) {
		if !o.GVKUnique || !o.GVRUnique {
			t.Errorf("an exact duplicate must stay unique both ways, got gvk=%v gvr=%v",
				o.GVKUnique, o.GVRUnique)
		}
	}
}

func TestObservationsFromEntries_SubresourcesFolded(t *testing.T) {
	entries := []Entry{
		mkEntry("apps", "Deployment", "deployments"),
		{
			GVK:         schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Scale"},
			GVR:         schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments/scale"},
			Subresource: true,
		},
		{
			GVK:         schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			GVR:         schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments/status"},
			Subresource: true,
		},
		// A CRD with a /scale subresource whose replica path is unknown.
		mkEntry("shop.example.com", "Widget", "widgets"),
		{
			GVK: schema.GroupVersionKind{Group: "shop.example.com", Version: "v1", Kind: "Scale"},
			GVR: schema.GroupVersionResource{
				Group:    "shop.example.com",
				Version:  "v1",
				Resource: "widgets/scale",
			},
			Subresource: true,
		},
	}
	obs := ObservationsFromEntries(entries, true)

	// Subresources never become their own observation.
	if _, ok := observationByGVR(obs, "deployments/scale"); ok {
		t.Error("a subresource must not produce its own observation")
	}

	dep, _ := observationByGVR(obs, "deployments")
	if !dep.Subresources.Status.Enabled {
		t.Error("deployment should fold in its /status subresource")
	}
	if !dep.Subresources.Scale.Enabled || !dep.Subresources.Scale.Usable {
		t.Errorf("deployment /scale should be enabled and usable (built-in), got %+v", dep.Subresources.Scale)
	}

	widget, _ := observationByGVR(obs, "widgets")
	if !widget.Subresources.Scale.Enabled || widget.Subresources.Scale.Usable {
		t.Errorf("CRD /scale should be enabled but not usable (unknown path), got %+v", widget.Subresources.Scale)
	}
}

func TestObservationsFromEntries_ScopeAndSensitivityAndDegraded(t *testing.T) {
	entries := []Entry{
		{ // cluster-scoped built-in
			GVK:        schema.GroupVersionKind{Version: "v1", Kind: "Namespace"},
			GVR:        schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			Namespaced: false,
			Verbs:      []string{"get", "list", "watch"},
			Allowed:    true,
		},
		{ // sensitive resource (the entry builder applied the policy), on a degraded GV
			GVK:        schema.GroupVersionKind{Version: "v1", Kind: "Secret"},
			GVR:        schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
			Namespaced: true,
			Verbs:      []string{"get", "list", "watch"},
			Allowed:    true,
			Degraded:   true,
			Sensitive:  true,
		},
	}
	obs := ObservationsFromEntries(entries, true)

	ns, _ := observationByGVR(obs, "namespaces")
	if ns.Identity.Scope != ScopeCluster {
		t.Errorf("namespace scope = %q, want ClusterScoped", ns.Identity.Scope)
	}

	secret, _ := observationByGVR(obs, "secrets")
	if !secret.Sensitive || !secret.SensitiveSupported {
		t.Errorf("core Secret should be sensitive-but-supported, got sensitive=%v supported=%v",
			secret.Sensitive, secret.SensitiveSupported)
	}
	if secret.Trusted {
		t.Error("a degraded group/version must observe its types as untrusted")
	}
}
