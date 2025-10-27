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

package rulestore

import (
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// TestNewStore verifies that NewStore creates an initialized store.
func TestNewStore(t *testing.T) {
	store := NewStore()
	if store == nil {
		t.Fatal("NewStore returned nil")
	}
	if store.rules == nil {
		t.Error("NewStore did not initialize rules map")
	}
	if store.clusterRules == nil {
		t.Error("NewStore did not initialize clusterRules map")
	}
}

// TestAddOrUpdateWatchRule verifies adding and updating WatchRules.
func TestAddOrUpdateWatchRule(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationCreate},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
				},
			},
		},
	}
	rule.Name = "test-rule"
	rule.Namespace = "default"

	// Add rule
	store.AddOrUpdateWatchRule(rule, "test-repo", "gitops-system", "main", "clusters/prod")

	// Verify it was added
	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiled, exists := store.rules[key]
	if !exists {
		t.Fatal("Rule was not added to store")
	}

	// Verify fields
	if compiled.Source != key {
		t.Errorf("Source mismatch: got %v, want %v", compiled.Source, key)
	}
	if compiled.GitRepoConfigRef != "test-repo" {
		t.Errorf("GitRepoConfigRef mismatch: got %s, want test-repo", compiled.GitRepoConfigRef)
	}
	if compiled.GitRepoConfigNamespace != "gitops-system" {
		t.Errorf("GitRepoConfigNamespace mismatch: got %s, want gitops-system", compiled.GitRepoConfigNamespace)
	}
	if compiled.Branch != "main" {
		t.Errorf("Branch mismatch: got %s, want main", compiled.Branch)
	}
	if compiled.BaseFolder != "clusters/prod" {
		t.Errorf("BaseFolder mismatch: got %s, want clusters/prod", compiled.BaseFolder)
	}
	if compiled.IsClusterScoped {
		t.Error("IsClusterScoped should be false for WatchRule")
	}
	if len(compiled.ResourceRules) != 1 {
		t.Errorf("Expected 1 resource rule, got %d", len(compiled.ResourceRules))
	}

	// Update rule with different values
	store.AddOrUpdateWatchRule(rule, "updated-repo", "gitops-system", "develop", "clusters/staging")

	compiled, exists = store.rules[key]
	if !exists {
		t.Fatal("Rule was deleted instead of updated")
	}
	if compiled.GitRepoConfigRef != "updated-repo" {
		t.Errorf("GitRepoConfigRef not updated: got %s, want updated-repo", compiled.GitRepoConfigRef)
	}
	if compiled.Branch != "develop" {
		t.Errorf("Branch not updated: got %s, want develop", compiled.Branch)
	}
	if compiled.BaseFolder != "clusters/staging" {
		t.Errorf("BaseFolder not updated: got %s, want clusters/staging", compiled.BaseFolder)
	}
}

// TestAddOrUpdateClusterWatchRule verifies adding and updating ClusterWatchRules.
func TestAddOrUpdateClusterWatchRule(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.ClusterWatchRule{
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"nodes"},
					Scope:       configv1alpha1.ResourceScopeCluster,
				},
			},
		},
	}
	rule.Name = "test-cluster-rule"

	// Add rule
	store.AddOrUpdateClusterWatchRule(rule, "cluster-repo", "gitops-system", "main", "cluster/audit")

	// Verify it was added
	key := types.NamespacedName{Name: "test-cluster-rule", Namespace: ""}
	compiled, exists := store.clusterRules[key]
	if !exists {
		t.Fatal("ClusterRule was not added to store")
	}

	// Verify fields
	if compiled.Source != key {
		t.Errorf("Source mismatch: got %v, want %v", compiled.Source, key)
	}
	if compiled.GitRepoConfigRef != "cluster-repo" {
		t.Errorf("GitRepoConfigRef mismatch: got %s, want cluster-repo", compiled.GitRepoConfigRef)
	}
	if compiled.Branch != "main" {
		t.Errorf("Branch mismatch: got %s, want main", compiled.Branch)
	}
	if compiled.BaseFolder != "cluster/audit" {
		t.Errorf("BaseFolder mismatch: got %s, want cluster/audit", compiled.BaseFolder)
	}
	if len(compiled.Rules) != 1 {
		t.Errorf("Expected 1 rule, got %d", len(compiled.Rules))
	}
	if compiled.Rules[0].Scope != configv1alpha1.ResourceScopeCluster {
		t.Errorf("Scope mismatch: got %v, want %v", compiled.Rules[0].Scope, configv1alpha1.ResourceScopeCluster)
	}
}

// TestDelete verifies rule deletion.
func TestDelete(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{Resources: []string{"pods"}},
			},
		},
	}
	rule.Name = "delete-test"
	rule.Namespace = "default"

	key := types.NamespacedName{Name: "delete-test", Namespace: "default"}

	// Add rule
	store.AddOrUpdateWatchRule(rule, "test-repo", "gitops-system", "main", "test")

	// Verify it exists
	if _, exists := store.rules[key]; !exists {
		t.Fatal("Rule was not added")
	}

	// Delete it
	store.Delete(key)

	// Verify it was deleted
	if _, exists := store.rules[key]; exists {
		t.Error("Rule was not deleted")
	}
}

// TestDeleteClusterWatchRule verifies cluster rule deletion.
func TestDeleteClusterWatchRule(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.ClusterWatchRule{
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Resources: []string{"nodes"},
					Scope:     configv1alpha1.ResourceScopeCluster,
				},
			},
		},
	}
	rule.Name = "delete-cluster-test"

	key := types.NamespacedName{Name: "delete-cluster-test", Namespace: ""}

	// Add rule
	store.AddOrUpdateClusterWatchRule(rule, "test-repo", "gitops-system", "main", "test")

	// Verify it exists
	if _, exists := store.clusterRules[key]; !exists {
		t.Fatal("ClusterRule was not added")
	}

	// Delete it
	store.DeleteClusterWatchRule(key)

	// Verify it was deleted
	if _, exists := store.clusterRules[key]; exists {
		t.Error("ClusterRule was not deleted")
	}
}

// TestGetMatchingRules verifies resource matching logic.
func TestGetMatchingRules(t *testing.T) {
	store := NewStore()

	// Add various rules
	rule1 := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationCreate},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
				},
			},
		},
	}
	rule1.Name = "pod-create-rule"
	rule1.Namespace = "default"

	rule2 := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments"},
				},
			},
		},
	}
	rule2.Name = "deployment-rule"
	rule2.Namespace = "default"

	store.AddOrUpdateWatchRule(rule1, "repo1", "gitops-system", "main", "test1")
	store.AddOrUpdateWatchRule(rule2, "repo2", "gitops-system", "main", "test2")

	tests := []struct {
		name            string
		resourcePlural  string
		operation       configv1alpha1.OperationType
		apiGroup        string
		apiVersion      string
		isClusterScoped bool
		expectedCount   int
		expectedNames   []string
	}{
		{
			name:            "Match pod CREATE",
			resourcePlural:  "pods",
			operation:       configv1alpha1.OperationCreate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   1,
			expectedNames:   []string{"pod-create-rule"},
		},
		{
			name:            "No match pod UPDATE",
			resourcePlural:  "pods",
			operation:       configv1alpha1.OperationUpdate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   0,
			expectedNames:   []string{},
		},
		{
			name:            "Match deployment CREATE",
			resourcePlural:  "deployments",
			operation:       configv1alpha1.OperationCreate,
			apiGroup:        "apps",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   1,
			expectedNames:   []string{"deployment-rule"},
		},
		{
			name:            "Match deployment UPDATE (OperationAll)",
			resourcePlural:  "deployments",
			operation:       configv1alpha1.OperationUpdate,
			apiGroup:        "apps",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   1,
			expectedNames:   []string{"deployment-rule"},
		},
		{
			name:            "No match cluster-scoped resource",
			resourcePlural:  "nodes",
			operation:       configv1alpha1.OperationCreate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: true,
			expectedCount:   0,
			expectedNames:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := store.GetMatchingRules(
				nil,
				tt.resourcePlural,
				tt.operation,
				tt.apiGroup,
				tt.apiVersion,
				tt.isClusterScoped,
			)

			if len(matches) != tt.expectedCount {
				t.Errorf("Expected %d matches, got %d", tt.expectedCount, len(matches))
			}

			for _, expectedName := range tt.expectedNames {
				found := false
				for _, match := range matches {
					if match.Source.Name == expectedName {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find rule %s in matches", expectedName)
				}
			}
		})
	}
}

// TestGetMatchingClusterRules verifies cluster rule matching.
func TestGetMatchingClusterRules(t *testing.T) {
	store := NewStore()

	// Cluster-scoped rule
	clusterRule := configv1alpha1.ClusterWatchRule{
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"nodes"},
					Scope:       configv1alpha1.ResourceScopeCluster,
				},
			},
		},
	}
	clusterRule.Name = "node-rule"

	// Namespaced rule in cluster watch rule
	namespacedRule := configv1alpha1.ClusterWatchRule{
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Operations:  []configv1alpha1.OperationType{configv1alpha1.OperationAll},
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
					Scope:       configv1alpha1.ResourceScopeNamespaced,
				},
			},
		},
	}
	namespacedRule.Name = "pod-cluster-rule"

	store.AddOrUpdateClusterWatchRule(clusterRule, "repo1", "gitops-system", "main", "cluster")
	store.AddOrUpdateClusterWatchRule(namespacedRule, "repo2", "gitops-system", "main", "namespaced")

	tests := []struct {
		name            string
		resourcePlural  string
		operation       configv1alpha1.OperationType
		apiGroup        string
		apiVersion      string
		isClusterScoped bool
		expectedCount   int
		expectedNames   []string
	}{
		{
			name:            "Match cluster-scoped nodes",
			resourcePlural:  "nodes",
			operation:       configv1alpha1.OperationCreate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: true,
			expectedCount:   1,
			expectedNames:   []string{"node-rule"},
		},
		{
			name:            "Match namespaced pods via cluster rule",
			resourcePlural:  "pods",
			operation:       configv1alpha1.OperationUpdate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   1,
			expectedNames:   []string{"pod-cluster-rule"},
		},
		{
			name:            "No match: cluster rule doesn't match namespaced scope",
			resourcePlural:  "nodes",
			operation:       configv1alpha1.OperationCreate,
			apiGroup:        "",
			apiVersion:      "v1",
			isClusterScoped: false,
			expectedCount:   0,
			expectedNames:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := store.GetMatchingClusterRules(
				tt.resourcePlural,
				tt.operation,
				tt.apiGroup,
				tt.apiVersion,
				tt.isClusterScoped,
				nil,
			)

			if len(matches) != tt.expectedCount {
				t.Errorf("Expected %d matches, got %d", tt.expectedCount, len(matches))
			}

			for _, expectedName := range tt.expectedNames {
				found := false
				for _, match := range matches {
					if match.Source.Name == expectedName {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected to find rule %s in matches", expectedName)
				}
			}
		})
	}
}

// TestResourceMatching verifies different resource matching patterns.
func TestResourceMatching(t *testing.T) {
	tests := []struct {
		name           string
		ruleResource   string
		resourcePlural string
		shouldMatch    bool
	}{
		{"Exact match", "pods", "pods", true},
		{"Case insensitive match", "pods", "Pods", true},
		{"Case insensitive match 2", "Pods", "pods", true},
		{"Wildcard all", "*", "anything", true},
		{"Subresource wildcard match", "pods/*", "pods/log", true},
		{"Subresource wildcard match 2", "pods/*", "pods/status", true},
		{"Subresource specific match", "pods/log", "pods/log", true},
		{"Subresource no match", "pods/log", "pods/status", false},
		{"No match different resource", "pods", "services", false},
		{"Subresource wildcard no match", "pods/*", "deployments/status", false},
		{"Empty pattern no match", "", "pods", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := CompiledResourceRule{
				Resources: []string{tt.ruleResource},
			}

			result := rule.singleResourceMatches(tt.ruleResource, tt.resourcePlural)
			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v for pattern '%s' against '%s', got %v",
					tt.shouldMatch, tt.ruleResource, tt.resourcePlural, result)
			}
		})
	}
}

// TestOperationMatching verifies operation matching logic.
func TestOperationMatching(t *testing.T) {
	tests := []struct {
		name           string
		ruleOperations []configv1alpha1.OperationType
		operation      configv1alpha1.OperationType
		shouldMatch    bool
	}{
		{"Empty matches all", []configv1alpha1.OperationType{}, configv1alpha1.OperationCreate, true},
		{
			"Wildcard matches all",
			[]configv1alpha1.OperationType{configv1alpha1.OperationAll},
			configv1alpha1.OperationCreate,
			true,
		},
		{
			"Exact match CREATE",
			[]configv1alpha1.OperationType{configv1alpha1.OperationCreate},
			configv1alpha1.OperationCreate,
			true,
		},
		{
			"No match CREATE vs UPDATE",
			[]configv1alpha1.OperationType{configv1alpha1.OperationCreate},
			configv1alpha1.OperationUpdate,
			false,
		},
		{
			"Multiple operations match",
			[]configv1alpha1.OperationType{configv1alpha1.OperationCreate, configv1alpha1.OperationUpdate},
			configv1alpha1.OperationUpdate,
			true,
		},
		{
			"Multiple operations no match",
			[]configv1alpha1.OperationType{configv1alpha1.OperationCreate, configv1alpha1.OperationUpdate},
			configv1alpha1.OperationDelete,
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := CompiledResourceRule{
				Operations: tt.ruleOperations,
			}

			result := rule.matchesOperations(tt.operation)
			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v, got %v", tt.shouldMatch, result)
			}
		})
	}
}

// TestAPIGroupMatching verifies API group matching.
func TestAPIGroupMatching(t *testing.T) {
	tests := []struct {
		name        string
		ruleGroups  []string
		apiGroup    string
		shouldMatch bool
	}{
		{"Empty matches all", []string{}, "apps", true},
		{"Wildcard matches all", []string{"*"}, "apps", true},
		{"Exact match core", []string{""}, "", true},
		{"Exact match apps", []string{"apps"}, "apps", true},
		{"No match", []string{"apps"}, "batch", false},
		{"Multiple groups match", []string{"", "apps"}, "apps", true},
		{"Multiple groups match core", []string{"", "apps"}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := CompiledResourceRule{
				APIGroups: tt.ruleGroups,
			}

			result := rule.matchesAPIGroups(tt.apiGroup)
			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v, got %v", tt.shouldMatch, result)
			}
		})
	}
}

// TestAPIVersionMatching verifies API version matching.
func TestAPIVersionMatching(t *testing.T) {
	tests := []struct {
		name         string
		ruleVersions []string
		apiVersion   string
		shouldMatch  bool
	}{
		{"Empty matches all", []string{}, "v1", true},
		{"Wildcard matches all", []string{"*"}, "v1beta1", true},
		{"Exact match v1", []string{"v1"}, "v1", true},
		{"No match", []string{"v1"}, "v1beta1", false},
		{"Multiple versions match", []string{"v1", "v1beta1"}, "v1beta1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := CompiledResourceRule{
				APIVersions: tt.ruleVersions,
			}

			result := rule.matchesAPIVersions(tt.apiVersion)
			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v, got %v", tt.shouldMatch, result)
			}
		})
	}
}

// TestSnapshotWatchRules verifies snapshot functionality.
func TestSnapshotWatchRules(t *testing.T) {
	store := NewStore()

	rule1 := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{Resources: []string{"pods"}},
			},
		},
	}
	rule1.Name = "rule1"
	rule1.Namespace = "default"

	rule2 := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{Resources: []string{"services"}},
			},
		},
	}
	rule2.Name = "rule2"
	rule2.Namespace = "default"

	store.AddOrUpdateWatchRule(rule1, "repo1", "gitops-system", "main", "test1")
	store.AddOrUpdateWatchRule(rule2, "repo2", "gitops-system", "main", "test2")

	// Get snapshot
	snapshot := store.SnapshotWatchRules()

	if len(snapshot) != 2 {
		t.Errorf("Expected 2 rules in snapshot, got %d", len(snapshot))
	}

	// Verify snapshot is independent (deep copy)
	snapshot[0].Branch = "modified"

	// Get another snapshot to verify original wasn't modified
	snapshot2 := store.SnapshotWatchRules()
	for _, rule := range snapshot2 {
		if rule.Branch == "modified" {
			t.Error("Snapshot modification affected original store")
		}
	}
}

// TestSnapshotClusterWatchRules verifies cluster rule snapshot.
func TestSnapshotClusterWatchRules(t *testing.T) {
	store := NewStore()

	rule1 := configv1alpha1.ClusterWatchRule{
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Resources: []string{"nodes"},
					Scope:     configv1alpha1.ResourceScopeCluster,
				},
			},
		},
	}
	rule1.Name = "cluster-rule1"

	store.AddOrUpdateClusterWatchRule(rule1, "repo1", "gitops-system", "main", "cluster")

	snapshot := store.SnapshotClusterWatchRules()

	if len(snapshot) != 1 {
		t.Errorf("Expected 1 rule in snapshot, got %d", len(snapshot))
	}

	// Verify deep copy
	snapshot[0].Branch = "modified"
	snapshot2 := store.SnapshotClusterWatchRules()
	if snapshot2[0].Branch == "modified" {
		t.Error("Snapshot modification affected original store")
	}
}

// TestConcurrentAccess verifies thread safety.
func TestConcurrentAccess(t *testing.T) {
	store := NewStore()

	var wg sync.WaitGroup
	numGoroutines := 10
	numOperations := 100

	// Concurrent writes
	wg.Add(numGoroutines)
	for i := range numGoroutines {
		go func(_ int) {
			defer wg.Done()
			for range numOperations {
				rule := configv1alpha1.WatchRule{
					Spec: configv1alpha1.WatchRuleSpec{
						Rules: []configv1alpha1.ResourceRule{
							{Resources: []string{"pods"}},
						},
					},
				}
				rule.Name = "concurrent-rule"
				rule.Namespace = "default"

				store.AddOrUpdateWatchRule(rule, "repo", "gitops-system", "main", "test")
			}
		}(i)
	}

	// Concurrent reads
	wg.Add(numGoroutines)
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for range numOperations {
				_ = store.SnapshotWatchRules()
				_ = store.GetMatchingRules(nil, "pods", configv1alpha1.OperationCreate, "", "v1", false)
			}
		}()
	}

	wg.Wait()

	// Verify store is still consistent
	snapshot := store.SnapshotWatchRules()
	if len(snapshot) != 1 {
		t.Errorf("Expected 1 rule after concurrent operations, got %d", len(snapshot))
	}
}

// TestMultipleResourceRules verifies handling of multiple resource rules in a single WatchRule.
func TestMultipleResourceRules(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		Spec: configv1alpha1.WatchRuleSpec{
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations: []configv1alpha1.OperationType{configv1alpha1.OperationCreate},
					Resources:  []string{"pods"},
				},
				{
					Operations: []configv1alpha1.OperationType{configv1alpha1.OperationUpdate},
					Resources:  []string{"services"},
				},
			},
		},
	}
	rule.Name = "multi-rule"
	rule.Namespace = "default"

	store.AddOrUpdateWatchRule(rule, "repo", "gitops-system", "main", "test")

	// Should match pod CREATE
	matches := store.GetMatchingRules(nil, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	if len(matches) != 1 {
		t.Errorf("Expected to match pod CREATE, got %d matches", len(matches))
	}

	// Should match service UPDATE
	matches = store.GetMatchingRules(nil, "services", configv1alpha1.OperationUpdate, "", "v1", false)
	if len(matches) != 1 {
		t.Errorf("Expected to match service UPDATE, got %d matches", len(matches))
	}

	// Should NOT match pod UPDATE
	matches = store.GetMatchingRules(nil, "pods", configv1alpha1.OperationUpdate, "", "v1", false)
	if len(matches) != 0 {
		t.Errorf("Should not match pod UPDATE, got %d matches", len(matches))
	}
}
