// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"fmt"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WatchRuleSpec defines the desired state of WatchRule.
type WatchRuleSpec struct {
	// GitTargetRef names the GitTarget, in this namespace, that mirrors what this
	// WatchRule selects.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitTargetRef.name must not be empty"
	GitTargetRef meta.LocalObjectReference `json:"gitTargetRef"`

	// Rules select the resources to mirror. A resource matching any rule is
	// mirrored. Each rule names resource types, optionally narrowed by API group,
	// version, source namespace and labels. The GitTarget's spec.prune.mode
	// decides whether a removal on the cluster deletes the Git document.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ResourceRule `json:"rules"`
}

// ResourceRule selects namespaced resources by type, and optionally by API
// group, version, source namespace and label.
type ResourceRule struct {
	// APIGroups are the API groups to match: "" is the core group, and "*" is
	// every group, as in ["", "apps"] or ["*"]. Omitted, each resource name is
	// looked up across the served groups and must be served by exactly one.
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions are the versions to match, or "*" for every served version.
	// Omitted, each resource's preferred version is used. The built-in Git path
	// has no version, so two versions of one object share a file unless the
	// GitTarget's placement template includes {version}.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources are the resource types to match, by plural name ("configmaps",
	// "deployments"), or "*" for every type. Matching is exact and
	// case-insensitive; partial wildcards such as "pod*" match nothing, and
	// subresources ("pods/log") are rejected. For a custom resource whose name
	// more than one group serves, set apiGroups.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:Pattern=`^[^/]*$`
	Resources []string `json:"resources"`

	// ObjectSelector mirrors only the objects whose labels match, as a
	// standard label selector. Omitted or {} selects every object.
	//
	// An object that stops matching is handled like a deleted one: the
	// GitTarget's spec.prune.mode decides whether its document is removed.
	// With prune mode "Always", a resync also removes documents of the type
	// that the selector does not match.
	//
	// Two rules on one GitTarget that select the same type in overlapping
	// namespaces must use the same selector. The older rule keeps the type,
	// and the newer one is refused with ResourcesResolved=False, reason
	// CollectionOverlap. A selector that does not parse refuses the whole rule
	// with ResourcesResolved=False, reason InvalidObjectSelector.
	// +optional
	ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`

	// "*" is one cluster-wide list and watch rather than a set of per-namespace ones, so its bound
	// is the credential's own RBAC and nothing else.
	//
	// A denied explicit name refuses the WHOLE WatchRule rather than trimming that item: mirroring
	// two of the three namespaces a rule asked for is worse than a loud failure.
	//
	// A cluster-wide collection is a PEER of a named-namespace collection on the same type, never a
	// replacement: collapsing the two once widened a named rule's stream to every namespace its
	// credential could read (see CollectionKey in internal/types/collection.go). A target carrying
	// both therefore runs two streams over overlapping objects, and that is correct.

	// SourceNamespace is the source-cluster namespace this rule reads: omitted for
	// this WatchRule's own namespace, a name for another one, or "*" for every
	// namespace the KubeProvider's credential can read. Git placement follows each
	// object's own namespace, not this field.
	//
	// The KubeProvider's spec.allowedGitTargets must allow the namespace, through
	// the sourceNamespaces of a grant admitting the GitTarget; "*" needs "*"
	// there. One rule outside the grants refuses the whole WatchRule
	// (SourceNamespaceNotAllowed), checked when it reconciles.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(\*|[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`
	SourceNamespace string `json:"sourceNamespace,omitempty"`
}

// SourceNamespaceWildcard is the literal rules[].sourceNamespace token meaning "every source
// namespace the GitTarget's credential can read", compiled to ONE cluster-wide list and watch.
//
// It kept its spelling and changed its meaning: it used to resolve live through
// GitTarget.spec.allowedSourceNamespaces into a concrete set. That field is gone, so the token is
// bounded by source-cluster RBAC alone and is refused unless the KubeProvider's grant gives
// sourceNamespaces "*". See docs/design/source-scope-simplification.md, which is the
// definition of record.
const SourceNamespaceWildcard = "*"

// EffectiveSourceNamespace is the source-cluster namespace this ITEM names, given the namespace of
// the WatchRule that carries it: spec.rules[].sourceNamespace when set, and the rule's OWN
// namespace otherwise. For a wildcard item it returns "*", which is not a namespace name: the
// caller compiles it to the cluster-wide collection (the empty namespace) rather than watching it.
//
// It is controller logic rather than an API-server default because an apiserver default cannot
// refer to metadata.namespace.
func (r *ResourceRule) EffectiveSourceNamespace(ruleNamespace string) string {
	if r.SourceNamespace != "" {
		return r.SourceNamespace
	}
	return ruleNamespace
}

// IsSourceNamespaceWildcard reports whether this item asks for every namespace its credential can
// read, as one cluster-wide stream.
func (r *ResourceRule) IsSourceNamespaceWildcard() bool {
	return r.SourceNamespace == SourceNamespaceWildcard
}

// DescribeSourceNamespace renders this item's requested source namespace for an operator-facing
// message. An omitted value is spelled out rather than shown as an empty string.
func (r *ResourceRule) DescribeSourceNamespace(ruleNamespace string) string {
	if r.SourceNamespace == "" {
		return fmt.Sprintf("%q (omitted; the WatchRule's own namespace)", ruleNamespace)
	}
	return fmt.Sprintf("%q", r.SourceNamespace)
}

// WatchRuleStatus defines the observed state of WatchRule.
type WatchRuleStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this WatchRule's state. Ready,
	// Reconciling and Stalled follow kstatus; ResourcesResolved,
	// SourceNamespaceAuthorized and StreamsRunning say which check holds the rule
	// back.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams counts the types this WatchRule resolves, by watch state.
	// +optional
	Streams *StreamsStatus `json:"streams,omitempty"`
}

// Deny-by-default and re-evaluated on EVERY reconcile, so a policy tightened after a rule was
// accepted revokes it rather than grandfathering it.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="SourceAuthorized",type=string,JSONPath=`.status.conditions[?(@.type=="SourceNamespaceAuthorized")].status`,priority=1
// +kubebuilder:printcolumn:name="ResourcesResolved",type=string,JSONPath=`.status.conditions[?(@.type=="ResourcesResolved")].status`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WatchRule selects namespaced resources for a GitTarget in its namespace to
// mirror, by type, API group, version, source namespace and label. Each rule
// reads one source namespace: this WatchRule's own by default. Use a
// ClusterWatchRule for cluster-scoped types.
type WatchRule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of WatchRule.
	// +required
	Spec WatchRuleSpec `json:"spec"`

	// status defines the observed state of WatchRule.
	// +optional
	Status WatchRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WatchRuleList contains a list of WatchRule.
type WatchRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []WatchRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WatchRule{}, &WatchRuleList{})
}
