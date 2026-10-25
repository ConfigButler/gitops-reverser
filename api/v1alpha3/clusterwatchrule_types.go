// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceScope names a Kubernetes resource's scope. It is a purely INTERNAL matching vocabulary
// with no field in any CRD: the resolver uses both constants to align a rule's selector with the
// discovered scope of each type. A WatchRule always resolves Namespaced records, a ClusterWatchRule
// always Cluster ones, and which kind you write is the whole of how scope is chosen.
type ResourceScope string

const (
	// ResourceScopeCluster indicates cluster-scoped resources (Nodes, ClusterRoles, etc.).
	ResourceScopeCluster ResourceScope = "Cluster"

	// ResourceScopeNamespaced indicates namespaced resources (Pods, Deployments, etc.).
	ResourceScopeNamespaced ResourceScope = "Namespaced"
)

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
	// GitTargetRef names the GitTarget that mirrors what this ClusterWatchRule selects. The
	// namespace is required, because a ClusterWatchRule has none to default to.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitTargetRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.namespace) && self.namespace != ''",message="spec.gitTargetRef.namespace is required: a ClusterWatchRule has no namespace to default to"
	GitTargetRef meta.NamespacedObjectReference `json:"gitTargetRef"`

	// Rules select the cluster-scoped resources to mirror. A resource matching any rule is
	// mirrored. A rule that resolves to no cluster-scoped type mirrors nothing; use a WatchRule for
	// namespaced resources.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterResourceRule `json:"rules"`
}

// There is no sourceNamespace: cluster-scoped objects have no namespace, so there is nothing to
// select.

// ClusterResourceRule selects cluster-scoped resources by type, and optionally by API group,
// version and label.
type ClusterResourceRule struct {
	// APIGroups are the API groups to match: "" is the core group, and "*" is every group, as in
	// ["rbac.authorization.k8s.io"] or ["*"]. Omitted, each resource name is looked up across the
	// served groups and must be served by exactly one.
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions are the versions to match, or "*" for every served version. Omitted, each
	// resource's preferred version is used.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources are the resource types to match, by plural name ("nodes", "clusterroles"), or "*"
	// for every type. Matching is exact and case-insensitive; partial wildcards such as "cluster*"
	// match nothing, and subresources ("nodes/status") are rejected.
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
	// Two rules on one GitTarget that select the same type must use the same
	// selector. The older rule keeps the type, and the newer one is refused
	// with ResourcesResolved=False, reason CollectionOverlap. A selector that
	// does not parse refuses the whole rule with ResourcesResolved=False,
	// reason InvalidObjectSelector.
	// +optional
	ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`
}

// ClusterWatchRuleStatus defines the observed state of ClusterWatchRule.
type ClusterWatchRuleStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are the latest observations of this ClusterWatchRule's state. Ready, Reconciling
	// and Stalled follow kstatus; ResourcesResolved and StreamsRunning say which check holds the
	// rule back.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams counts the types this ClusterWatchRule resolves, by watch state.
	// +optional
	Streams *StreamsStatus `json:"streams,omitempty"`
}

// Cluster-scoped objects have no namespace, so no namespace policy bounds them: this is
// cluster-global, limited only by its source credential's RBAC. Isolating tenants takes separate
// KubeProviders.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="GitTarget",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="ResourcesResolved",type=string,JSONPath=`.status.conditions[?(@.type=="ResourcesResolved")].status`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWatchRule selects cluster-scoped resources, such as Nodes, ClusterRoles and CRDs, for a
// GitTarget to mirror. The KubeProvider's grant admitting that GitTarget must list each type in
// clusterResources. Use a WatchRule for namespaced resources.
type ClusterWatchRule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ClusterWatchRule.
	// +required
	Spec ClusterWatchRuleSpec `json:"spec"`

	// status defines the observed state of ClusterWatchRule.
	// +optional
	Status ClusterWatchRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterWatchRuleList contains a list of ClusterWatchRule.
type ClusterWatchRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterWatchRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterWatchRule{}, &ClusterWatchRuleList{})
}
