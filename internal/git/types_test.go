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

package git

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestBranchKeyString verifies BranchKey string representation.
func TestBranchKeyString(t *testing.T) {
	tests := []struct {
		name     string
		key      BranchKey
		expected string
	}{
		{
			name: "Standard branch key",
			key: BranchKey{
				RepoNamespace: "gitops-system",
				RepoName:      "my-repo",
				Branch:        "main",
			},
			expected: "gitops-system/my-repo/main",
		},
		{
			name: "Development branch",
			key: BranchKey{
				RepoNamespace: "default",
				RepoName:      "test-repo",
				Branch:        "develop",
			},
			expected: "default/test-repo/develop",
		},
		{
			name: "Feature branch",
			key: BranchKey{
				RepoNamespace: "prod",
				RepoName:      "app",
				Branch:        "feature/new-api",
			},
			expected: "prod/app/feature/new-api",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.key.String()
			if result != tt.expected {
				t.Errorf("BranchKey.String() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestBranchKeyEquality verifies BranchKey can be used as map keys.
func TestBranchKeyEquality(t *testing.T) {
	key1 := BranchKey{
		RepoNamespace: "ns1",
		RepoName:      "repo1",
		Branch:        "main",
	}
	key2 := BranchKey{
		RepoNamespace: "ns1",
		RepoName:      "repo1",
		Branch:        "main",
	}
	key3 := BranchKey{
		RepoNamespace: "ns1",
		RepoName:      "repo1",
		Branch:        "develop",
	}

	// Test map key usage
	testMap := make(map[BranchKey]string)
	testMap[key1] = "value1"
	testMap[key2] = "value2" // Should overwrite value1
	testMap[key3] = "value3"

	if len(testMap) != 2 {
		t.Errorf("Expected 2 entries in map (key1==key2), got %d", len(testMap))
	}

	if testMap[key1] != "value2" {
		t.Errorf("key1 should map to 'value2' (overwritten by key2), got %q", testMap[key1])
	}

	if testMap[key3] != "value3" {
		t.Errorf("key3 should map to 'value3', got %q", testMap[key3])
	}
}

// TestEventWithObject verifies event creation with objects.
func TestEventWithObject(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "admin",
			UID:      "12345",
		},
		Path: "clusters/prod",
	}

	if event.Object == nil {
		t.Error("Expected Object to be set")
	}
	if event.Object.GetName() != "test-pod" {
		t.Errorf("Object name = %q, want 'test-pod'", event.Object.GetName())
	}
	if event.Operation != "CREATE" {
		t.Errorf("Operation = %q, want 'CREATE'", event.Operation)
	}
	if event.Path != "clusters/prod" {
		t.Errorf("Path = %q, want 'clusters/prod'", event.Path)
	}
}
