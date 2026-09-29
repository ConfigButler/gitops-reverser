// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectSelectors returns every rule item's objectSelector, index-aligned with Rules.
func (s *WatchRuleSpec) ObjectSelectors() []*metav1.LabelSelector {
	out := make([]*metav1.LabelSelector, 0, len(s.Rules))
	for i := range s.Rules {
		out = append(out, s.Rules[i].ObjectSelector)
	}
	return out
}

// ObjectSelectors returns every rule item's objectSelector, index-aligned with Rules.
func (s *ClusterWatchRuleSpec) ObjectSelectors() []*metav1.LabelSelector {
	out := make([]*metav1.LabelSelector, 0, len(s.Rules))
	for i := range s.Rules {
		out = append(out, s.Rules[i].ObjectSelector)
	}
	return out
}
