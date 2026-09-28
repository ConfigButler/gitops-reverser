// SPDX-License-Identifier: Apache-2.0

package rulestore

import (
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

func TestAddOrUpdateWatchRule_CompilesTheCanonicalObjectSelector(t *testing.T) {
	store := NewStore()
	created := metav1.NewTime(time.Unix(1000, 0))
	rule := configv1alpha3.WatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a", CreationTimestamp: created},
		Spec: configv1alpha3.WatchRuleSpec{
			GitTargetRef: meta.LocalObjectReference{Name: "target"},
			Rules: []configv1alpha3.ResourceRule{
				{Resources: []string{"secrets"}, ObjectSelector: &configv1alpha3.ObjectSelector{
					MatchExpressions: []configv1alpha3.ObjectSelectorRequirement{
						{Key: "team", Operator: metav1.LabelSelectorOpIn, Values: []string{"b", "a"}},
					},
				}},
				{Resources: []string{"configmaps"}},
				// Unreachable through the compile path, which refuses the whole rule first; the
				// store's own fail-safe leaves the item out rather than selecting everything.
				{Resources: []string{"services"}, ObjectSelector: &configv1alpha3.ObjectSelector{
					MatchExpressions: []configv1alpha3.ObjectSelectorRequirement{
						{Key: "team", Operator: metav1.LabelSelectorOpIn},
					},
				}},
			},
		},
	}
	store.AddOrUpdateWatchRule(rule, ownNamespaceScope(rule), "target", "team-a", "p", "team-a", "main", "path")

	compiled, ok := store.GetWatchRule(types.NamespacedName{Name: "r", Namespace: "team-a"})
	if !ok {
		t.Fatal("rule not compiled")
	}
	if !compiled.CreatedAt.Equal(&created) {
		t.Errorf("CreatedAt = %v, want %v", compiled.CreatedAt, created)
	}
	if len(compiled.ResourceRules) != 2 {
		t.Fatalf("compiled %d items, want 2 (the invalid one left out)", len(compiled.ResourceRules))
	}
	if got := compiled.ResourceRules[0].LabelSelector; got != "team in (a,b)" {
		t.Errorf("secrets selector = %q, want %q", got, "team in (a,b)")
	}
	if got := compiled.ResourceRules[1].LabelSelector; got != "" {
		t.Errorf("configmaps selector = %q, want empty", got)
	}
}

func TestAddOrUpdateClusterWatchRule_CompilesTheCanonicalObjectSelector(t *testing.T) {
	store := NewStore()
	rule := configv1alpha3.ClusterWatchRule{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: configv1alpha3.ClusterWatchRuleSpec{
			Rules: []configv1alpha3.ClusterResourceRule{{
				Resources:      []string{"namespaces"},
				ObjectSelector: &configv1alpha3.ObjectSelector{MatchLabels: map[string]string{"tenant": "x"}},
			}},
		},
	}
	store.AddOrUpdateClusterWatchRule(rule, "target", "ns", "p", "ns", "main", "path")

	rules := store.SnapshotClusterWatchRules()
	if len(rules) != 1 || len(rules[0].Rules) != 1 {
		t.Fatalf("unexpected compiled rules: %+v", rules)
	}
	if got := rules[0].Rules[0].LabelSelector; got != "tenant in (x)" {
		t.Errorf("selector = %q, want %q", got, "tenant in (x)")
	}
}
