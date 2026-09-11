// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"strings"
	"testing"
	"testing/fstest"
)

// labeledConfigMapRequest is newConfigMapRequest with metadata.labels attached — the only
// difference the "{label:key}" variable reads.
func labeledConfigMapRequest(name string, labels map[string]string) PlacementRequest {
	req := newConfigMapRequest(name, "app")
	req.Labels = labels
	return req
}

func TestLocateNew_LabelVariable_RendersTheValue(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team}/{namespace}/{name}.yaml"}
	req := labeledConfigMapRequest("cache", map[string]string{"team": "payments"})

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "payments/app/cache.yaml"; res.Path != want || res.Source != PlacementSourceDefault {
		t.Fatalf("got %+v, want %q from the declared default", res, want)
	}
}

// A prefixed key is the common case in practice (app.kubernetes.io/…), and it is the case the
// widened placeholder pattern exists for: its "/" must be consumed as part of the variable name,
// never pasted into the path as a directory separator.
func TestLocateNew_PrefixedLabelKey_IsOneVariableNotTwoSegments(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{
		Default: "{label:app.kubernetes.io/instance}/{name}.yaml",
	}
	req := labeledConfigMapRequest("cache", map[string]string{
		"app.kubernetes.io/instance": "voter",
	})

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "voter/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// A resource missing the label is still placed — at the built-in unlabeled sentinel, the same
// trick {namespace} already uses for "_cluster" on a cluster-scoped resource: a fixed,
// value rather than a refusal or an invisible per-deployment default.
func TestLocateNew_MissingLabel_PlacesAtTheUnlabeledSentinel(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team}/{namespace}/{name}.yaml"}
	req := labeledConfigMapRequest("cache", map[string]string{"other": "x"})

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "_unlabeled/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// Kubernetes accepts an empty label value, and an empty segment is dropped from the rendered
// path — so "present but empty" has to fall back exactly as "absent" does, or every resource
// carrying the label empty folds onto the wrong file.
func TestLocateNew_EmptyLabelValue_FallsBackLikeAMissingLabel(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team}/{namespace}/{name}.yaml"}
	req := labeledConfigMapRequest("cache", map[string]string{"team": ""})

	res, err := LocateNew(store, policy, req)
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "_unlabeled/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// The sentinel is per resource, not per template: an unlabeled resource and its labeled sibling
// each resolve independently.
func TestLocateNew_MissingLabel_DoesNotAffectALabeledSibling(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team}/{namespace}/{name}.yaml"}

	bare, err := LocateNew(store, policy, labeledConfigMapRequest("bare", nil))
	if err != nil {
		t.Fatalf("LocateNew for the unlabeled resource: %v", err)
	}
	if want := "_unlabeled/app/bare.yaml"; bare.Path != want {
		t.Fatalf("got %q, want %q", bare.Path, want)
	}
	res, err := LocateNew(store, policy,
		labeledConfigMapRequest("cache", map[string]string{"team": "payments"}))
	if err != nil {
		t.Fatalf("LocateNew for the labeled sibling: %v", err)
	}
	if want := "payments/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// A label is not identity, so it may add discrimination to a sensitive path but can never supply
// the {name}/scope part that makes one identity-complete.
func TestIdentityCompletePlacementTemplate_LabelCountsForNothing(t *testing.T) {
	if IdentityCompletePlacementTemplate("{label:team}/{label:app}.yaml", true) {
		t.Error("labels alone must not make a template identity-complete")
	}
	if !IdentityCompletePlacementTemplate("{label:team}/{namespace}/{name}.yaml", true) {
		t.Error("a label beside a complete identity must not make the template incomplete")
	}
}

func TestValidPlacementTemplateSyntax_Labels(t *testing.T) {
	cases := []struct {
		tmpl string
		ok   bool
		why  string
	}{
		{"{label:team}/{name}.yaml", true, "a bare label key"},
		{"{label:app.kubernetes.io/name}/{name}.yaml", true, "a prefixed label key"},
		{"{label:}/{name}.yaml", false, "an empty label key"},
		{"{label:not a key}/{name}.yaml", false, "a label key with spaces"},
		{"{label:a//b}/{name}.yaml", false, "a label key with two slashes"},
		{"{label:-bad}/{name}.yaml", false, "a label name that does not start alphanumeric"},
		{"{annotation:team}/{name}.yaml", false, "annotations are not a placement variable"},
	}
	for _, tc := range cases {
		err := ValidPlacementTemplateSyntax(tc.tmpl)
		if tc.ok && err != nil {
			t.Errorf("%s: ValidPlacementTemplateSyntax(%q) = %v, want accepted", tc.why, tc.tmpl, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: ValidPlacementTemplateSyntax(%q) = nil, want rejected", tc.why, tc.tmpl)
		}
	}
}

// The syntax gate must not need a resource to run: it is the static Validated check, and a label
// has no value until an object arrives. A template referencing a label it cannot resolve is
// valid; the resource that does not carry it is refused later, at write time.
func TestValidPlacementTemplateSyntax_LabelNeedsNoValue(t *testing.T) {
	if err := ValidPlacementTemplateSyntax("{label:team}/{namespace}/{name}.yaml"); err != nil {
		t.Fatalf("ValidPlacementTemplateSyntax: %v, want a label template to pass the static gate", err)
	}
}

// Widening the placeholder pattern is the load-bearing half of this feature: before it, anything
// that was not "{word}" was left in the rendered path as literal text, so a mistyped label
// placeholder would have written its own braces — and its "/" — into the repository.
func TestRenderPlacementTemplate_MalformedPlaceholderIsReportedNotPastedThrough(t *testing.T) {
	for _, tmpl := range []string{
		"{lable:team}/{name}.yaml",
		"{label:app.kubernetes.io/na me}/{name}.yaml",
		"{}/{name}.yaml",
	} {
		got, err := RenderPlacementTemplate(tmpl, map[string]string{"name": "app"})
		if err == nil {
			t.Errorf("RenderPlacementTemplate(%q) = %q, want an unknown-variable error", tmpl, got)
		}
	}
}

// A genuinely unknown variable is still reported, even beside a label placeholder that resolves
// fine (to the sentinel, since no value is supplied here).
func TestRenderPlacementTemplate_UnknownVariableIsReportedBesideALabel(t *testing.T) {
	_, err := RenderPlacementTemplate("{bogus}/{label:team}.yaml", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "{bogus}") {
		t.Fatalf("err = %v, want it to name {bogus}", err)
	}
}

func TestPlacementVars_LabelsCannotShadowABuiltInVariable(t *testing.T) {
	req := labeledConfigMapRequest("cache", map[string]string{"namespace": "hijacked"})
	vars := placementVars(req)
	if vars["namespace"] != "app" {
		t.Errorf("namespace = %q, want the resource's own namespace, not a label of the same name",
			vars["namespace"])
	}
	if vars["label:namespace"] != "hijacked" {
		t.Errorf("label:namespace = %q, want the label value", vars["label:namespace"])
	}
}

// A label value cannot legally contain "/", but the value is read off a live object rather than
// validated by this package, so the segment defence has to hold for it too.
func TestRenderPlacementTemplate_SanitizesSlashInALabelValue(t *testing.T) {
	got, err := RenderPlacementTemplate("{label:team}/{name}.yaml", map[string]string{
		"label:team": "a/b", "name": "app",
	})
	if err != nil {
		t.Fatalf("RenderPlacementTemplate: %v", err)
	}
	if want := "a%2Fb/app.yaml"; got != want {
		t.Errorf("got %q, want %q (slash percent-encoded, not a path separator)", got, want)
	}
}

// An explicit fallback overrides the built-in unlabeled sentinel with the author's own bucket name.
func TestLocateNew_LabelFallback_OverridesTheSentinel(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|unassigned}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", nil))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "unassigned/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// The fallback is only a fallback: a resource that carries the label is unaffected by it.
func TestLocateNew_LabelFallback_YieldsToARealValue(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|unassigned}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", map[string]string{"team": "payments"}))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "payments/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// An empty label value is "absent" for the fallback too, matching the sentinel it replaces.
func TestLocateNew_LabelFallback_CoversAnEmptyValue(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|unassigned}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", map[string]string{"team": ""}))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "unassigned/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// The fallback is author-supplied text that goes straight into a path, so it is fenced by the
// half of the label-value rules that protects a path segment — no separator, no "..", bounded
// length — and deliberately NOT by the half that only serves label semantics: a fallback may
// start with "_", which a label value may not, and may be empty.
func TestValidPlacementTemplateSyntax_LabelFallback(t *testing.T) {
	cases := []struct {
		tmpl string
		ok   bool
		why  string
	}{
		{"{label:team|unassigned}/{name}.yaml", true, "an ordinary fallback"},
		{"{label:team|no-team.v2}/{name}.yaml", true, "the dots and dashes a label value allows"},
		{"{label:team|_none}/{name}.yaml", true, "a leading underscore, which no label value can have"},
		{"{label:team|_unlabeled}/{name}.yaml", true, "the built-in sentinel, named explicitly"},
		{"{label:team|}/{name}.yaml", true, "an empty fallback: a declared request to collapse the segment"},
		{"{label:team|" + strings.Repeat("a", 63) + "}/{name}.yaml", true,
			"the longest a label value may be"},
		{"{label:team|" + strings.Repeat("a", 64) + "}/{name}.yaml", false,
			"one character longer than any label value"},
		{"{label:team|../escape}/{name}.yaml", false, "a fallback escaping the write jail"},
		{"{label:team|a/b}/{name}.yaml", false, "a fallback inventing a directory"},
		{"{label:team|..}/{name}.yaml", false, "a bare parent-directory fallback"},
		{"{label:team|.}/{name}.yaml", false, "a bare current-directory fallback"},
		{`{label:team|a\b}/{name}.yaml`, false, "a fallback with a backslash separator"},
		{"{label:team|has space}/{name}.yaml", false, "a fallback with a space"},
		{"{label:not a key|x}/{name}.yaml", false, "a bad key is still a bad key with a fallback"},
	}
	for _, tc := range cases {
		err := ValidPlacementTemplateSyntax(tc.tmpl)
		if tc.ok && err != nil {
			t.Errorf("%s: ValidPlacementTemplateSyntax(%q) = %v, want accepted", tc.why, tc.tmpl, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: ValidPlacementTemplateSyntax(%q) = nil, want rejected", tc.why, tc.tmpl)
		}
	}
}

func TestPlacementTemplateLabelKeys(t *testing.T) {
	got := PlacementTemplateLabelKeys(
		"{label:team}/{namespace}/{label:app.kubernetes.io/instance}/{label:bad key}/{name}.yaml")
	want := []string{"team", "app.kubernetes.io/instance"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (malformed keys are left to the syntax check)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if keys := PlacementTemplateLabelKeys("{namespace}/{name}.yaml"); keys != nil {
		t.Errorf("got %v, want no keys for a template that reads no label", keys)
	}
}

// The fallback belongs to the placeholder, not to the key: a caller checking which labels a
// template reads must see "team", never "team|unassigned".
func TestPlacementTemplateLabelKeys_StripsTheFallback(t *testing.T) {
	got := PlacementTemplateLabelKeys("{label:team|unassigned}/{name}.yaml")
	if len(got) != 1 || got[0] != "team" {
		t.Fatalf("got %v, want [team]", got)
	}
}

// A fallback may start with "_" even though a label value may not, and that is the point: it is
// the only way a template can name a bucket no real label value will ever land in. The built-in
// sentinel is spelled this way too, and may be named explicitly.
func TestLocateNew_UnderscoreFallback_NamesACollisionProofBucket(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|_none}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", nil))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "_none/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// An empty fallback is a declared "no segment here": the resource lands one directory up rather
// than in a bucket of its own. The sentinel protects the default; a template that spells this out
// has made the choice in text, so it is honoured.
func TestLocateNew_EmptyFallback_CollapsesTheSegment(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", nil))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q (the empty fallback drops its segment)", res.Path, want)
	}
}

// The empty fallback still only answers for the resources that are missing the label: a labeled
// sibling keeps its own bucket, so the two do not merge.
func TestLocateNew_EmptyFallback_LeavesALabeledResourceAlone(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{label:team|}/{namespace}/{name}.yaml"}

	res, err := LocateNew(store, policy,
		labeledConfigMapRequest("cache", map[string]string{"team": "payments"}))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "payments/app/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// An empty fallback in the middle of a FILE NAME rather than of a directory renders nothing at
// all, because only whole empty segments collapse — the same rule {groupPath} has always followed
// for a core resource.
func TestLocateNew_EmptyFallback_InAFileNameRendersNothing(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{namespace}/{label:team|}-{name}.yaml"}

	res, err := LocateNew(store, policy, labeledConfigMapRequest("cache", nil))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "app/-cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}
