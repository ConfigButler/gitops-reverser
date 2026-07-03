// SPDX-License-Identifier: Apache-2.0

package typeset

import "testing"

func TestBuiltinScale_Scalable(t *testing.T) {
	scalable := []struct{ group, resource string }{
		{"apps", "deployments"},
		{"apps", "statefulsets"},
		{"apps", "replicasets"},
		{"", "replicationcontrollers"},
	}
	for _, tt := range scalable {
		t.Run(tt.group+"/"+tt.resource, func(t *testing.T) {
			binding, ok := BuiltinScale(tt.group, tt.resource)
			if !ok {
				t.Fatalf("BuiltinScale(%q,%q) should be a built-in scalable type", tt.group, tt.resource)
			}
			assertBuiltinScaleBinding(t, binding)
		})
	}
}

func assertBuiltinScaleBinding(t *testing.T, binding ScaleBinding) {
	t.Helper()
	if !binding.Enabled || !binding.Usable {
		t.Errorf("built-in scalable binding should be enabled and usable: %+v", binding)
	}
	if binding.Source != ScaleSourceBuiltinRegistry {
		t.Errorf("source = %q, want %q", binding.Source, ScaleSourceBuiltinRegistry)
	}
	if binding.SpecReplicasPath != ".spec.replicas" {
		t.Errorf("specReplicasPath = %q, want .spec.replicas", binding.SpecReplicasPath)
	}
	if binding.ResponseGVK.Kind != "Scale" || binding.ResponseGVK.Group != "autoscaling" {
		t.Errorf("responseGVK = %+v, want autoscaling/v1 Scale", binding.ResponseGVK)
	}
}

func TestBuiltinScale_NotScalable(t *testing.T) {
	notScalable := []struct{ group, resource string }{
		{"apps", "daemonsets"},     // not scalable via the standardized path
		{"batch", "jobs"},          // not scalable
		{"example.com", "widgets"}, // CRD: resolved from the CRD, not here
		{"metrics.k8s.io", "pods"}, // aggregated: no generic path
	}
	for _, tt := range notScalable {
		t.Run(tt.group+"/"+tt.resource, func(t *testing.T) {
			binding, ok := BuiltinScale(tt.group, tt.resource)
			if ok {
				t.Fatalf("BuiltinScale(%q,%q) should not be a built-in scalable type", tt.group, tt.resource)
			}
			if binding != (ScaleBinding{}) {
				t.Errorf("non-scalable should return zero binding, got %+v", binding)
			}
		})
	}
}
