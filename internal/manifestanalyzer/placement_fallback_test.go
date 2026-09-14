// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// newClusterRoleRequest is a cluster-scoped request: the one case that makes {namespace} absent.
func newClusterRoleRequest(name string) PlacementRequest {
	return PlacementRequest{
		Identifier: types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", name),
		Kind:       "ClusterRole",
	}
}

// The point of the change: the bucket a cluster-scoped resource lands in is the author's to name,
// exactly as {label:key|fallback} already let them name the unlabeled one.
func TestLocateNew_NamespaceFallback_ReplacesTheClusterSentinel(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{namespace|_global}/{resource}/{name}.yaml"}

	res, err := LocateNew(store, policy, newClusterRoleRequest("admin"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "_global/clusterroles/admin.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q", res.Path, want)
	}
}

// A fallback is for the ABSENT case only. A namespaced resource has a namespace, so the fallback
// must never displace it — the same rule that keeps a labeled resource out of the unlabeled bucket.
func TestLocateNew_NamespaceFallback_IsNotUsedWhenTheResourceHasANamespace(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{namespace|_global}/{resource}/{name}.yaml"}

	res, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "app/configmaps/cache.yaml"; res.Path != want {
		t.Fatalf("got %q, want %q (a real namespace always wins over the fallback)", res.Path, want)
	}
}

// The declared EMPTY fallback, the one form that renders no segment at all. It is safe HERE for a
// reason that does not hold for a named one: it shortens the path only for cluster-scoped
// resources, and a namespaced resource always renders a non-empty segment in that position, so
// the two can never land on the same path.
func TestLocateNew_EmptyNamespaceFallback_CollapsesOnlyForClusterScoped(t *testing.T) {
	store := placementStore(t, fstest.MapFS{})
	policy := &PlacementPolicy{Default: "{namespace|}/{resource}/{name}.yaml"}

	clusterScoped, err := LocateNew(store, policy, newClusterRoleRequest("admin"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "clusterroles/admin.yaml"; clusterScoped.Path != want {
		t.Fatalf("cluster-scoped: got %q, want %q", clusterScoped.Path, want)
	}

	namespaced, err := LocateNew(store, policy, newConfigMapRequest("cache", "app"))
	if err != nil {
		t.Fatalf("LocateNew: %v", err)
	}
	if want := "app/configmaps/cache.yaml"; namespaced.Path != want {
		t.Fatalf("namespaced: got %q, want %q", namespaced.Path, want)
	}
}

// The namespace position is identity, so its fallback carries one rule a label fallback does not:
// it may not be a name a real namespace could hold, or a cluster-scoped resource would render the
// path a namespaced resource of the same type and name already renders.
func TestValidPlacementTemplateSyntax_NamespaceFallback(t *testing.T) {
	cases := []struct {
		tmpl string
		ok   bool
		why  string
	}{
		{"{namespace|_global}/{name}.yaml", true, "a leading underscore, which no namespace may have"},
		{"{namespace|_cluster}/{name}.yaml", true, "the built-in sentinel, named explicitly"},
		{"{namespace|no.namespace}/{name}.yaml", true, "a dot, which no namespace may contain"},
		{"{namespace|NoNamespace}/{name}.yaml", true, "uppercase, which no namespace may contain"},
		{"{namespace|}/{name}.yaml", true, "an empty fallback: a declared request to collapse the segment"},
		{"{namespace|team-a}/{name}.yaml", false, "a legal namespace name would collide with that namespace"},
		{"{namespace|prod}/{name}.yaml", false, "a legal namespace name, however unlikely to exist"},
		{"{namespace|cluster}/{name}.yaml", false, "\"cluster\" is itself a legal namespace name"},
		{"{namespace|_a/b}/{name}.yaml", false, "a fallback inventing a directory"},
		{"{namespace|..}/{name}.yaml", false, "a bare parent-directory fallback"},
		{"{namespace|_" + strings.Repeat("a", 63) + "}/{name}.yaml", false,
			"longer than the segment ceiling"},
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

// A fallback on a variable that always has a value is a misunderstanding, not a typo, so it is
// refused with the reason rather than accepted as dead syntax that never renders.
func TestValidPlacementTemplateSyntax_FallbackOnlyWhereAVariableCanBeAbsent(t *testing.T) {
	for _, tmpl := range []string{
		"{namespace}/{name|orphan}.yaml",
		"{namespace}/{resource|things}/{name}.yaml",
		"{groupPath|core}/{namespace}/{name}.yaml",
		"{namespace}/{kind|Unknown}/{name}.yaml",
	} {
		err := ValidPlacementTemplateSyntax(tmpl)
		if err == nil {
			t.Errorf("ValidPlacementTemplateSyntax(%q) = nil, want a fallback-not-supported refusal", tmpl)
			continue
		}
		if !strings.Contains(err.Error(), "takes no") {
			t.Errorf("ValidPlacementTemplateSyntax(%q) = %v, want the reason named", tmpl, err)
		}
	}
}

// The refusals an author is most likely to hit must say what to write instead, not "unknown
// variable" — the message that sends a reader hunting for a misspelling that is not there.
func TestValidPlacementTemplateSyntax_FallbackRefusalsExplainThemselves(t *testing.T) {
	err := ValidPlacementTemplateSyntax("{namespace|team-a}/{name}.yaml")
	if err == nil {
		t.Fatal("a colliding namespace fallback must be rejected")
	}
	for _, want := range []string{"legal namespace name", "share a file", "_team-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}

	err = ValidPlacementTemplateSyntax("{namespace|_a b}/{name}.yaml")
	if err == nil {
		t.Fatal("an unsafe fallback must be rejected")
	}
	if !strings.Contains(err.Error(), "one path segment") {
		t.Errorf("error %q does not explain the path-segment charset", err.Error())
	}
}

// A namespace carrying a fallback still discriminates by namespace, so a Secret route that uses
// it is identity-complete. Matching the literal "{namespace}" would have silently refused it.
func TestIdentityCompletePlacementTemplate_NamespaceFallbackStillCounts(t *testing.T) {
	for _, tmpl := range []string{
		"{namespace|_global}/{name}.yaml",
		"{namespace|}/{name}.yaml",
	} {
		if !IdentityCompletePlacementTemplate(tmpl, true) {
			t.Errorf("IdentityCompletePlacementTemplate(%q, true) = false, want true", tmpl)
		}
	}
	if IdentityCompletePlacementTemplate("{label:team|x}/{name}.yaml", true) {
		t.Error("a label fallback is not a namespace and must not satisfy the scope half")
	}
}

// PlacementTemplateLabelKeys feeds the stripped-label gate, so it must still see the key through a
// fallback — a template reading a stripped label is no less broken for naming its own bucket.
func TestPlacementTemplateLabelKeys_SeesKeysThroughAFallback(t *testing.T) {
	got := PlacementTemplateLabelKeys("{label:team|unassigned}/{namespace|_global}/{name}.yaml")
	if len(got) != 1 || got[0] != "team" {
		t.Fatalf("got %v, want [team]", got)
	}
}
