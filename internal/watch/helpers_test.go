// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ownNamespaceScope is the resolved scope a WatchRule whose items set no sourceNamespace compiles
// to: every item watches the rule's OWN namespace. Tests that are not about the source-namespace
// gate use it so the compiled rule looks exactly as CompileWatchRule would leave it.
func ownNamespaceScope(rule configv1alpha3.WatchRule) [][]string {
	out := make([][]string, len(rule.Spec.Rules))
	for i := range rule.Spec.Rules {
		out[i] = []string{rule.Spec.Rules[i].EffectiveSourceNamespace(rule.Namespace)}
	}
	return out
}

// itemScope is the resolved scope for a rule whose single item watches the given namespaces —
// the shape a `sourceNamespace: "*"` item compiles to once expanded.
func itemScope(namespaces ...string) [][]string {
	return [][]string{namespaces}
}

func TestNormalizeResource(t *testing.T) {
	got := normalizeResource("  Deployments  ")
	if got != "deployments" {
		t.Fatalf("normalizeResource returned %q, want %q", got, "deployments")
	}
}

func TestMatchesScope(t *testing.T) {
	// namespaced true matches namespaced scope
	if !matchesScope(true, configv1alpha3.ResourceScopeNamespaced) {
		t.Fatalf("expected namespaced=true to match Namespaced scope")
	}
	// namespaced false matches cluster scope
	if !matchesScope(false, configv1alpha3.ResourceScopeCluster) {
		t.Fatalf("expected namespaced=false to match Cluster scope")
	}
	// namespaced true should not match Cluster scope
	if matchesScope(true, configv1alpha3.ResourceScopeCluster) {
		t.Fatalf("did not expect namespaced=true to match Cluster scope")
	}
}
