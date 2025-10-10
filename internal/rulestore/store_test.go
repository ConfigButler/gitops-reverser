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

func TestAddOrUpdateWatchRule_BasicRule(t *testing.T) {
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

	store.AddOrUpdateWatchRule(rule)

	assert.Len(t, store.rules, 1)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule, exists := store.rules[key]
	require.True(t, exists)

	assert.Equal(t, key, compiledRule.Source)
	assert.Equal(t, "my-repo-config", compiledRule.GitRepoConfigRef)
	assert.Equal(t, "default", compiledRule.GitRepoConfigNamespace)
	assert.False(t, compiledRule.IsClusterScoped)
	assert.Nil(t, compiledRule.ObjectSelector)
	assert.Len(t, compiledRule.ResourceRules, 1)
	assert.Equal(t, []string{"pods", "services"}, compiledRule.ResourceRules[0].Resources)
}

func TestAddOrUpdateWatchRule_RuleWithObjectSelector(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ObjectSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpDoesNotExist,
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

	store.AddOrUpdateWatchRule(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	require.NotNil(t, compiledRule.ObjectSelector)
	assert.Len(t, compiledRule.ObjectSelector.MatchExpressions, 1)
	assert.Equal(t, "configbutler.ai/ignore", compiledRule.ObjectSelector.MatchExpressions[0].Key)
}

func TestAddOrUpdateWatchRule_MultipleResourceRules(t *testing.T) {
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

	store.AddOrUpdateWatchRule(rule)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	// Should have 2 separate resource rules (not flattened)
	assert.Len(t, compiledRule.ResourceRules, 2)
	assert.Equal(t, []string{"pods", "services"}, compiledRule.ResourceRules[0].Resources)
	assert.Equal(t, []string{"deployments", "configmaps"}, compiledRule.ResourceRules[1].Resources)
}

func TestAddOrUpdateWatchRule_UpdateExistingRule(t *testing.T) {
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
	store.AddOrUpdateWatchRule(rule1)

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
	store.AddOrUpdateWatchRule(rule2)

	// Should still have only one rule, but updated
	assert.Len(t, store.rules, 1)

	key := types.NamespacedName{Name: "test-rule", Namespace: "default"}
	compiledRule := store.rules[key]

	assert.Equal(t, "repo-config-2", compiledRule.GitRepoConfigRef)
	assert.Len(t, compiledRule.ResourceRules, 1)
	assert.Equal(t, []string{"services", "deployments"}, compiledRule.ResourceRules[0].Resources)
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
	store.AddOrUpdateWatchRule(rule)

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
	store.AddOrUpdateWatchRule(rule)

	// Create a Pod object
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Len(t, matches, 1)
	assert.Equal(t, "pod-rule", matches[0].Source.Name)
}

func TestGetMatchingRules_OperationFiltering(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-update-only",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations: []configv1alpha1.OperationType{
						configv1alpha1.OperationCreate,
						configv1alpha1.OperationUpdate,
					},
					Resources: []string{"pods"},
				},
			},
		},
	}
	store.AddOrUpdateWatchRule(rule)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	// CREATE should match
	matches := store.GetMatchingRules(obj, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Len(t, matches, 1)

	// UPDATE should match
	matches = store.GetMatchingRules(obj, "pods", configv1alpha1.OperationUpdate, "", "v1", false)
	assert.Len(t, matches, 1)

	// DELETE should NOT match
	matches = store.GetMatchingRules(obj, "pods", configv1alpha1.OperationDelete, "", "v1", false)
	assert.Empty(t, matches)
}

func TestGetMatchingRules_APIGroupFiltering(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apps-only",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					APIGroups: []string{"apps"},
					Resources: []string{"deployments"},
				},
			},
		},
	}
	store.AddOrUpdateWatchRule(rule)

	// Deployment in apps group should match
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})
	deployment.SetName("test-deployment")
	deployment.SetNamespace("default")

	matches := store.GetMatchingRules(deployment, "deployments", configv1alpha1.OperationCreate, "apps", "v1", false)
	assert.Len(t, matches, 1)

	// Pod in core group should NOT match
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	pod.SetName("test-pod")
	pod.SetNamespace("default")

	matches = store.GetMatchingRules(pod, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)
}

func TestGetMatchingRules_ObjectSelectorFiltering(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "labeled-pods",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ObjectSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "app",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"myapp"},
					},
					{
						Key:      "configbutler.ai/ignore",
						Operator: metav1.LabelSelectorOpDoesNotExist,
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
	store.AddOrUpdateWatchRule(rule)

	// Pod with app=myapp should match
	obj1 := &unstructured.Unstructured{}
	obj1.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj1.SetName("matching-pod")
	obj1.SetNamespace("default")
	obj1.SetLabels(map[string]string{"app": "myapp"})

	matches := store.GetMatchingRules(obj1, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Len(t, matches, 1)

	// Pod without app label should NOT match
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj2.SetName("non-matching-pod")
	obj2.SetNamespace("default")

	matches = store.GetMatchingRules(obj2, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)

	// Pod with ignore label should NOT match
	obj3 := &unstructured.Unstructured{}
	obj3.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj3.SetName("ignored-pod")
	obj3.SetNamespace("default")
	obj3.SetLabels(map[string]string{
		"app":                    "myapp",
		"configbutler.ai/ignore": "true",
	})

	matches = store.GetMatchingRules(obj3, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)
}

func TestGetMatchingRules_NamespaceIsolation(t *testing.T) {
	store := NewStore()

	// Rule in namespace "team-a"
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a-rule",
			Namespace: "team-a",
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
	store.AddOrUpdateWatchRule(rule)

	// Pod in team-a namespace should match
	obj1 := &unstructured.Unstructured{}
	obj1.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj1.SetName("team-a-pod")
	obj1.SetNamespace("team-a")

	matches := store.GetMatchingRules(obj1, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Len(t, matches, 1)

	// Pod in team-b namespace should NOT match
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj2.SetName("team-b-pod")
	obj2.SetNamespace("team-b")

	matches = store.GetMatchingRules(obj2, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)
}

func TestGetMatchingRules_ClusterScopedFiltering(t *testing.T) {
	store := NewStore()

	// Namespace-scoped rule
	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "namespaced-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			Rules: []configv1alpha1.ResourceRule{
				{
					Resources: []string{"*"},
				},
			},
		},
	}
	store.AddOrUpdateWatchRule(rule)

	// Namespaced Pod should match
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	pod.SetName("test-pod")
	pod.SetNamespace("default")

	matches := store.GetMatchingRules(pod, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Len(t, matches, 1)

	// Cluster-scoped Node should NOT match (WatchRule can't watch cluster resources)
	node := &unstructured.Unstructured{}
	node.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Node",
	})
	node.SetName("test-node")
	// No namespace - cluster-scoped

	matches = store.GetMatchingRules(node, "nodes", configv1alpha1.OperationCreate, "", "v1", true)
	assert.Empty(t, matches)
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

	store.AddOrUpdateWatchRule(rule1)
	store.AddOrUpdateWatchRule(rule2)
	store.AddOrUpdateWatchRule(rule3)

	// Test Pod in default namespace - should match rule1 and rule2
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	obj.SetName("test-pod")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj, "pods", configv1alpha1.OperationCreate, "", "v1", false)
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
	store.AddOrUpdateWatchRule(rule)

	// Test Service - should not match
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Service",
	})
	obj.SetName("test-service")
	obj.SetNamespace("default")

	matches := store.GetMatchingRules(obj, "services", configv1alpha1.OperationCreate, "", "v1", false)
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

	matches := store.GetMatchingRules(obj, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)
}

func TestCompiledRule_matches_InvalidLabelSelector(t *testing.T) {
	// Test edge case where label selector is malformed
	rule := &CompiledRule{
		Source:                 types.NamespacedName{Name: "test", Namespace: "default"},
		GitRepoConfigRef:       "repo",
		GitRepoConfigNamespace: "default",
		IsClusterScoped:        false,
		ResourceRules: []CompiledResourceRule{
			{
				Resources: []string{"pods"},
			},
		},
		ObjectSelector: &metav1.LabelSelector{
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
	matches := rule.matches(obj, "pods", configv1alpha1.OperationCreate, "", "v1")
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
			store.AddOrUpdateWatchRule(rule)
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
			store.GetMatchingRules(obj, "pods", configv1alpha1.OperationCreate, "", "v1", false)
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
		{"", "pods", false},         // Empty pattern
		{"pods", "pods", true},      // Exact match
		{"pods", "Pods", true},      // Case insensitive
		{"PODS", "pods", true},      // Case insensitive
		{"pods", "services", false}, // Different resource
		// Subresource support
		{"pods/*", "pods/log", true},
		{"pods/*", "pods/status", true},
		{"pods/*", "pods", false}, // Not a subresource
		{"pods/log", "pods/log", true},
		{"pods/log", "pods/status", false},
	}

	for _, tc := range testCases {
		t.Run(tc.pattern+"_vs_"+tc.resourcePlural, func(t *testing.T) {
			rule := &CompiledResourceRule{
				Resources: []string{tc.pattern},
			}

			matches := rule.resourceMatches(tc.resourcePlural)
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
	store.AddOrUpdateWatchRule(rule)

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
	matches := store.GetMatchingRules(
		obj, "myapps.example.com",
		configv1alpha1.OperationCreate,
		"example.com", "v1", false,
	)
	assert.Len(t, matches, 1, "Expected custom resource to match using group-qualified plural")
	assert.Equal(t, "myapp-rule", matches[0].Source.Name)

	// Test that just the plural without group doesn't match
	matches = store.GetMatchingRules(obj, "myapps", configv1alpha1.OperationCreate, "example.com", "v1", false)
	assert.Empty(t, matches, "Expected no match without group qualifier")
}

func TestGetMatchingRules_ComplexScenario(t *testing.T) {
	store := NewStore()

	rule := configv1alpha1.WatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "complex-rule",
			Namespace: "default",
		},
		Spec: configv1alpha1.WatchRuleSpec{
			GitRepoConfigRef: "my-repo-config",
			ObjectSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"environment": "production",
				},
			},
			Rules: []configv1alpha1.ResourceRule{
				{
					Operations: []configv1alpha1.OperationType{
						configv1alpha1.OperationCreate,
						configv1alpha1.OperationUpdate,
					},
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments", "statefulsets"},
				},
			},
		},
	}
	store.AddOrUpdateWatchRule(rule)

	// Deployment that matches all criteria
	matchingDeployment := &unstructured.Unstructured{}
	matchingDeployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})
	matchingDeployment.SetName("prod-deployment")
	matchingDeployment.SetNamespace("default")
	matchingDeployment.SetLabels(map[string]string{"environment": "production"})

	matches := store.GetMatchingRules(
		matchingDeployment, "deployments",
		configv1alpha1.OperationCreate,
		"apps", "v1", false,
	)
	assert.Len(t, matches, 1)

	// Deployment missing label - should NOT match
	noLabelDeployment := &unstructured.Unstructured{}
	noLabelDeployment.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})
	noLabelDeployment.SetName("dev-deployment")
	noLabelDeployment.SetNamespace("default")

	matches = store.GetMatchingRules(
		noLabelDeployment, "deployments",
		configv1alpha1.OperationCreate,
		"apps", "v1", false,
	)
	assert.Empty(t, matches)

	// Deployment with DELETE operation - should NOT match
	matches = store.GetMatchingRules(
		matchingDeployment, "deployments",
		configv1alpha1.OperationDelete,
		"apps", "v1", false,
	)
	assert.Empty(t, matches)

	// Pod (different resource) - should NOT match
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Pod",
	})
	pod.SetName("prod-pod")
	pod.SetNamespace("default")
	pod.SetLabels(map[string]string{"environment": "production"})

	matches = store.GetMatchingRules(pod, "pods", configv1alpha1.OperationCreate, "", "v1", false)
	assert.Empty(t, matches)
}

// TestAddOrUpdateClusterWatchRule_BasicRule tests adding a ClusterWatchRule with Cluster scope.
func TestAddOrUpdateClusterWatchRule_BasicRule(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					APIGroups: []string{""},
					Resources: []string{"nodes"},
				},
			},
		},
	}

	store.AddOrUpdateClusterWatchRule(clusterRule)

	assert.Len(t, store.clusterRules, 1)

	key := types.NamespacedName{Name: "test-cluster-rule", Namespace: ""}
	compiledRule, exists := store.clusterRules[key]
	require.True(t, exists)

	assert.Equal(t, key, compiledRule.Source)
	assert.Equal(t, "audit-config", compiledRule.GitRepoConfigRef)
	assert.Equal(t, "audit-system", compiledRule.GitRepoConfigNamespace)
	assert.Len(t, compiledRule.Rules, 1)
	assert.Equal(t, configv1alpha1.ResourceScopeCluster, compiledRule.Rules[0].Scope)
}

// TestAddOrUpdateClusterWatchRule_NamespacedScope tests ClusterWatchRule with Namespaced scope.
func TestAddOrUpdateClusterWatchRule_NamespacedScope(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "namespaced-cluster-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					APIGroups: []string{""},
					Resources: []string{"secrets"},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"env": "production",
						},
					},
				},
			},
		},
	}

	store.AddOrUpdateClusterWatchRule(clusterRule)

	key := types.NamespacedName{Name: "namespaced-cluster-rule", Namespace: ""}
	compiledRule := store.clusterRules[key]

	assert.Len(t, compiledRule.Rules, 1)
	assert.Equal(t, configv1alpha1.ResourceScopeNamespaced, compiledRule.Rules[0].Scope)
	require.NotNil(t, compiledRule.Rules[0].NamespaceSelector)
	assert.Equal(t, "production", compiledRule.Rules[0].NamespaceSelector.MatchLabels["env"])
}

// TestDeleteClusterWatchRule tests removing a ClusterWatchRule.
func TestDeleteClusterWatchRule(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					Resources: []string{"nodes"},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	key := types.NamespacedName{Name: "test-cluster-rule", Namespace: ""}
	assert.Len(t, store.clusterRules, 1)

	store.DeleteClusterWatchRule(key)
	assert.Empty(t, store.clusterRules)
}

// TestGetMatchingClusterRules_ClusterScope tests matching cluster-scoped resources.
func TestGetMatchingClusterRules_ClusterScope(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					APIGroups: []string{""},
					Resources: []string{"nodes"},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	matches := store.GetMatchingClusterRules(
		"nodes",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		true, // cluster-scoped
		nil,  // no namespace labels for cluster resources
	)

	assert.Len(t, matches, 1)
	assert.Equal(t, "node-rule", matches[0].Source.Name)
}

// TestGetMatchingClusterRules_ClusterScopeDoesNotMatchNamespaced tests that Cluster scope rules
// don't match namespaced resources.
func TestGetMatchingClusterRules_ClusterScopeDoesNotMatchNamespaced(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-only-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					Resources: []string{"*"},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Namespaced resource should NOT match
	matches := store.GetMatchingClusterRules(
		"pods",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		false, // namespaced
		map[string]string{},
	)

	assert.Empty(t, matches)
}

// TestGetMatchingClusterRules_NamespacedScopeAllNamespaces tests Namespaced scope without selector.
func TestGetMatchingClusterRules_NamespacedScopeAllNamespaces(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "all-deployments-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					APIGroups: []string{"apps"},
					Resources: []string{"deployments"},
					// No namespaceSelector = all namespaces
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Deployment in any namespace should match
	matches := store.GetMatchingClusterRules(
		"deployments",
		configv1alpha1.OperationCreate,
		"apps",
		"v1",
		false, // namespaced
		map[string]string{"env": "dev"},
	)
	assert.Len(t, matches, 1)

	matches = store.GetMatchingClusterRules(
		"deployments",
		configv1alpha1.OperationCreate,
		"apps",
		"v1",
		false, // namespaced
		map[string]string{"env": "prod"},
	)
	assert.Len(t, matches, 1)
}

// TestGetMatchingClusterRules_NamespacedScopeWithMatchingSelector tests namespace selector matching.
func TestGetMatchingClusterRules_NamespacedScopeWithMatchingSelector(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-secrets-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					APIGroups: []string{""},
					Resources: []string{"secrets"},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"env": "production",
						},
					},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Secret in production namespace should match
	matches := store.GetMatchingClusterRules(
		"secrets",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		false, // namespaced
		map[string]string{"env": "production"},
	)
	assert.Len(t, matches, 1)
}

// TestGetMatchingClusterRules_NamespacedScopeWithNonMatchingSelector tests selector filtering.
func TestGetMatchingClusterRules_NamespacedScopeWithNonMatchingSelector(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-secrets-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					APIGroups: []string{""},
					Resources: []string{"secrets"},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"env": "production",
						},
					},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Secret in dev namespace should NOT match
	matches := store.GetMatchingClusterRules(
		"secrets",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		false, // namespaced
		map[string]string{"env": "dev"},
	)
	assert.Empty(t, matches)
}

// TestGetMatchingClusterRules_MixedScopes tests a rule with both Cluster and Namespaced scope.
func TestGetMatchingClusterRules_MixedScopes(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mixed-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					APIGroups: []string{""},
					Resources: []string{"nodes"},
				},
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					APIGroups: []string{""},
					Resources: []string{"pods"},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Cluster-scoped Node should match
	matches := store.GetMatchingClusterRules(
		"nodes",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		true, // cluster-scoped
		nil,
	)
	assert.Len(t, matches, 1)

	// Namespaced Pod should match
	matches = store.GetMatchingClusterRules(
		"pods",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		false, // namespaced
		map[string]string{},
	)
	assert.Len(t, matches, 1)
}

// TestGetMatchingClusterRules_InvalidSelector tests handling of invalid namespace selectors.
func TestGetMatchingClusterRules_InvalidSelector(t *testing.T) {
	store := NewStore()

	clusterRule := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "invalid-selector-rule",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "audit-config",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeNamespaced,
					Resources: []string{"secrets"},
					NamespaceSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "invalid-key!@#",
								Operator: "InvalidOperator",
							},
						},
					},
				},
			},
		},
	}
	store.AddOrUpdateClusterWatchRule(clusterRule)

	// Should not match due to invalid selector
	matches := store.GetMatchingClusterRules(
		"secrets",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		false, // namespaced
		map[string]string{"env": "prod"},
	)
	assert.Empty(t, matches)
}

// TestGetMatchingClusterRules_MultipleMatchingRules tests multiple ClusterWatchRules matching.
func TestGetMatchingClusterRules_MultipleMatchingRules(t *testing.T) {
	store := NewStore()

	// Rule 1: All nodes
	clusterRule1 := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "all-nodes",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "config1",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					Resources: []string{"nodes"},
				},
			},
		},
	}

	// Rule 2: All cluster resources
	clusterRule2 := configv1alpha1.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "all-cluster-resources",
		},
		Spec: configv1alpha1.ClusterWatchRuleSpec{
			GitRepoConfigRef: configv1alpha1.NamespacedName{
				Name:      "config2",
				Namespace: "audit-system",
			},
			Rules: []configv1alpha1.ClusterResourceRule{
				{
					Scope:     configv1alpha1.ResourceScopeCluster,
					Resources: []string{"*"},
				},
			},
		},
	}

	store.AddOrUpdateClusterWatchRule(clusterRule1)
	store.AddOrUpdateClusterWatchRule(clusterRule2)

	// Node should match both rules
	matches := store.GetMatchingClusterRules(
		"nodes",
		configv1alpha1.OperationCreate,
		"",
		"v1",
		true, // cluster-scoped
		nil,
	)

	assert.Len(t, matches, 2)
	ruleNames := []string{matches[0].Source.Name, matches[1].Source.Name}
	assert.Contains(t, ruleNames, "all-nodes")
	assert.Contains(t, ruleNames, "all-cluster-resources")
}
