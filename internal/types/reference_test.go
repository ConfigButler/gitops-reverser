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

package types

import (
	"testing"
)

func TestNewResourceReference(t *testing.T) {
	tests := []struct {
		name              string
		inputName         string
		inputNamespace    string
		expectedName      string
		expectedNamespace string
	}{
		{
			name:              "normal reference",
			inputName:         "my-gitdest",
			inputNamespace:    "default",
			expectedName:      "my-gitdest",
			expectedNamespace: "default",
		},
		{
			name:              "empty name",
			inputName:         "",
			inputNamespace:    "default",
			expectedName:      "",
			expectedNamespace: "default",
		},
		{
			name:              "empty namespace",
			inputName:         "my-gitdest",
			inputNamespace:    "",
			expectedName:      "my-gitdest",
			expectedNamespace: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := NewResourceReference(tt.inputName, tt.inputNamespace)
			if ref.Name != tt.expectedName {
				t.Errorf("Name = %q, want %q", ref.Name, tt.expectedName)
			}
			if ref.Namespace != tt.expectedNamespace {
				t.Errorf("Namespace = %q, want %q", ref.Namespace, tt.expectedNamespace)
			}
		})
	}
}

func TestResourceReference_String(t *testing.T) {
	tests := []struct {
		name     string
		ref      ResourceReference
		expected string
	}{
		{
			name:     "normal reference",
			ref:      ResourceReference{Name: "my-gitdest", Namespace: "default"},
			expected: "default/my-gitdest",
		},
		{
			name:     "different namespace",
			ref:      ResourceReference{Name: "gitdest-2", Namespace: "kube-system"},
			expected: "kube-system/gitdest-2",
		},
		{
			name:     "empty name",
			ref:      ResourceReference{Name: "", Namespace: "default"},
			expected: "default/",
		},
		{
			name:     "empty namespace",
			ref:      ResourceReference{Name: "my-gitdest", Namespace: ""},
			expected: "/my-gitdest",
		},
		{
			name:     "both empty",
			ref:      ResourceReference{Name: "", Namespace: ""},
			expected: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.ref.String()
			if result != tt.expected {
				t.Errorf("String() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestResourceReference_Key(t *testing.T) {
	tests := []struct {
		name     string
		ref      ResourceReference
		expected string
	}{
		{
			name:     "normal reference",
			ref:      ResourceReference{Name: "my-gitdest", Namespace: "default"},
			expected: "default/my-gitdest",
		},
		{
			name:     "key should match String",
			ref:      ResourceReference{Name: "gitdest-2", Namespace: "kube-system"},
			expected: "kube-system/gitdest-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.ref.Key()
			if result != tt.expected {
				t.Errorf("Key() = %q, want %q", result, tt.expected)
			}
			// Verify Key() matches String()
			if result != tt.ref.String() {
				t.Errorf("Key() = %q does not match String() = %q", result, tt.ref.String())
			}
		})
	}
}

func TestResourceReference_Equal(t *testing.T) {
	tests := []struct {
		name     string
		ref1     ResourceReference
		ref2     ResourceReference
		expected bool
	}{
		{
			name:     "equal references",
			ref1:     ResourceReference{Name: "my-gitdest", Namespace: "default"},
			ref2:     ResourceReference{Name: "my-gitdest", Namespace: "default"},
			expected: true,
		},
		{
			name:     "different names",
			ref1:     ResourceReference{Name: "gitdest-1", Namespace: "default"},
			ref2:     ResourceReference{Name: "gitdest-2", Namespace: "default"},
			expected: false,
		},
		{
			name:     "different namespaces",
			ref1:     ResourceReference{Name: "my-gitdest", Namespace: "default"},
			ref2:     ResourceReference{Name: "my-gitdest", Namespace: "kube-system"},
			expected: false,
		},
		{
			name:     "both different",
			ref1:     ResourceReference{Name: "gitdest-1", Namespace: "default"},
			ref2:     ResourceReference{Name: "gitdest-2", Namespace: "kube-system"},
			expected: false,
		},
		{
			name:     "both empty",
			ref1:     ResourceReference{Name: "", Namespace: ""},
			ref2:     ResourceReference{Name: "", Namespace: ""},
			expected: true,
		},
		{
			name:     "one empty name",
			ref1:     ResourceReference{Name: "", Namespace: "default"},
			ref2:     ResourceReference{Name: "my-gitdest", Namespace: "default"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.ref1.Equal(tt.ref2)
			if result != tt.expected {
				t.Errorf("Equal() = %v, want %v", result, tt.expected)
			}
			// Verify symmetry
			result2 := tt.ref2.Equal(tt.ref1)
			if result != result2 {
				t.Errorf("Equal() is not symmetric: ref1.Equal(ref2) = %v, ref2.Equal(ref1) = %v", result, result2)
			}
		})
	}
}

func TestResourceReference_IsZero(t *testing.T) {
	tests := []struct {
		name     string
		ref      ResourceReference
		expected bool
	}{
		{
			name:     "normal reference",
			ref:      ResourceReference{Name: "my-gitdest", Namespace: "default"},
			expected: false,
		},
		{
			name:     "empty reference",
			ref:      ResourceReference{Name: "", Namespace: ""},
			expected: true,
		},
		{
			name:     "only name",
			ref:      ResourceReference{Name: "my-gitdest", Namespace: ""},
			expected: false,
		},
		{
			name:     "only namespace",
			ref:      ResourceReference{Name: "", Namespace: "default"},
			expected: false,
		},
		{
			name:     "zero value",
			ref:      ResourceReference{},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.ref.IsZero()
			if result != tt.expected {
				t.Errorf("IsZero() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestResourceReference_AsMapKey(t *testing.T) {
	// Test that ResourceReference can be used as a map key via Key() method
	m := make(map[string]int)

	ref1 := ResourceReference{Name: "gitdest-1", Namespace: "default"}
	ref2 := ResourceReference{Name: "gitdest-2", Namespace: "default"}
	ref3 := ResourceReference{Name: "gitdest-1", Namespace: "kube-system"}
	ref4 := ResourceReference{Name: "gitdest-1", Namespace: "default"} // Same as ref1

	m[ref1.Key()] = 1
	m[ref2.Key()] = 2
	m[ref3.Key()] = 3

	// Verify distinct keys
	if len(m) != 3 {
		t.Errorf("Map should have 3 entries, got %d", len(m))
	}

	// Verify lookup
	if val, ok := m[ref1.Key()]; !ok || val != 1 {
		t.Errorf("ref1 lookup failed: got %v, %v", val, ok)
	}

	// Verify ref4 (equal to ref1) returns same value
	if val, ok := m[ref4.Key()]; !ok || val != 1 {
		t.Errorf("ref4 (equal to ref1) lookup failed: got %v, %v", val, ok)
	}
}
