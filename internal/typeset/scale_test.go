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

package typeset

import (
	"reflect"
	"testing"
)

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

func TestSplitFieldPath(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{".spec.replicas", []string{"spec", "replicas"}},
		{"spec.replicas", []string{"spec", "replicas"}},
		{"  .spec.replicas  ", []string{"spec", "replicas"}},
		{".status.replicas", []string{"status", "replicas"}},
		{"", nil},
		{"   ", nil},
		{".", nil},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := SplitFieldPath(tt.path); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitFieldPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
