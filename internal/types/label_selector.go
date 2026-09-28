// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// CanonicalLabelSelector validates a rule's objectSelector and returns its canonical string: the
// selector's identity for the LIST/WATCH request, the collection key, the watch cursor, resync
// coalescing, render fidelity and retention.
//
// Nil and empty both select everything and yield "". That is spelled out here because
// metav1.LabelSelectorAsSelector reads nil as "nothing", which on a mirror would be a sweep of
// every document in scope.
//
// The normalization is syntactic, never a Boolean simplification: matchLabels equality becomes a
// singleton In, In/NotIn values are sorted and deduplicated, duplicate requirements collapse, and
// requirements sort by their whole (key, operator, values) tuple. labels.Selector.String alone is
// not enough: its sort compares keys only, so `team in (a,b),team notin (b)` and its reverse would
// be two identities for one selection, and a reordered rule would restart its watch.
func CanonicalLabelSelector(selector *metav1.LabelSelector) (string, error) {
	if selector == nil || (len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0) {
		return "", nil
	}
	if _, err := metav1.LabelSelectorAsSelector(selector); err != nil {
		return "", fmt.Errorf("invalid objectSelector: %w", err)
	}
	reqs, err := selectorRequirements(selector)
	if err != nil {
		return "", err
	}
	sort.Slice(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.key != b.key {
			return a.key < b.key
		}
		if a.op != b.op {
			return a.op < b.op
		}
		return slices.Compare(a.values, b.values) < 0
	})
	rendered := make([]string, 0, len(reqs))
	for _, req := range reqs {
		r, err := labels.NewRequirement(req.key, req.op, req.values)
		if err != nil {
			return "", fmt.Errorf("invalid objectSelector: %w", err)
		}
		rendered = append(rendered, r.String())
	}
	return strings.Join(slices.Compact(rendered), ","), nil
}

// selectorRequirement is one requirement of a selector, in the normalized form it is sorted by.
type selectorRequirement struct {
	key    string
	op     selection.Operator
	values []string
}

// selectorRequirements normalizes a validated selector's requirements: matchLabels equality becomes
// a singleton In, and In/NotIn values are sorted and deduplicated.
func selectorRequirements(selector *metav1.LabelSelector) ([]selectorRequirement, error) {
	reqs := make([]selectorRequirement, 0, len(selector.MatchLabels)+len(selector.MatchExpressions))
	for key, value := range selector.MatchLabels {
		reqs = append(reqs, selectorRequirement{key: key, op: selection.In, values: []string{value}})
	}
	for _, expr := range selector.MatchExpressions {
		op, ok := selectorOperator(expr.Operator)
		if !ok {
			return nil, fmt.Errorf("invalid objectSelector: unsupported operator %q", expr.Operator)
		}
		req := selectorRequirement{key: expr.Key, op: op}
		if op == selection.In || op == selection.NotIn {
			values := append([]string(nil), expr.Values...)
			sort.Strings(values)
			req.values = slices.Compact(values)
		}
		reqs = append(reqs, req)
	}
	return reqs, nil
}

// selectorOperator maps a LabelSelector operator to the labels package's operator.
func selectorOperator(op metav1.LabelSelectorOperator) (selection.Operator, bool) {
	switch op {
	case metav1.LabelSelectorOpIn:
		return selection.In, true
	case metav1.LabelSelectorOpNotIn:
		return selection.NotIn, true
	case metav1.LabelSelectorOpExists:
		return selection.Exists, true
	case metav1.LabelSelectorOpDoesNotExist:
		return selection.DoesNotExist, true
	default:
		return "", false
	}
}
