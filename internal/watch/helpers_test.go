// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

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
