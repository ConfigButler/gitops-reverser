// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func expr(key string, op metav1.LabelSelectorOperator, values ...string) metav1.LabelSelectorRequirement {
	return metav1.LabelSelectorRequirement{Key: key, Operator: op, Values: values}
}

func TestCanonicalLabelSelector_SelectEverything(t *testing.T) {
	for name, selector := range map[string]*metav1.LabelSelector{
		"omitted": nil,
		"empty":   {},
		"empty maps": {
			MatchLabels:      map[string]string{},
			MatchExpressions: []metav1.LabelSelectorRequirement{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CanonicalLabelSelector(selector)
			if err != nil || got != "" {
				t.Fatalf("CanonicalLabelSelector = %q, %v; want \"\", nil", got, err)
			}
		})
	}
}

// Every pair here is one selection written two ways. None may produce two identities, because
// an identity change restarts the watch and could read as an overlap refusal.
func TestCanonicalLabelSelector_EquivalentFormsShareOneIdentity(t *testing.T) {
	cases := map[string][2]*metav1.LabelSelector{
		"requirement order on one key (ByKey.Less compares keys only)": {
			{MatchExpressions: []metav1.LabelSelectorRequirement{
				expr("team", metav1.LabelSelectorOpIn, "a", "b"),
				expr("team", metav1.LabelSelectorOpNotIn, "b"),
			}},
			{MatchExpressions: []metav1.LabelSelectorRequirement{
				expr("team", metav1.LabelSelectorOpNotIn, "b"),
				expr("team", metav1.LabelSelectorOpIn, "a", "b"),
			}},
		},
		"reordered values": {
			{MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "b", "a")}},
			{MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "a", "b")}},
		},
		"duplicate values": {
			{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					expr("team", metav1.LabelSelectorOpIn, "a", "a", "b"),
				},
			},
			{MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "a", "b")}},
		},
		"duplicate requirements": {
			{MatchExpressions: []metav1.LabelSelectorRequirement{
				expr("tier", metav1.LabelSelectorOpExists),
				expr("tier", metav1.LabelSelectorOpExists),
			}},
			{MatchExpressions: []metav1.LabelSelectorRequirement{expr("tier", metav1.LabelSelectorOpExists)}},
		},
		"matchLabels equals singleton In": {
			{MatchLabels: map[string]string{"team": "a"}},
			{MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "a")}},
		},
		"matchLabels duplicated by an expression": {
			{
				MatchLabels:      map[string]string{"team": "a"},
				MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "a")},
			},
			{MatchLabels: map[string]string{"team": "a"}},
		},
		"requirement order across keys": {
			{MatchExpressions: []metav1.LabelSelectorRequirement{
				expr("b", metav1.LabelSelectorOpDoesNotExist),
				expr("a", metav1.LabelSelectorOpExists),
			}},
			{MatchExpressions: []metav1.LabelSelectorRequirement{
				expr("a", metav1.LabelSelectorOpExists),
				expr("b", metav1.LabelSelectorOpDoesNotExist),
			}},
		},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			first, err := CanonicalLabelSelector(pair[0])
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			second, err := CanonicalLabelSelector(pair[1])
			if err != nil {
				t.Fatalf("second: %v", err)
			}
			if first != second {
				t.Fatalf("equivalent selectors differ: %q vs %q", first, second)
			}
		})
	}
}

func TestCanonicalLabelSelector_DifferentSelectionsDiffer(t *testing.T) {
	a, _ := CanonicalLabelSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}})
	ab, _ := CanonicalLabelSelector(&metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpIn, "a", "b")},
	})
	notA, _ := CanonicalLabelSelector(&metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{expr("team", metav1.LabelSelectorOpNotIn, "a")},
	})
	if a == ab || a == notA || ab == notA {
		t.Fatalf("distinct selections collapsed: %q %q %q", a, ab, notA)
	}
}

// The canonical string is also the request's labelSelector, so the API server must parse it to the
// same selection the rule declared.
func TestCanonicalLabelSelector_ParsesToTheDeclaredSelection(t *testing.T) {
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"internal.cozystack.io/tenantresource": "true"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", metav1.LabelSelectorOpIn, "b", "a"),
			expr("team", metav1.LabelSelectorOpNotIn, "b"),
			expr("tier", metav1.LabelSelectorOpExists),
			expr("legacy", metav1.LabelSelectorOpDoesNotExist),
		},
	}
	canonical, err := CanonicalLabelSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	want := "internal.cozystack.io/tenantresource in (true),!legacy,team in (a,b),team notin (b),tier"
	if canonical != want {
		t.Fatalf("canonical = %q, want %q", canonical, want)
	}
	parsed, err := labels.Parse(canonical)
	if err != nil {
		t.Fatalf("labels.Parse(%q): %v", canonical, err)
	}
	declared, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []labels.Set{
		{"internal.cozystack.io/tenantresource": "true", "team": "a", "tier": "x"},
		{"internal.cozystack.io/tenantresource": "true", "team": "b", "tier": "x"},
		{"internal.cozystack.io/tenantresource": "true", "team": "a"},
		{"internal.cozystack.io/tenantresource": "true", "team": "a", "tier": "x", "legacy": "y"},
		{"team": "a", "tier": "x"},
	} {
		if parsed.Matches(set) != declared.Matches(set) {
			t.Fatalf("canonical %q and declared selector disagree on %v", canonical, set)
		}
	}
}

func TestCanonicalLabelSelector_InvalidIsRefused(t *testing.T) {
	for name, selector := range map[string]*metav1.LabelSelector{
		"In without values": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", metav1.LabelSelectorOpIn),
		}},
		"Exists with values": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", metav1.LabelSelectorOpExists, "a"),
		}},
		"unknown operator": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", "Gt", "1"),
		}},
		"invalid key":   {MatchLabels: map[string]string{"bad key": "a"}},
		"invalid value": {MatchLabels: map[string]string{"team": "not valid!"}},
		"empty expression key": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("", metav1.LabelSelectorOpExists),
		}},
		"invalid expression key": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("bad key", metav1.LabelSelectorOpExists),
		}},
		"invalid expression value": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", metav1.LabelSelectorOpIn, "not valid!"),
		}},
		"expression value longer than 63": {MatchExpressions: []metav1.LabelSelectorRequirement{
			expr("team", metav1.LabelSelectorOpIn, strings.Repeat("a", 64)),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := CanonicalLabelSelector(selector); err == nil {
				t.Fatalf("CanonicalLabelSelector = %q, nil; want an error", got)
			}
		})
	}
}
