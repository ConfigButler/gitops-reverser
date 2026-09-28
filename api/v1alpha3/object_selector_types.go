// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectSelector has the wire shape of a Kubernetes label selector (metav1.LabelSelector). It is its
// own type so the schema can bound it and enumerate its operators, which the upstream type's schema
// does not; the rule compiler and the admission webhook check what a schema cannot express.

// ObjectSelector selects objects by label, with the semantics of a Kubernetes label selector. The
// requirements are ANDed. An empty selector selects every object.
type ObjectSelector struct {
	// MatchLabels selects objects carrying every listed label with exactly that value.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// MatchExpressions are set-based requirements.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	MatchExpressions []ObjectSelectorRequirement `json:"matchExpressions,omitempty"`
}

// ObjectSelectorRequirement is one set-based label requirement.
// +kubebuilder:validation:XValidation:rule="(self.operator in ['In', 'NotIn']) == (has(self.values) && size(self.values) > 0)",message="values must be non-empty for In and NotIn, and empty for Exists and DoesNotExist"
type ObjectSelectorRequirement struct {
	// Key is the label key the requirement applies to.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`
	Key string `json:"key"`

	// Operator relates the key to the values: In, NotIn, Exists, or DoesNotExist.
	// +required
	// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
	Operator metav1.LabelSelectorOperator `json:"operator"`

	// Values are the label values for In and NotIn, which need at least one; Exists and
	// DoesNotExist take none.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	Values []string `json:"values,omitempty"`
}

// LabelSelector returns the selector as the Kubernetes type the label machinery reads. Nil stays
// nil, which callers read as "every object".
func (s *ObjectSelector) LabelSelector() *metav1.LabelSelector {
	if s == nil {
		return nil
	}
	out := &metav1.LabelSelector{MatchLabels: s.MatchLabels}
	for _, r := range s.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, metav1.LabelSelectorRequirement{
			Key: r.Key, Operator: r.Operator, Values: r.Values,
		})
	}
	return out
}

// ObjectSelectors returns every rule item's objectSelector as the Kubernetes type, index-aligned
// with Rules.
func (s *WatchRuleSpec) ObjectSelectors() []*metav1.LabelSelector {
	out := make([]*metav1.LabelSelector, 0, len(s.Rules))
	for i := range s.Rules {
		out = append(out, s.Rules[i].ObjectSelector.LabelSelector())
	}
	return out
}

// ObjectSelectors returns every rule item's objectSelector as the Kubernetes type, index-aligned
// with Rules.
func (s *ClusterWatchRuleSpec) ObjectSelectors() []*metav1.LabelSelector {
	out := make([]*metav1.LabelSelector, 0, len(s.Rules))
	for i := range s.Rules {
		out = append(out, s.Rules[i].ObjectSelector.LabelSelector())
	}
	return out
}
