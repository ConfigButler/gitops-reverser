package rulestore

import (
	"testing"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewStore(t *testing.T) {
	store := NewStore()
	assert.NotNil(t, store)
	assert.NotNil(t, store.rules)
	assert.Equal(t, 0, len(store.rules))
}

func TestAddOrUpdate_BasicRule(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod", "Service"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	assert.Equal(t, 1, len(store.rules))

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule, exists := store.rules[key]
	require.True(t, exists)

	assert.Equal(t, key, compiledRule.Source)
	assert.Equal(t, "my-repo-config", compiledRule.GitRepoConfigRef)
	assert.Equal(t, []string{"Pod", "Service"}, compiledRule.Resources)
	assert.Nil(t, compiledRule.ExcludeLabels)
}

func TestAddOrUpdate_RuleWithExcludeLabels(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ExcludeLabels: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Deployment"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	require.NotNil(t, compiledRule.ExcludeLabels)
	assert.Equal(t, 1, len(compiledRule.ExcludeLabels.MatchExpressions))
	assert.Equal(t, "configbutler.ai/ignore", compiledRule.ExcludeLabels.MatchExpressions[0].Key)
}

func TestAddOrUpdate_MultipleResourceRules(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod", "Service"},
				},
				{
					Resources: []string{"Deployment", "ConfigMap"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	// Should flatten all resources from all rules
	expected := []string{"Pod", "Service", "Deployment", "ConfigMap"}
	assert.Equal(t, expected, compiledRule.Resources)
}

func TestAddOrUpdate_UpdateExistingRule(t *testing.T) {
	store := NewStore()

	// Add initial rule
	rule1 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-1",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule1)

	// Update with different spec
	rule2 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-2",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Service", "Deployment"},
				},
			},
		},
	}
	store.AddOrUpdate(rule2)

	// Should still have only one rule, but updated
	assert.Equal(t, 1, len(store.rules))

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	assert.Equal(t, "repo-config-2", compiledRule.GitRepoConfigRef)
	assert.Equal(t, []string{"Service", "Deployment"}, compiledRule.Resources)
}

func TestDelete(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	assert.Equal(t, 1, len(store.rules))

	store.Delete(key)
	assert.Equal(t, 0, len(store.rules))
}

func TestDelete_NonExistentRule(t *testing.T) {
	store := NewStore()

	key := types.NamespacedName{Name: "non-existent", Namespace: "default"}

	// Should not panic
	store.Delete(key)
	assert.Equal(t, 0, len(store.rules))
}

func TestGetMatchingRules_ExactMatch(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Create a Pod object
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj)
	assert.Equal(t, 1, len(matches))
	assert.Equal(t, "pod-rule", matches[0].Source.Name)
}

func TestGetMatchingRules_WildcardMatch(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Ingress*"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Test various Ingress-related resources
	testCases := []struct {
		kind        string
		shouldMatch bool
	}{
		{"Ingress", true},
		{"IngressClass", true},
		{"IngressRoute", true},
		{"Service", false},
		{"Pod", false},
	}

	for _, tc := range testCases {
		t.Run(tc.kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "networking.k8s.io",
				Version: "v1",
				Kind:    tc.kind,
			})
			obj.SetName("test-resource")
			obj.SetNamespace("default")

			matches := store.GetMatchingRules(obj)
			if tc.shouldMatch {
				assert.Equal(t, 1, len(matches), "Expected %s to match Ingress*", tc.kind)
			} else {
				assert.Equal(t, 0, len(matches), "Expected %s not to match Ingress*", tc.kind)
			}
		})
	}
}

func TestGetMatchingRules_ExcludedByLabels(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ExcludeLabels: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Test Pod without ignore label - should match
	obj1 := &unstructured.Unstructured{}
	obj1.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj1.SetName("normal-pod")
	obj1.SetNamespace("default")

	matches := store.GetMatchingRules(obj1)
	assert.Equal(t, 1, len(matches))

	// Test Pod with ignore label - should not match
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj2.SetName("ignored-pod")
	obj2.SetNamespace("default")
	obj2.SetLabels(map[string]string{
		"configbutler.ai/ignore": "true",
	})

	matches = store.GetMatchingRules(obj2)
	assert.Equal(t, 0, len(matches))
}

func TestGetMatchingRules_ComplexLabelSelector(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "complex-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ExcludeLabels: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"environment": "test",
				},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "version",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"v1", "v2"},
					},
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	testCases := []struct {
		name        string
		labels      map[string]string
		shouldMatch bool
	}{
		{
			name:        "no matching labels",
			labels:      map[string]string{"app": "myapp"},
			shouldMatch: true,
		},
		{
			name:        "matches environment only",
			labels:      map[string]string{"environment": "test"},
			shouldMatch: true, // Only environment=test, but version is required too for exclusion
		},
		{
			name:        "matches version only",
			labels:      map[string]string{"version": "v1"},
			shouldMatch: true, // Only environment=test would exclude
		},
		{
			name: "matches both conditions",
			labels: map[string]string{
				"environment": "test",
				"version":     "v1",
			},
			shouldMatch: false,
		},
		{
			name: "matches environment but not version",
			labels: map[string]string{
				"environment": "test",
				"version":     "v3",
			},
			shouldMatch: true, // version v3 not in [v1, v2], so selector doesn't match
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			})
			obj.SetName("test-pod")
			obj.SetNamespace("default")
			obj.SetLabels(tc.labels)

			matches := store.GetMatchingRules(obj)
			if tc.shouldMatch {
				assert.Equal(t, 1, len(matches), "Expected pod with labels %v to match", tc.labels)
			} else {
				assert.Equal(t, 0, len(matches), "Expected pod with labels %v to be excluded", tc.labels)
			}
		})
	}
}

func TestGetMatchingRules_MultipleRules(t *testing.T) {
	store := NewStore()

	// Add multiple rules
	rule1 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-1",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}

	rule2 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "all-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-2",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod", "Service", "Deployment"},
				},
			},
		},
	}

	rule3 := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-rule",
			Namespace: "other",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "repo-config-3",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Service"},
				},
			},
		},
	}

	store.AddOrUpdate(rule1)
	store.AddOrUpdate(rule2)
	store.AddOrUpdate(rule3)

	// Test Pod - should match rule1 and rule2
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj)
	assert.Equal(t, 2, len(matches))

	// Verify both rules are returned
	ruleNames := make([]string, len(matches))
	for i, match := range matches {
		ruleNames[i] = match.Source.Name
	}
	assert.Contains(t, ruleNames, "pod-rule")
	assert.Contains(t, ruleNames, "all-rule")
}

func TestGetMatchingRules_NoMatches(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"Pod"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Test Service - should not match
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Service",
	})
	obj.SetName("test-service")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj)
	assert.Equal(t, 0, len(matches))
}

func TestGetMatchingRules_EmptyStore(t *testing.T) {
	store := NewStore()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj)
	assert.Equal(t, 0, len(matches))
}

func TestCompiledRule_matches_InvalidLabelSelector(t *testing.T) {
	// Test edge case where label selector is malformed
	rule := &CompiledRule{
		Source:           types.NamespacedName{Name: "test", Namespace: "default"},
		GitRepoConfigRef: "repo",
		Resources:        []string{"Pod"},
		ExcludeLabels: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "invalid-key-with-invalid-chars!@#",
					Operator: "InvalidOperator", // Invalid operator
				},
			},
		},
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	// Should return false when label selector is invalid
	matches := rule.matches(obj)
	assert.False(t, matches)
}

func TestConcurrentAccess(t *testing.T) {
	store := NewStore()

	// Test concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			rule := configv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rule-" + string(rune(i)),
					Namespace: "default",
				},
				Spec: configv1alpha1.WatchRuleSpec{
					GitRepoConfigRef: "repo-config",
					Rules: []configv1alpha1.ResourceRule{
						{
							Resources: []string{"Pod"},
						},
					},
				},
			}
			store.AddOrUpdate(rule)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "",
			Version: "v1",
			Kind:    "Pod",
		})
		obj.SetName("test-pod")
		obj.SetNamespace("default")

		for i := 0; i < 100; i++ {
			store.GetMatchingRules(obj)
		}
		done <- true
	}()

	// Wait for both goroutines to complete
	<-done
	<-done

	// Verify final state
	assert.Equal(t, 100, len(store.rules))
}

func TestWildcardMatching_EdgeCases(t *testing.T) {
	testCases := []struct {
		pattern     string
		kind        string
		shouldMatch bool
	}{
		{"*", "Pod", true},
		{"*", "Service", true},
		{"*", "", true},
		{"Pod*", "Pod", true},
		{"Pod*", "PodDisruptionBudget", true},
		{"Pod*", "Service", false},
		{"*Pod", "Pod", false}, // Only prefix wildcards supported
		{"P*d", "Pod", false},  // Only suffix wildcards supported
		{"", "Pod", false},     // Empty pattern
		{"Pod", "Pod", true},   // Exact match
		{"Pod", "pod", false},  // Case sensitive
	}

	for _, tc := range testCases {
		t.Run(tc.pattern+"_vs_"+tc.kind, func(t *testing.T) {
			rule := &CompiledRule{
				Resources: []string{tc.pattern},
			}

			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    tc.kind,
			})

			matches := rule.matches(obj)
			assert.Equal(t, tc.shouldMatch, matches,
				"Pattern %s should match %s: %v", tc.pattern, tc.kind, tc.shouldMatch)
		})
	}
}
