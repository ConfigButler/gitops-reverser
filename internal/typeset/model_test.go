// SPDX-License-Identifier: Apache-2.0

package typeset

import "testing"

func TestTypeRecord_Followable(t *testing.T) {
	tests := []struct {
		verdict Verdict
		want    bool
	}{
		{VerdictFollowable, true},
		{VerdictRetained, true},
		{VerdictRefused, false},
		{VerdictUnknown, false},
		{Verdict(""), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.verdict), func(t *testing.T) {
			rec := TypeRecord{Followability: Followability{Verdict: tt.verdict}}
			if got := rec.Followable(); got != tt.want {
				t.Errorf("Followable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFollowability_Check(t *testing.T) {
	f := Followability{Checks: []Check{
		{Requirement: RequirementServed, Result: ResultPass},
		{Requirement: RequirementVerbs, Result: ResultFail, Reason: ReasonMissingVerb},
	}}
	if c, ok := f.Check(RequirementVerbs); !ok || c.Reason != ReasonMissingVerb {
		t.Errorf("Check(verbs) = %+v, %v", c, ok)
	}
	if _, ok := f.Check(RequirementScale); ok {
		t.Error("Check(scale) should report missing")
	}
}

func TestFollowability_FirstFailure(t *testing.T) {
	f := Followability{Checks: []Check{
		{Requirement: RequirementServed, Result: ResultPass},
		{Requirement: RequirementScope, Result: ResultFail, Reason: ReasonScopeUnknown},
		{Requirement: RequirementVerbs, Result: ResultFail, Reason: ReasonMissingVerb},
	}}
	c, ok := f.FirstFailure()
	if !ok || c.Requirement != RequirementScope {
		t.Errorf("FirstFailure() = %+v, %v, want scope", c, ok)
	}

	none := Followability{Checks: []Check{{Requirement: RequirementServed, Result: ResultPass}}}
	if _, ok := none.FirstFailure(); ok {
		t.Error("FirstFailure() on all-pass should report no failure")
	}
}

func TestCheck_Failed(t *testing.T) {
	if !(Check{Result: ResultFail}).Failed() {
		t.Error("fail check should report Failed")
	}
	for _, r := range []Result{ResultPass, ResultSkip, ResultUnknown} {
		if (Check{Result: r}).Failed() {
			t.Errorf("%s check should not report Failed", r)
		}
	}
}
