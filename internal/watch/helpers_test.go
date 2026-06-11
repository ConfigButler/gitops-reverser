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

package watch

import (
	"testing"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestNormalizeResource(t *testing.T) {
	got := normalizeResource("  Deployments  ")
	if got != "deployments" {
		t.Fatalf("normalizeResource returned %q, want %q", got, "deployments")
	}
}

func TestMatchesScope(t *testing.T) {
	// namespaced true matches namespaced scope
	if !matchesScope(true, configv1alpha1.ResourceScopeNamespaced) {
		t.Fatalf("expected namespaced=true to match Namespaced scope")
	}
	// namespaced false matches cluster scope
	if !matchesScope(false, configv1alpha1.ResourceScopeCluster) {
		t.Fatalf("expected namespaced=false to match Cluster scope")
	}
	// namespaced true should not match Cluster scope
	if matchesScope(true, configv1alpha1.ResourceScopeCluster) {
		t.Fatalf("did not expect namespaced=true to match Cluster scope")
	}
}
