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

package auditutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsScaleSubresource(t *testing.T) {
	tests := []struct {
		name        string
		subresource string
		isScale     bool
	}{
		{"scale is the one supported subresource", "scale", true},
		{"status is not scale", "status", false},
		{"exec is not scale", "exec", false},
		{"empty subresource is not scale", "", false},
		{"an arbitrary subresource is not scale", "throttle", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.isScale, IsScaleSubresource(tt.subresource))
		})
	}
}

func TestBuiltinScaleReplicasPath(t *testing.T) {
	tests := []struct {
		name     string
		group    string
		resource string
		wantPath []string
		wantOK   bool
	}{
		{"apps deployments", "apps", "deployments", []string{"spec", "replicas"}, true},
		{"apps statefulsets", "apps", "statefulsets", []string{"spec", "replicas"}, true},
		{"apps replicasets", "apps", "replicasets", []string{"spec", "replicas"}, true},
		{"core replicationcontrollers", "", "replicationcontrollers", []string{"spec", "replicas"}, true},
		{"a CRD scalable type has no known path", "example.com", "widgets", nil, false},
		{"an aggregated API scalable type has no known path", "metrics.k8s.io", "things", nil, false},
		{"deployments outside the apps group are not built-in", "extensions", "deployments", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ok := BuiltinScaleReplicasPath(tt.group, tt.resource)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPath, path)
		})
	}
}

// The returned path is a copy: mutating it must not corrupt the shared policy for the
// next caller.
func TestBuiltinScaleReplicasPath_ReturnsCopy(t *testing.T) {
	first, ok := BuiltinScaleReplicasPath("apps", "deployments")
	assert.True(t, ok)
	first[0] = "mutated"

	second, ok := BuiltinScaleReplicasPath("apps", "deployments")
	assert.True(t, ok)
	assert.Equal(t, []string{"spec", "replicas"}, second, "the shared policy must be immutable")
}
