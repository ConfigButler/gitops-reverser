// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NamespaceMatcher is the one deny-by-default namespace-policy SHAPE this API uses wherever a
// field bounds "which namespaces". It carries an explicit name allow-list and a label selector,
// ORed: a namespace is admitted if it is listed OR its labels match.
//
// It is deny-by-default and the empty matcher is NOT "unrestricted": a matcher with neither names
// nor selector admits NOTHING. Every use of this shape is authorization, and the fail-open reading
// is the catastrophic one — so an absent field means "no policy declared" (which each call site
// interprets in its own legacy terms) while a declared-but-empty one means "admit nothing".
//
// Two fields use it, and they mean namespaces in DIFFERENT clusters — which is exactly why the
// shape is shared but the fields are not:
//
//   - ClusterProvider.spec.allowedNamespaces — control-cluster namespaces that may create a
//     GitTarget using the provider. Selector labels come from the CONTROL cluster.
//   - GitTarget.spec.allowedSourceNamespaces — source-cluster namespaces that may be mirrored
//     into this target, by any rule kind. Selector labels come from the SOURCE cluster.
//
// Because the two clusters differ, the LABEL half cannot be evaluated by one shared helper: only
// the caller knows which cluster's Namespace labels to read. Matches therefore takes the labels
// rather than fetching them, and MatchesName exists so an exact-name policy stays answerable when
// the labels cannot be read at all (see the source-scope service's degradation path).
type NamespaceMatcher struct {
	// Names is an explicit allow-list of namespace names.
	// +optional
	// +listType=set
	Names []string `json:"names,omitempty"`

	// Selector is a label selector matched against Namespace labels; a namespace whose labels
	// match is admitted. ORed with Names.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// MatchesName reports whether nsName is in the matcher's explicit Names allow-list.
//
// It is separate from Matches on purpose: the name half needs NO Namespace read, so a policy that
// admits by name keeps working against a cluster whose Namespace list/watch is Forbidden. Callers
// that can fail to read labels must consult this FIRST and only then fall through to the selector.
// A nil matcher matches nothing.
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

// HasSelector reports whether the matcher declares a label selector, i.e. whether evaluating it
// requires reading the Namespace's labels in that field's own cluster.
func (m *NamespaceMatcher) HasSelector() bool {
	return m != nil && m.Selector != nil
}

// Declared reports whether a policy exists at all. A nil matcher is "no policy declared"; a
// non-nil one is a declared policy even when it is empty (and an empty declared policy admits
// nothing). The distinction is load-bearing: an absent field keeps a caller's legacy scope, while
// a declared one is exhaustive.
func (m *NamespaceMatcher) Declared() bool {
	return m != nil
}

// SelectorAdmits reports whether the matcher's SELECTOR half alone — ignoring Names — admits a
// namespace carrying these labels. It exists for ENUMERATION: expanding a wildcard means asking
// this of every namespace in a snapshot, which Matches cannot do because its name check would
// short-circuit per candidate.
//
// A nil matcher or a nil selector admits nothing; a present-but-EMPTY selector admits everything.
// That asymmetry is not incidental — LabelSelectorAsSelector returns labels.Nothing() for nil and
// labels.Everything() for an empty selector, which is exactly the absent-versus-declared
// distinction this type is built around, and `selector: {}` is the deliberate "every namespace"
// declaration.
func (m *NamespaceMatcher) SelectorAdmits(nsLabels map[string]string) (bool, error) {
	if m == nil || m.Selector == nil {
		return false, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(m.Selector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(nsLabels)), nil
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
