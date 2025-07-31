package rulestore

import (
	"strings"
	"sync"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// Check if the resource kind matches.
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	kindMatch := false
	for _, rk := range r.Resources {
		if rk == "" {
			// Empty pattern doesn't match anything
			continue
		}
		if rk == "*" {
			// Universal wildcard matches everything
			kindMatch = true
			break
		}
		if rk == kind {
			// Exact match
			kindMatch = true
			break
		}
		if len(rk) > 1 && rk[len(rk)-1] == '*' {
			// Prefix wildcard match (e.g., "Pod*" matches "PodDisruptionBudget")
			prefix := rk[:len(rk)-1]
			if strings.HasPrefix(kind, prefix) {
				kindMatch = true
				break
			}
		}
	}
	if !kindMatch {
		return false
	}

	// Check if the resource is excluded by labels.
	if r.ExcludeLabels != nil {
		selector, err := metav1.LabelSelectorAsSelector(r.ExcludeLabels)
		if err != nil {
			// This should not happen if validation is working.
			return false
		}
		if selector.Matches(labels.Set(obj.GetLabels())) {
			return false
		}
	}

	return true
}
