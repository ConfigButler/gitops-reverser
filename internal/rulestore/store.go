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
func (s *RuleStore) GetMatchingRules(obj client.Object) []CompiledRule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchingRules []CompiledRule
	for _, rule := range s.rules {
		if rule.matches(obj) {
			matchingRules = append(matchingRules, rule)
		}
	}
	return matchingRules
}

// matches checks if a single rule matches the given object.
func (r *CompiledRule) matches(obj client.Object) bool {
	kind := obj.GetObjectKind().GroupVersionKind().Kind

	if !r.kindMatches(kind) {
		return false
	}

	return !r.isExcludedByLabels(obj)
}

// kindMatches checks if the resource kind matches any of the rule patterns.
func (r *CompiledRule) kindMatches(kind string) bool {
	for _, rk := range r.Resources {
		if r.singleKindMatches(rk, kind) {
			return true
		}
	}
	return false
}

// singleKindMatches checks if a single rule pattern matches the given kind.
func (r *CompiledRule) singleKindMatches(ruleKind, kind string) bool {
	if ruleKind == "" {
		return false
	}

	if ruleKind == "*" {
		return true
	}

	if ruleKind == kind {
		return true
	}

	if r.isPluralMatch(ruleKind, kind) {
		return true
	}

	return r.isWildcardMatch(ruleKind, kind)
}

// isPluralMatch handles common Kubernetes resource plural forms.
func (r *CompiledRule) isPluralMatch(ruleKind, kind string) bool {
	pluralMappings := map[string]string{
		"configmaps":  "ConfigMap",
		"pods":        "Pod",
		"services":    "Service",
		"deployments": "Deployment",
		"secrets":     "Secret",
	}

	if expectedKind, exists := pluralMappings[strings.ToLower(ruleKind)]; exists {
		return strings.EqualFold(kind, expectedKind)
	}
	return false
}

// isWildcardMatch handles prefix wildcard matching.
func (r *CompiledRule) isWildcardMatch(ruleKind, kind string) bool {
	if len(ruleKind) <= 1 || ruleKind[len(ruleKind)-1] != '*' {
		return false
	}

	prefix := ruleKind[:len(ruleKind)-1]
	return strings.HasPrefix(strings.ToLower(kind), strings.ToLower(prefix))
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
