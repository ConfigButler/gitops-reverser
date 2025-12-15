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

/*
Package rulestore manages the in-memory cache of compiled WatchRule configurations.
It provides efficient lookup and matching of Kubernetes resources against active watch rules.
*/
package rulestore

import (
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// CompiledRule represents a fully processed WatchRule, ready for quick lookups.
type CompiledRule struct {
	// Source is the NamespacedName of the WatchRule CR.
	Source types.NamespacedName

	// GitTarget reference (for event routing)
	GitTargetRef       string
	GitTargetNamespace string

	// Resolved values (from GitTarget)
	GitProviderRef       string
	GitProviderNamespace string
	Branch               string
	Path                 string

	// IsClusterScoped indicates if this rule watches cluster-scoped resources.
	// Always false for WatchRule (namespace-scoped).
	IsClusterScoped bool
	// ResourceRules contains the compiled resource matching rules.
	ResourceRules []CompiledResourceRule
}

// CompiledResourceRule represents a single resource matching rule with all its filters.
type CompiledResourceRule struct {
	// Operations specifies which operations trigger this rule.
	Operations []configv1alpha1.OperationType
	// APIGroups specifies which API groups this rule matches.
	APIGroups []string
	// APIVersions specifies which API versions this rule matches.
	APIVersions []string
	// Resources specifies which resource types this rule matches.
	Resources []string
}

// CompiledClusterRule represents a fully processed ClusterWatchRule, ready for quick lookups.
type CompiledClusterRule struct {
	// Source is the NamespacedName of the ClusterWatchRule CR (namespace will be empty).
	Source types.NamespacedName

	// GitTarget reference (for event routing)
	GitTargetRef       string
	GitTargetNamespace string

	// Resolved values (from GitTarget)
	GitProviderRef       string
	GitProviderNamespace string
	Branch               string
	Path                 string

	// Rules contains the compiled cluster resource rules with per-rule scope.
	Rules []CompiledClusterResourceRule
}

// CompiledClusterResourceRule represents a single cluster resource rule with scope.
type CompiledClusterResourceRule struct {
	// Operations specifies which operations trigger this rule.
	Operations []configv1alpha1.OperationType
	// APIGroups specifies which API groups this rule matches.
	APIGroups []string
	// APIVersions specifies which API versions this rule matches.
	APIVersions []string
	// Resources specifies which resource types this rule matches.
	Resources []string
	// Scope indicates whether this rule watches Cluster or Namespaced resources.
	Scope configv1alpha1.ResourceScope
}

// RuleStore holds the in-memory representation of all active watch rules.
// It is safe for concurrent use.
type RuleStore struct {
	mu           sync.RWMutex
	rules        map[types.NamespacedName]CompiledRule
	clusterRules map[types.NamespacedName]CompiledClusterRule
}

// NewStore creates a new, empty RuleStore.
func NewStore() *RuleStore {
	return &RuleStore{
		rules:        make(map[types.NamespacedName]CompiledRule),
		clusterRules: make(map[types.NamespacedName]CompiledClusterRule),
	}
}

// AddOrUpdateWatchRule adds or updates a WatchRule with a resolved target from GitTarget.
// The chain is: WatchRule -> GitTarget -> GitProvider
// Parameters:
//   - rule: the WatchRule to add or update
//   - gitTargetName: the name of the GitTarget
//   - gitTargetNamespace: the namespace containing the GitTarget
//   - gitProviderName: the name of the resolved GitProvider (from GitTarget.Spec.Provider)
//   - gitProviderNamespace: the namespace containing the resolved GitProvider
//   - branch: the Git branch to write to (from GitTarget.Spec.Branch)
//   - path: POSIX-like relative path prefix for writes (from GitTarget.Spec.Path, sanitized upstream)
func (s *RuleStore) AddOrUpdateWatchRule(
	rule configv1alpha1.WatchRule,
	gitTargetName string,
	gitTargetNamespace string,
	gitProviderName string,
	gitProviderNamespace string,
	branch string,
	path string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{
		Name:      rule.Name,
		Namespace: rule.Namespace,
	}

	compiled := CompiledRule{
		Source:               key,
		GitTargetRef:         gitTargetName,
		GitTargetNamespace:   gitTargetNamespace,
		GitProviderRef:       gitProviderName,
		GitProviderNamespace: gitProviderNamespace,
		Branch:               branch,
		Path:                 path,
		IsClusterScoped:      false,
		ResourceRules:        make([]CompiledResourceRule, 0, len(rule.Spec.Rules)),
	}

	for _, r := range rule.Spec.Rules {
		compiled.ResourceRules = append(compiled.ResourceRules, CompiledResourceRule{
			Operations:  r.Operations,
			APIGroups:   r.APIGroups,
			APIVersions: r.APIVersions,
			Resources:   r.Resources,
		})
	}

	s.rules[key] = compiled
}

// AddOrUpdateClusterWatchRule adds or updates a ClusterWatchRule with a resolved target from GitTarget.
// The chain is: ClusterWatchRule -> GitTarget -> GitProvider
// Parameters:
//   - rule: the ClusterWatchRule to add or update
//   - gitTargetName: the name of the GitTarget
//   - gitTargetNamespace: the namespace containing the GitTarget
//   - gitProviderName: the name of the resolved GitProvider (from GitTarget.Spec.Provider)
//   - gitProviderNamespace: the namespace containing the resolved GitProvider
//   - branch: the Git branch to write to (from GitTarget.Spec.Branch)
//   - path: POSIX-like relative path prefix for writes (from GitTarget.Spec.Path, sanitized upstream)
func (s *RuleStore) AddOrUpdateClusterWatchRule(
	rule configv1alpha1.ClusterWatchRule,
	gitTargetName string,
	gitTargetNamespace string,
	gitProviderName string,
	gitProviderNamespace string,
	branch string,
	path string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{
		Name:      rule.Name,
		Namespace: "", // cluster-scoped
	}

	compiled := CompiledClusterRule{
		Source:               key,
		GitTargetRef:         gitTargetName,
		GitTargetNamespace:   gitTargetNamespace,
		GitProviderRef:       gitProviderName,
		GitProviderNamespace: gitProviderNamespace,
		Branch:               branch,
		Path:                 path,
		Rules:                make([]CompiledClusterResourceRule, 0, len(rule.Spec.Rules)),
	}

	for _, r := range rule.Spec.Rules {
		compiled.Rules = append(compiled.Rules, CompiledClusterResourceRule{
			Operations:  r.Operations,
			APIGroups:   r.APIGroups,
			APIVersions: r.APIVersions,
			Resources:   r.Resources,
			Scope:       r.Scope,
		})
	}

	s.clusterRules[key] = compiled
}

// Delete removes a rule from the store.
func (s *RuleStore) Delete(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, key)
}

// DeleteClusterWatchRule removes a ClusterWatchRule from the store.
func (s *RuleStore) DeleteClusterWatchRule(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clusterRules, key)
}

// GetMatchingRules returns all rules that match the given resource with enhanced filtering.
// Parameters:
//   - obj: The Kubernetes object to match (unused now; kept for signature stability)
//   - resourcePlural: The plural form of the resource (e.g., "pods", "deployments")
//   - operation: The operation type (CREATE, UPDATE, DELETE)
//   - apiGroup: The API group of the resource (empty string for core API)
//   - apiVersion: The API version of the resource
//   - isClusterScoped: Whether the resource is cluster-scoped
func (s *RuleStore) GetMatchingRules(
	_ client.Object,
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
) []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchingRules []CompiledRule
	for _, rule := range s.rules {
		// First check: Does rule scope match resource scope?
		if rule.IsClusterScoped != isClusterScoped {
			continue // WatchRule can't match cluster resources
		}

		// For namespace-scoped rules, check namespace match
		// Note: WatchRule only watches its own namespace; caller is responsible for namespace scoping
		// based on Source.Namespace when using this result.

		if rule.matches(resourcePlural, operation, apiGroup, apiVersion) {
			matchingRules = append(matchingRules, rule)
		}
	}
	return matchingRules
}

// GetMatchingClusterRules returns ClusterWatchRules matching the resource.
// This handles both cluster-scoped and namespaced resources with per-rule scope matching.
// Parameters:
//   - resourcePlural: The plural form of the resource (e.g., "nodes", "pods")
//   - operation: The operation type (CREATE, UPDATE, DELETE)
//   - apiGroup: The API group of the resource (empty string for core API)
//   - apiVersion: The API version of the resource
//   - isClusterScoped: Whether the resource is cluster-scoped
//   - namespaceLabels: Labels of the namespace (ignored in simplified MVP)
func (s *RuleStore) GetMatchingClusterRules(
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
	_ map[string]string,
) []CompiledClusterRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchingRules []CompiledClusterRule
	for _, clusterRule := range s.clusterRules {
		if s.clusterRuleMatches(
			clusterRule,
			resourcePlural,
			operation,
			apiGroup,
			apiVersion,
			isClusterScoped,
		) {
			matchingRules = append(matchingRules, clusterRule)
		}
	}
	return matchingRules
}

// clusterRuleMatches checks if a cluster rule matches the given criteria.
func (s *RuleStore) clusterRuleMatches(
	clusterRule CompiledClusterRule,
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
	isClusterScoped bool,
) bool {
	for _, rule := range clusterRule.Rules {
		if s.ruleMatchesScope(rule, isClusterScoped) &&
			rule.matchesCluster(resourcePlural, operation, apiGroup, apiVersion) {
			return true
		}
	}
	return false
}

// ruleMatchesScope checks if a rule's scope matches the resource scope.
// Simplified MVP: Namespaced scope matches all namespaces (no NamespaceSelector).
func (s *RuleStore) ruleMatchesScope(
	rule CompiledClusterResourceRule,
	isClusterScoped bool,
) bool {
	// For cluster-scoped resources, only match Cluster scope rules
	if isClusterScoped {
		return rule.Scope == configv1alpha1.ResourceScopeCluster
	}

	// For namespaced resources, match Namespaced scope (all namespaces)
	return rule.Scope == configv1alpha1.ResourceScopeNamespaced
}

// matches checks if a single rule matches the given filters.
func (r *CompiledRule) matches(
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	// Check if any resource rule matches (logical OR)
	for _, rule := range r.ResourceRules {
		if rule.matches(resourcePlural, operation, apiGroup, apiVersion) {
			return true
		}
	}

	return false
}

// matches checks if a resource rule matches the given filters.
func (r *CompiledResourceRule) matches(
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	// Match operations (empty = match all)
	if !r.matchesOperations(operation) {
		return false
	}

	// Match API groups (empty = match all)
	if !r.matchesAPIGroups(apiGroup) {
		return false
	}

	// Match API versions (empty = match all)
	if !r.matchesAPIVersions(apiVersion) {
		return false
	}

	// Match resource plural (required)
	return r.resourceMatches(resourcePlural)
}

// matchesOperations checks if the operation matches any in the rule.
func (r *CompiledResourceRule) matchesOperations(operation configv1alpha1.OperationType) bool {
	if len(r.Operations) == 0 {
		return true // Empty = match all
	}

	for _, op := range r.Operations {
		if op == configv1alpha1.OperationAll || op == operation {
			return true
		}
	}
	return false
}

// matchesAPIGroups checks if the API group matches any in the rule.
func (r *CompiledResourceRule) matchesAPIGroups(apiGroup string) bool {
	if len(r.APIGroups) == 0 {
		return true // Empty = match all
	}

	for _, group := range r.APIGroups {
		if group == "*" || group == apiGroup {
			return true
		}
	}
	return false
}

// matchesAPIVersions checks if the API version matches any in the rule.
func (r *CompiledResourceRule) matchesAPIVersions(apiVersion string) bool {
	if len(r.APIVersions) == 0 {
		return true // Empty = match all
	}

	for _, version := range r.APIVersions {
		if version == "*" || version == apiVersion {
			return true
		}
	}
	return false
}

// resourceMatches checks if the resource plural matches any of the rule patterns.
func (r *CompiledResourceRule) resourceMatches(resourcePlural string) bool {
	for _, ruleResource := range r.Resources {
		if r.singleResourceMatches(ruleResource, resourcePlural) {
			return true
		}
	}
	return false
}

// singleResourceMatches checks if a single rule pattern matches the given resource plural.
// Supports:
//   - "*" - matches all resources
//   - "pods" - exact match (case-insensitive)
//   - "pods/*" - matches all pod subresources (pods/log, pods/status, etc.)
//   - "pods/log" - matches specific subresource
//
// Does NOT support:
//   - Prefix wildcards: "pod*" (removed per enhancement plan)
//   - Suffix wildcards: "*.example.com" (removed per enhancement plan)
func (r *CompiledResourceRule) singleResourceMatches(ruleResource, resourcePlural string) bool {
	if ruleResource == "" {
		return false
	}

	// Match wildcard for all resources
	if ruleResource == "*" {
		return true
	}

	// Exact match (case-insensitive)
	if strings.EqualFold(ruleResource, resourcePlural) {
		return true
	}

	// Subresource wildcard: "pods/*" matches "pods/log", "pods/status", etc.
	if strings.HasSuffix(ruleResource, "/*") {
		prefix := ruleResource[:len(ruleResource)-2] // Remove "/*"
		return strings.HasPrefix(strings.ToLower(resourcePlural), strings.ToLower(prefix)+"/")
	}

	return false
}

// matchesCluster checks if a cluster resource rule matches the given filters.
func (r *CompiledClusterResourceRule) matchesCluster(
	resourcePlural string,
	operation configv1alpha1.OperationType,
	apiGroup string,
	apiVersion string,
) bool {
	// Match operations (empty = match all)
	if !r.matchesOperations(operation) {
		return false
	}

	// Match API groups (empty = match all)
	if !r.matchesAPIGroups(apiGroup) {
		return false
	}

	// Match API versions (empty = match all)
	if !r.matchesAPIVersions(apiVersion) {
		return false
	}

	// Match resource plural (required)
	return r.resourceMatches(resourcePlural)
}

// matchesOperations checks if the operation matches any in the rule.
func (r *CompiledClusterResourceRule) matchesOperations(operation configv1alpha1.OperationType) bool {
	if len(r.Operations) == 0 {
		return true // Empty = match all
	}

	for _, op := range r.Operations {
		if op == configv1alpha1.OperationAll || op == operation {
			return true
		}
	}
	return false
}

// matchesAPIGroups checks if the API group matches any in the rule.
func (r *CompiledClusterResourceRule) matchesAPIGroups(apiGroup string) bool {
	if len(r.APIGroups) == 0 {
		return true // Empty = match all
	}

	for _, group := range r.APIGroups {
		if group == "*" || group == apiGroup {
			return true
		}
	}
	return false
}

// matchesAPIVersions checks if the API version matches any in the rule.
func (r *CompiledClusterResourceRule) matchesAPIVersions(apiVersion string) bool {
	if len(r.APIVersions) == 0 {
		return true // Empty = match all
	}

	for _, version := range r.APIVersions {
		if version == "*" || version == apiVersion {
			return true
		}
	}
	return false
}

// resourceMatches checks if the resource plural matches any of the rule patterns.
func (r *CompiledClusterResourceRule) resourceMatches(resourcePlural string) bool {
	for _, ruleResource := range r.Resources {
		if r.singleResourceMatches(ruleResource, resourcePlural) {
			return true
		}
	}
	return false
}

// singleResourceMatches checks if a single rule pattern matches the given resource plural.
func (r *CompiledClusterResourceRule) singleResourceMatches(ruleResource, resourcePlural string) bool {
	if ruleResource == "" {
		return false
	}

	// Match wildcard for all resources
	if ruleResource == "*" {
		return true
	}

	// Exact match (case-insensitive)
	if strings.EqualFold(ruleResource, resourcePlural) {
		return true
	}

	// Subresource wildcard: "pods/*" matches "pods/log", "pods/status", etc.
	if strings.HasSuffix(ruleResource, "/*") {
		prefix := ruleResource[:len(ruleResource)-2] // Remove "/*"
		return strings.HasPrefix(strings.ToLower(resourcePlural), strings.ToLower(prefix)+"/")
	}

	return false
}

// SnapshotWatchRules returns a deep-copied slice of compiled WatchRule entries.
// Safe for concurrent use; the returned slice can be freely modified by callers.
func (s *RuleStore) SnapshotWatchRules() []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]CompiledRule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, deepCopyCompiledRule(r))
	}
	return out
}

// SnapshotClusterWatchRules returns a deep-copied slice of compiled ClusterWatchRule entries.
// Safe for concurrent use; the returned slice can be freely modified by callers.
func (s *RuleStore) SnapshotClusterWatchRules() []CompiledClusterRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]CompiledClusterRule, 0, len(s.clusterRules))
	for _, r := range s.clusterRules {
		out = append(out, deepCopyCompiledClusterRule(r))
	}
	return out
}

// deepCopyCompiledRule creates a defensive copy of a CompiledRule including its slices.
func deepCopyCompiledRule(in CompiledRule) CompiledRule {
	cp := in
	if len(in.ResourceRules) > 0 {
		cp.ResourceRules = make([]CompiledResourceRule, len(in.ResourceRules))
		copy(cp.ResourceRules, in.ResourceRules)
	}
	return cp
}

// deepCopyCompiledClusterRule creates a defensive copy of a CompiledClusterRule including its slices.
func deepCopyCompiledClusterRule(in CompiledClusterRule) CompiledClusterRule {
	cp := in
	if len(in.Rules) > 0 {
		cp.Rules = make([]CompiledClusterResourceRule, len(in.Rules))
		copy(cp.Rules, in.Rules)
	}
	return cp
}
