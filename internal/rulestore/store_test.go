package rulestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestNewStore(t *testing.T) {
	store := NewStore()
	assert.NotNil(t, store)
	assert.NotNil(t, store.rules)
	assert.Empty(t, store.rules)
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
					Resources: []string{"pods", "services"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	assert.Len(t, store.rules, 1)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule, exists := store.rules[key]
	require.True(t, exists)

	assert.Equal(t, key, compiledRule.Source)
	assert.Equal(t, "my-repo-config", compiledRule.GitRepoConfigRef)
	assert.Equal(t, []string{"pods", "services"}, compiledRule.Resources)
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
					Resources: []string{"deployments"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	require.NotNil(t, compiledRule.ExcludeLabels)
	assert.Len(t, compiledRule.ExcludeLabels.MatchExpressions, 1)
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
					Resources: []string{"pods", "services"},
				},
				{
					Resources: []string{"deployments", "configmaps"},
				},
			},
		},
	}

	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	// Should flatten all resources from all rules
	expected := []string{"pods", "services", "deployments", "configmaps"}
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
					Resources: []string{"pods"},
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
					Resources: []string{"services", "deployments"},
				},
			},
		},
	}
	store.AddOrUpdate(rule2)

	// Should still have only one rule, but updated
	assert.Len(t, store.rules, 1)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	assert.Equal(t, "repo-config-2", compiledRule.GitRepoConfigRef)
	assert.Equal(t, []string{"services", "deployments"}, compiledRule.Resources)
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
					Resources: []string{"pods"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	assert.Len(t, store.rules, 1)

	store.Delete(key)
	assert.Empty(t, store.rules)
}

func TestDelete_NonExistentRule(t *testing.T) {
	store := NewStore()

	key := types.NamespacedName{Name: "non-existent", Namespace: "default"}

	// Should not panic
	store.Delete(key)
	assert.Empty(t, store.rules)
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
					Resources: []string{"pods"},
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

	matches := store.GetMatchingRules(obj, "pods")
	assert.Len(t, matches, 1)
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
					Resources: []string{"ingress*"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Test various Ingress-related resources
	testCases := []struct {
		kind           string
		resourcePlural string
		shouldMatch    bool
	}{
		{"Ingress", "ingresses", true},
		{"IngressClass", "ingressclasses", true},
		{"IngressRoute", "ingressroutes", true},
		{"Service", "services", false},
		{"Pod", "pods", false},
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

			matches := store.GetMatchingRules(obj, tc.resourcePlural)
			if tc.shouldMatch {
				assert.Len(t, matches, 1, "Expected %s to match ingress*", tc.resourcePlural)
			} else {
				assert.Empty(t, matches, "Expected %s not to match ingress*", tc.resourcePlural)
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
					Resources: []string{"pods"},
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

	matches := store.GetMatchingRules(obj1, "pods")
	assert.Len(t, matches, 1)

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

	matches = store.GetMatchingRules(obj2, "pods")
	assert.Empty(t, matches)
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
					Resources: []string{"pods"},
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

			matches := store.GetMatchingRules(obj, "pods")
			if tc.shouldMatch {
				assert.Len(t, matches, 1, "Expected pod with labels %v to match", tc.labels)
			} else {
				assert.Empty(t, matches, "Expected pod with labels %v to be excluded", tc.labels)
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
					Resources: []string{"pods"},
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
					Resources: []string{"pods", "services", "deployments"},
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
					Resources: []string{"services"},
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

	matches := store.GetMatchingRules(obj, "pods")
	assert.Len(t, matches, 2)

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
					Resources: []string{"pods"},
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

	matches := store.GetMatchingRules(obj, "services")
	assert.Empty(t, matches)
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

	matches := store.GetMatchingRules(obj, "pods")
	assert.Empty(t, matches)
}

func TestCompiledRule_matches_InvalidLabelSelector(t *testing.T) {
	// Test edge case where label selector is malformed
	rule := &CompiledRule{
		Source:           types.NamespacedName{Name: "test", Namespace: "default"},
		GitRepoConfigRef: "repo",
		Resources:        []string{"pods"},
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
	matches := rule.matches(obj, "pods")
	assert.False(t, matches)
}

func TestConcurrentAccess(t *testing.T) {
	store := NewStore()

	// Test concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := range 100 {
			rule := configv1alpha1.WatchRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rule-" + string(rune(i)),
					Namespace: "default",
				},
				Spec: configv1alpha1.WatchRuleSpec{
					GitRepoConfigRef: "repo-config",
					Rules: []configv1alpha1.ResourceRule{
						{
							Resources: []string{"pods"},
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

		for range 100 {
			store.GetMatchingRules(obj, "pods")
		}
		done <- true
	}()

	// Wait for both goroutines to complete
	<-done
	<-done

	// Verify final state
	assert.Len(t, store.rules, 100)
}

func TestWildcardMatching_EdgeCases(t *testing.T) {
	testCases := []struct {
		pattern        string
		resourcePlural string
		shouldMatch    bool
	}{
		{"*", "pods", true},
		{"*", "services", true},
		{"*", "", true},
		{"pod*", "pods", true},
		{"pod*", "poddisruptionbudgets", true},
		{"pod*", "services", false},
		{"*pods", "pods", true},                       // Prefix wildcard (suffix matching)
		{"*.example.com", "myapps.example.com", true}, // Prefix wildcard for group
		{"p*d", "pods", false},                        // Middle wildcards not supported
		{"", "pods", false},                           // Empty pattern
		{"pods", "pods", true},                        // Exact match
		{"pods", "Pods", true},                        // Case insensitive
		{"PODS", "pods", true},                        // Case insensitive
		{"pods", "services", false},                   // Different resource
	}

	for _, tc := range testCases {
		t.Run(tc.pattern+"_vs_"+tc.resourcePlural, func(t *testing.T) {
			rule := &CompiledRule{
				Resources: []string{tc.pattern},
			}

			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "SomeKind", // Kind doesn't matter anymore
			})

			matches := rule.matches(obj, tc.resourcePlural)
			assert.Equal(t, tc.shouldMatch, matches,
				"Pattern %s should match %s: %v", tc.pattern, tc.resourcePlural, tc.shouldMatch)
		})
	}
}

func TestGetMatchingRules_CustomResources(t *testing.T) {
	store := NewStore()

	// Test matching custom resources using plural resource names
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"myapps.example.com"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	// Create a MyApp custom resource object
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "example.com",
		Version: "v1",
		Kind:    "MyApp",
	})
	obj.SetName("test-myapp")
	obj.SetNamespace("default")

	// Test matching with the full group-qualified plural name
	matches := store.GetMatchingRules(obj, "myapps.example.com")
	assert.Len(t, matches, 1, "Expected custom resource to match using group-qualified plural")
	assert.Equal(t, "myapp-rule", matches[0].Source.Name)

	// Test that just the plural without group doesn't match
	matches = store.GetMatchingRules(obj, "myapps")
	assert.Empty(t, matches, "Expected no match without group qualifier")
}

func TestGetMatchingRules_MultipleCustomResources(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crd-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{
						"myapps.example.com",
						"databases.db.example.com",
						"queues.messaging.io",
					},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	testCases := []struct {
		name           string
		group          string
		kind           string
		resourcePlural string
		shouldMatch    bool
	}{
		{
			name:           "MyApp matches",
			group:          "example.com",
			kind:           "MyApp",
			resourcePlural: "myapps.example.com",
			shouldMatch:    true,
		},
		{
			name:           "Database matches",
			group:          "db.example.com",
			kind:           "Database",
			resourcePlural: "databases.db.example.com",
			shouldMatch:    true,
		},
		{
			name:           "Queue matches",
			group:          "messaging.io",
			kind:           "Queue",
			resourcePlural: "queues.messaging.io",
			shouldMatch:    true,
		},
		{
			name:           "Unrelated CRD doesn't match",
			group:          "other.io",
			kind:           "Other",
			resourcePlural: "others.other.io",
			shouldMatch:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   tc.group,
				Version: "v1",
				Kind:    tc.kind,
			})
			obj.SetName("test-resource")
			obj.SetNamespace("default")

			matches := store.GetMatchingRules(obj, tc.resourcePlural)
			if tc.shouldMatch {
				assert.Len(t, matches, 1, "Expected %s to match", tc.resourcePlural)
			} else {
				assert.Empty(t, matches, "Expected %s not to match", tc.resourcePlural)
			}
		})
	}
}

func TestGetMatchingRules_MixedCoreAndCustomResources(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mixed-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					// Mix core resources and custom resources
					Resources: []string{
						"configmaps",
						"secrets",
						"myapps.example.com",
					},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	testCases := []struct {
		name           string
		group          string
		kind           string
		resourcePlural string
		shouldMatch    bool
	}{
		{
			name:           "ConfigMap matches",
			group:          "",
			kind:           "ConfigMap",
			resourcePlural: "configmaps",
			shouldMatch:    true,
		},
		{
			name:           "Secret matches",
			group:          "",
			kind:           "Secret",
			resourcePlural: "secrets",
			shouldMatch:    true,
		},
		{
			name:           "MyApp custom resource matches",
			group:          "example.com",
			kind:           "MyApp",
			resourcePlural: "myapps.example.com",
			shouldMatch:    true,
		},
		{
			name:           "Pod doesn't match",
			group:          "",
			kind:           "Pod",
			resourcePlural: "pods",
			shouldMatch:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   tc.group,
				Version: "v1",
				Kind:    tc.kind,
			})
			obj.SetName("test-resource")
			obj.SetNamespace("default")

			matches := store.GetMatchingRules(obj, tc.resourcePlural)
			if tc.shouldMatch {
				assert.Len(t, matches, 1, "Expected %s to match", tc.resourcePlural)
			} else {
				assert.Empty(t, matches, "Expected %s not to match", tc.resourcePlural)
			}
		})
	}
}

func TestGetMatchingRules_CustomResourceWildcard(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wildcard-crd-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					// Wildcard to match all resources in example.com group
					Resources: []string{"*.example.com"},
				},
			},
		},
	}
	store.AddOrUpdate(rule)

	testCases := []struct {
		name           string
		group          string
		kind           string
		resourcePlural string
		shouldMatch    bool
	}{
		{
			name:           "MyApps in example.com matches",
			group:          "example.com",
			kind:           "MyApp",
			resourcePlural: "myapps.example.com",
			shouldMatch:    true,
		},
		{
			name:           "Databases in example.com matches",
			group:          "example.com",
			kind:           "Database",
			resourcePlural: "databases.example.com",
			shouldMatch:    true,
		},
		{
			name:           "Resources in different group don't match",
			group:          "other.io",
			kind:           "Other",
			resourcePlural: "others.other.io",
			shouldMatch:    false,
		},
		{
			name:           "Core resources don't match",
			group:          "",
			kind:           "Pod",
			resourcePlural: "pods",
			shouldMatch:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   tc.group,
				Version: "v1",
				Kind:    tc.kind,
			})
			obj.SetName("test-resource")
			obj.SetNamespace("default")

			matches := store.GetMatchingRules(obj, tc.resourcePlural)
			if tc.shouldMatch {
				assert.Len(t, matches, 1, "Expected %s to match *.example.com", tc.resourcePlural)
			} else {
				assert.Empty(t, matches, "Expected %s not to match *.example.com", tc.resourcePlural)
			}
		})
	}
}
