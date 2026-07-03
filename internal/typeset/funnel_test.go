// SPDX-License-Identifier: Apache-2.0

package typeset

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// followableObservation is the baseline every funnel test mutates one field of: a
// served, trusted, unambiguous, namespaced built-in Deployment with full verbs.
func followableObservation() Observation {
	return Observation{
		Identity: Identity{
			GVK:   schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			GVR:   schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Scope: ScopeNamespaced,
		},
		Origin:       Origin{Kind: OriginBuiltin, Confidence: ConfidenceInferred},
		Verbs:        []string{"get", "list", "watch", "patch", "create", "delete"},
		Served:       true,
		Trusted:      true,
		CatalogReady: true,
		GVKUnique:    true,
		GVRUnique:    true,
	}
}

func TestEvaluate_Followable(t *testing.T) {
	f := Evaluate(followableObservation())
	if f.Verdict != VerdictFollowable {
		t.Fatalf("verdict = %q, want followable", f.Verdict)
	}
	if f.Summary != "followable" {
		t.Fatalf("summary = %q, want followable", f.Summary)
	}
	if len(f.Checks) != 10 {
		t.Fatalf("got %d checks, want 10 (one per requirement)", len(f.Checks))
	}
	// Every check passes except scale, which is skipped when scale is unused.
	for _, c := range f.Checks {
		if c.Requirement == RequirementScale {
			if c.Result != ResultSkip {
				t.Errorf("scale check = %q, want skip", c.Result)
			}
			continue
		}
		if c.Result != ResultPass {
			t.Errorf("%s check = %q, want pass", c.Requirement, c.Result)
		}
	}
}

func TestEvaluate_CheckOrderIsFunnelOrder(t *testing.T) {
	want := []Requirement{
		RequirementServed, RequirementTrusted, RequirementStable, RequirementIdentity,
		RequirementScope, RequirementVerbs, RequirementOrigin, RequirementPolicy,
		RequirementSensitivity, RequirementScale,
	}
	got := Evaluate(followableObservation()).Checks
	for i, req := range want {
		if got[i].Requirement != req {
			t.Errorf("check[%d] = %q, want %q", i, got[i].Requirement, req)
		}
	}
}

func TestEvaluate_RequirementFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Observation)
		req     Requirement
		reason  Reason
		detail  string
		verdict Verdict
		summary string
	}{
		{
			name:    "not served",
			mutate:  func(o *Observation) { o.Served = false },
			req:     RequirementServed,
			reason:  ReasonNotServed,
			verdict: VerdictRetained,
			summary: "retained — not served",
		},
		{
			name:    "subresource only",
			mutate:  func(o *Observation) { o.Served = false; o.SubresourceOnly = true },
			req:     RequirementServed,
			reason:  ReasonSubresourceOnly,
			verdict: VerdictRetained,
			summary: "retained — served only as a subresource",
		},
		{
			name:    "discovery degraded",
			mutate:  func(o *Observation) { o.Trusted = false },
			req:     RequirementTrusted,
			reason:  ReasonDiscoveryDegraded,
			verdict: VerdictRetained,
			summary: "retained — discovery degraded for its group/version",
		},
		{
			name:    "catalog unavailable",
			mutate:  func(o *Observation) { o.Trusted = false; o.CatalogReady = false },
			req:     RequirementTrusted,
			reason:  ReasonCatalogUnavailable,
			verdict: VerdictUnknown,
			summary: "not followable — API catalog unavailable",
		},
		{
			name:    "absence expired",
			mutate:  func(o *Observation) { o.Served = false; o.AbsenceExpired = true },
			req:     RequirementStable,
			reason:  ReasonAbsenceExpired,
			verdict: VerdictRefused,
			summary: "not followable — not served",
		},
		{
			name:    "gvk not unique",
			mutate:  func(o *Observation) { o.GVKUnique = false; o.GVKConflictDetail = "widgets, widgetz" },
			req:     RequirementIdentity,
			reason:  ReasonGVKNotUnique,
			detail:  "widgets, widgetz",
			verdict: VerdictRefused,
			summary: "not followable — GVK served by more than one resource: widgets, widgetz",
		},
		{
			name:    "gvr not unique",
			mutate:  func(o *Observation) { o.GVRUnique = false; o.GVRConflictDetail = "Widget, Gadget" },
			req:     RequirementIdentity,
			reason:  ReasonGVRNotUnique,
			detail:  "Widget, Gadget",
			verdict: VerdictRefused,
			summary: "not followable — resource resolves to more than one kind: Widget, Gadget",
		},
		{
			name:    "scope unknown",
			mutate:  func(o *Observation) { o.Identity.Scope = ScopeUnknown },
			req:     RequirementScope,
			reason:  ReasonScopeUnknown,
			verdict: VerdictRefused,
			summary: "not followable — scope unknown",
		},
		{
			name:    "missing verb",
			mutate:  func(o *Observation) { o.Verbs = []string{"get", "list"} },
			req:     RequirementVerbs,
			reason:  ReasonMissingVerb,
			detail:  "watch",
			verdict: VerdictRefused,
			summary: "not followable — missing required verb: watch",
		},
		{
			name:    "origin unknown",
			mutate:  func(o *Observation) { o.Origin = Origin{Kind: OriginUnknown} },
			req:     RequirementOrigin,
			reason:  ReasonOriginUnknown,
			verdict: VerdictRefused,
			summary: "not followable — origin could not be classified",
		},
		{
			name:    "denied by policy",
			mutate:  func(o *Observation) { o.Denied = true; o.DenyDetail = "excluded by default policy" },
			req:     RequirementPolicy,
			reason:  ReasonDeniedByPolicy,
			detail:  "excluded by default policy",
			verdict: VerdictRefused,
			summary: "not followable — excluded by default policy",
		},
		{
			name:    "sensitive unsupported",
			mutate:  func(o *Observation) { o.Sensitive = true },
			req:     RequirementSensitivity,
			reason:  ReasonSensitiveUnsupported,
			verdict: VerdictRefused,
			summary: "not followable — sensitive type without supported write handling",
		},
		{
			name: "scale path unresolved",
			mutate: func(o *Observation) {
				o.Subresources.Scale = ScaleBinding{Enabled: true, Usable: false}
			},
			req:     RequirementScale,
			reason:  ReasonScalePathUnresolved,
			verdict: VerdictRefused,
			summary: "not followable — scale parent replica path unresolved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := followableObservation()
			tt.mutate(&obs)
			f := Evaluate(obs)
			if f.Verdict != tt.verdict {
				t.Errorf("verdict = %q, want %q", f.Verdict, tt.verdict)
			}
			if f.Summary != tt.summary {
				t.Errorf("summary = %q, want %q", f.Summary, tt.summary)
			}
			check, ok := f.Check(tt.req)
			if !ok {
				t.Fatalf("missing %s check", tt.req)
			}
			if check.Result != ResultFail {
				t.Errorf("%s result = %q, want fail", tt.req, check.Result)
			}
			if check.Reason != tt.reason {
				t.Errorf("%s reason = %q, want %q", tt.req, check.Reason, tt.reason)
			}
			if check.Detail != tt.detail {
				t.Errorf("%s detail = %q, want %q", tt.req, check.Detail, tt.detail)
			}
		})
	}
}

func TestEvaluate_ScaleUsablePasses(t *testing.T) {
	obs := followableObservation()
	binding, ok := BuiltinScale("apps", "deployments")
	if !ok {
		t.Fatal("expected deployments to be built-in scalable")
	}
	obs.Subresources.Scale = binding
	f := Evaluate(obs)
	if f.Verdict != VerdictFollowable {
		t.Fatalf("verdict = %q, want followable", f.Verdict)
	}
	check, _ := f.Check(RequirementScale)
	if check.Result != ResultPass {
		t.Errorf("scale check = %q, want pass", check.Result)
	}
}

func TestEvaluate_PermanentFailureWinsOverTransient(t *testing.T) {
	// A type that is both absent (served fail) and ambiguous (identity fail) is
	// refused, not retained: a permanent failure is never masked by the grace.
	obs := followableObservation()
	obs.Served = false
	obs.GVKUnique = false
	obs.GVKConflictDetail = "widgets, widgetz"
	f := Evaluate(obs)
	if f.Verdict != VerdictRefused {
		t.Fatalf("verdict = %q, want refused", f.Verdict)
	}
}

func TestEvaluate_FirstFailureDrivesSummary(t *testing.T) {
	// served fails before verbs in funnel order, so the summary names served.
	obs := followableObservation()
	obs.Served = false
	obs.Verbs = []string{"get"}
	f := Evaluate(obs)
	first, ok := f.FirstFailure()
	if !ok || first.Requirement != RequirementServed {
		t.Fatalf("first failure = %+v, want served", first)
	}
}

func TestEvaluate_EmptyScopeTreatedAsUnknown(t *testing.T) {
	obs := followableObservation()
	obs.Identity.Scope = ""
	check, _ := Evaluate(obs).Check(RequirementScope)
	if check.Result != ResultFail || check.Reason != ReasonScopeUnknown {
		t.Errorf("scope check = %+v, want fail/scope-unknown", check)
	}
}

func TestEvaluate_EmptyOriginTreatedAsUnknown(t *testing.T) {
	obs := followableObservation()
	obs.Origin = Origin{}
	check, _ := Evaluate(obs).Check(RequirementOrigin)
	if check.Result != ResultFail || check.Reason != ReasonOriginUnknown {
		t.Errorf("origin check = %+v, want fail/origin-unknown", check)
	}
}

func TestEvaluate_SensitiveSupportedPasses(t *testing.T) {
	obs := followableObservation()
	obs.Sensitive = true
	obs.SensitiveSupported = true
	if Evaluate(obs).Verdict != VerdictFollowable {
		t.Error("sensitive-but-supported should stay followable")
	}
}
