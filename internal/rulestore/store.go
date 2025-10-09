/*
Package rulestore manages the in-memory cache of compiled WatchRule configurations.
It provides efficient lookup and matching of Kubernetes resources against active watch rules.
*/
package rulestore

import (
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// CompiledRule represents a fully processed WatchRule, ready for quick lookups.
type CompiledRule struct {
	// Source is the NamespacedName of the WatchRule CR.
	Source types.NamespacedName
	// GitRepoConfigRef is the name of the GitRepoConfig to use.
	GitRepoConfigRef string
	// ExcludeLabels is the selector to filter out resources.
	ExcludeLabels *metav1.LabelSelector
	// Resources is a list of resource kinds to watch.
	Resources []string
}

// RuleStore holds the in-memory representation of all active watch rules.
// It is safe for concurrent use.
type RuleStore struct {
	mu    sync.RWMutex
	rules map[types.NamespacedName]CompiledRule
}

// NewStore creates a new, empty RuleStore.
func NewStore() *RuleStore {
	return &RuleStore{
		rules: make(map[types.NamespacedName]CompiledRule),
	}
}

// AddOrUpdate adds or updates a rule in the store.
func (s *RuleStore) AddOrUpdate(rule configv1alpha1.WatchRule) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{
		Name:      rule.Name,
		Namespace: rule.Namespace,
	}

	compiled := CompiledRule{
		Source:           key,
		GitRepoConfigRef: rule.Spec.GitRepoConfigRef,
		ExcludeLabels:    rule.Spec.ExcludeLabels,
	}
	for _, r := range rule.Spec.Rules {
		compiled.Resources = append(compiled.Resources, r.Resources...)
	}

	s.rules[key] = compiled
}

// Delete removes a rule from the store.
func (s *RuleStore) Delete(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, key)
}

// GetMatchingRules returns all rules that match the given resource.
// resourcePlural is the plural form of the resource (e.g., "pods", "deployments", "myapps").
func (s *RuleStore) GetMatchingRules(obj client.Object, resourcePlural string) []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchingRules []CompiledRule
	for _, rule := range s.rules {
		if rule.matches(obj, resourcePlural) {
			matchingRules = append(matchingRules, rule)
		}
	}
	return matchingRules
}

// matches checks if a single rule matches the given object.
func (r *CompiledRule) matches(obj client.Object, resourcePlural string) bool {
	if !r.resourceMatches(resourcePlural) {
		return false
	}

	return !r.isExcludedByLabels(obj)
}

// resourceMatches checks if the resource plural matches any of the rule patterns.
func (r *CompiledRule) resourceMatches(resourcePlural string) bool {
	for _, ruleResource := range r.Resources {
		if r.singleResourceMatches(ruleResource, resourcePlural) {
			return true
		}
	}
	return false
}

// singleResourceMatches checks if a single rule pattern matches the given resource plural.
// This supports exact matches, wildcard patterns, and group-qualified resources.
func (r *CompiledRule) singleResourceMatches(ruleResource, resourcePlural string) bool {
	if ruleResource == "" {
		return false
	}

	// Match wildcard for all resources
	if ruleResource == "*" {
		return true
	}

	// Exact match (case-insensitive for better compatibility)
	if strings.EqualFold(ruleResource, resourcePlural) {
		return true
	}

	// Wildcard prefix match (e.g., "ingress*" matches "ingresses.networking.k8s.io")
	if r.isWildcardMatch(ruleResource, resourcePlural) {
		return true
	}

	// Group-qualified match (e.g., "myapps.example.com" matches "myapps.example.com")
	// This is already handled by exact match above, but explicitly documented here
	return false
}

// isWildcardMatch handles wildcard matching for both prefix and suffix patterns.
// Supports patterns like "prefix*" (matches anything starting with prefix)
// and "*suffix" (matches anything ending with suffix).
func (r *CompiledRule) isWildcardMatch(ruleResource, resourcePlural string) bool {
	if len(ruleResource) <= 1 {
		return false
	}

	lowerRule := strings.ToLower(ruleResource)
	lowerResource := strings.ToLower(resourcePlural)

	// Handle suffix wildcard: "prefix*"
	if lowerRule[len(lowerRule)-1] == '*' {
		prefix := lowerRule[:len(lowerRule)-1]
		return strings.HasPrefix(lowerResource, prefix)
	}

	// Handle prefix wildcard: "*suffix"
	if lowerRule[0] == '*' {
		suffix := lowerRule[1:]
		return strings.HasSuffix(lowerResource, suffix)
	}

	return false
}

// isExcludedByLabels checks if the resource is excluded by label selectors.
func (r *CompiledRule) isExcludedByLabels(obj client.Object) bool {
	if r.ExcludeLabels == nil {
		return false
	}

	selector, err := metav1.LabelSelectorAsSelector(r.ExcludeLabels)
	if err != nil {
		return true // Treat invalid selectors as exclusions for safety
	}

	return selector.Matches(labels.Set(obj.GetLabels()))
}
