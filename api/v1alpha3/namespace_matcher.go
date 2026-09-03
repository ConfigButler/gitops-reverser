// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NamespaceMatcher is the deny-by-default namespace-policy SHAPE this API uses where a field bounds
// "which namespaces". It carries an explicit name allow-list and a label selector, ORed: a
// namespace is admitted if it is listed OR its labels match.
//
// It is deny-by-default and the empty matcher is NOT "unrestricted": a matcher with neither names
// nor selector admits NOTHING. Its one use is authorization, and the fail-open reading is the
// catastrophic one, so an absent field means "no policy declared" (which admits nothing here) while
// a declared-but-empty one also admits nothing.
//
// One field uses it: ClusterProvider.spec.accessFrom, whose selector matches labels on Namespaces
// in the CONTROL cluster. It had a source-side twin, GitTarget.spec.allowedSourceNamespaces,
// evaluated against Namespace labels in another cluster; that twin and everything it forced (a
// three-valued verdict, a degradation path, source-cluster Namespace get/list/watch) were deleted.
// Matches therefore still takes the labels rather than fetching them, which is now merely
// convenient rather than load-bearing.
type NamespaceMatcher struct {
	// `*` is rejected because Kubernetes treats it as a LITERAL name: `names: ["*"]` would admit
	// nothing while reading as if it admitted everything. `selector: {}` is the "every namespace"
	// form, because it resolves live.

	// Names is an explicit allow-list of namespace names. Entries are namespace names (DNS-1123
	// labels), never patterns — `*` is rejected. To admit every namespace, declare `selector: {}`.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Names []string `json:"names,omitempty"`

	// Selector is a label selector matched against Namespace labels; a namespace whose labels
	// match is admitted. ORed with Names.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// MatchesName reports whether nsName is in the matcher's explicit Names allow-list.
//
// It is separate from Matches so the answer never depends on the labels when a name already
// admits. A nil matcher matches nothing.
func (m *NamespaceMatcher) MatchesName(nsName string) bool {
	if m == nil {
		return false
	}
	for _, n := range m.Names {
		if n == nsName {
			return true
		}
	}
	return false
}

// Matches reports whether a namespace (by name and by the labels it carries IN THE CLUSTER THIS
// FIELD DESCRIBES) is admitted. Names are checked before the selector, so the answer never depends
// on the labels when a name already admits. A malformed selector is returned as an error rather
// than a silent allow or a silent deny — it is a configuration mistake the operator must see.
// A nil matcher admits nothing.
func (m *NamespaceMatcher) Matches(nsName string, nsLabels map[string]string) (bool, error) {
	if m == nil {
		return false, nil
	}
	if m.MatchesName(nsName) {
		return true, nil
	}
	if m.Selector == nil {
		return false, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(m.Selector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(nsLabels)), nil
}
