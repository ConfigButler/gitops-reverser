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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestSingleConcrete(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		wantOK  bool
		wantVal string
	}{
		{name: "single value", in: []string{"apps"}, wantOK: true, wantVal: "apps"},
		{name: "star wildcard", in: []string{"*"}, wantOK: false},
		{name: "empty slice", in: []string{}, wantOK: false},
		// Empty string is valid (represents core API group)
		{name: "empty string for core API", in: []string{""}, wantOK: true, wantVal: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := singleConcrete(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("singleConcrete(%v) ok=%v, want %v", tt.in, ok, tt.wantOK)
			}
			if ok {
				if len(got) != 1 || got[0] != tt.wantVal {
					t.Fatalf("singleConcrete(%v) got=%v, want [%s]", tt.in, got, tt.wantVal)
				}
			}
		})
	}
}

func TestNormalizeResource(t *testing.T) {
	got := normalizeResource("  Deployments  ")
	if got != "deployments" {
		t.Fatalf("normalizeResource returned %q, want %q", got, "deployments")
	}
}

func TestAddGVR_Dedup(t *testing.T) {
	seen := make(map[string]struct{})
	var out []GVR

	addGVR("apps", "v1", "deployments", configv1alpha1.ResourceScopeNamespaced, &out, seen)
	addGVR("apps", "v1", "deployments", configv1alpha1.ResourceScopeNamespaced, &out, seen)

	if len(out) != 1 {
		t.Fatalf("expected 1 unique GVR after dedup, got %d", len(out))
	}

	addGVR("apps", "v1", "statefulsets", configv1alpha1.ResourceScopeNamespaced, &out, seen)
	if len(out) != 2 {
		t.Fatalf("expected 2 unique GVRs, got %d", len(out))
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

func TestKeyFunction(t *testing.T) {
	got := key("apps", "v1", "deployments")
	want := "apps|v1|deployments"
	if got != want {
		t.Fatalf("key() = %q, want %q", got, want)
	}
}

func TestToUnstructuredFromInformer(t *testing.T) {
	// Direct Unstructured
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetName("cm1")
	if got := toUnstructuredFromInformer(u); got == nil {
		t.Fatalf("expected non-nil for direct *unstructured.Unstructured")
	}

	// DeletedFinalStateUnknown (value)
	dfsu := cache.DeletedFinalStateUnknown{Obj: u}
	if got := toUnstructuredFromInformer(dfsu); got == nil {
		t.Fatalf("expected non-nil for DeletedFinalStateUnknown value")
	}

	// DeletedFinalStateUnknown (pointer)
	dfsuPtr := &cache.DeletedFinalStateUnknown{Obj: u}
	if got := toUnstructuredFromInformer(dfsuPtr); got == nil {
		t.Fatalf("expected non-nil for *DeletedFinalStateUnknown pointer")
	}

	// Typed object convertible to Unstructured
	cm := &corev1.ConfigMap{}
	cm.APIVersion = "v1"
	cm.Kind = "ConfigMap"
	cm.Name = "cm-typed"
	if got := toUnstructuredFromInformer(cm); got == nil {
		t.Fatalf("expected non-nil for typed object convertible to Unstructured")
	}
}
