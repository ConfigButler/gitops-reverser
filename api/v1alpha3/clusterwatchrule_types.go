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
	// GitTargetRef names the GitTarget this rule feeds. A ClusterWatchRule has no namespace of its
	// own, so the namespace is required here rather than defaulted.
	// +required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="spec.gitTargetRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.namespace) && self.namespace != ''",message="spec.gitTargetRef.namespace is required: a ClusterWatchRule has no namespace to default to"
	GitTargetRef meta.NamespacedObjectReference `json:"gitTargetRef"`

	// Rules define which CLUSTER-SCOPED resources to watch.
	// Multiple rules create a logical OR - a resource matching ANY rule is watched.
	// A rule that resolves to no cluster-scoped type simply watches nothing; use a WatchRule with
	// spec.rules[].sourceNamespace for namespaced resources.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterResourceRule `json:"rules"`
}

// ClusterResourceRule defines which CLUSTER-SCOPED resources to watch. It deliberately has no
// sourceNamespace: cluster-scoped objects have no namespace, so there is nothing to select.
type ClusterResourceRule struct {
	// Operations to watch. If empty, watches all operations (CREATE, UPDATE, DELETE).
	// Supports: CREATE, UPDATE, DELETE, or * (wildcard for all operations).
	// Examples:
	//   - ["CREATE", "UPDATE"] watches only creation and updates
	//   - ["*"] or [] watches all operations
	// +optional
	Operations []OperationType `json:"operations,omitempty"`

	// APIGroups to match. Empty string ("") matches the core API group.
	// If omitted, GitOps Reverser resolves the resource name across all served API groups.
	// Wildcards supported: "*" matches all groups.
	// Examples:
	//   - [""] matches core API (nodes, namespaces)
	//   - ["rbac.authorization.k8s.io"] matches RBAC resources
	//   - ["*"] matches all groups
	//   - [] resolves a named resource only when it is served by one API group
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions to match. If empty, uses the preferred served version for each group/resource.
	// Wildcards supported: "*" matches all versions.
	// Examples:
	//   - ["v1"] matches only v1 version
	//   - ["*"] matches all served versions
	//   - [] matches the preferred served version
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources to match (plural names like "nodes", "clusterroles").
	// This field is required and determines which resource types trigger this rule.
	// Wildcard semantics follow Kubernetes admission webhook patterns:
	//   - "*" matches all resources
	//   - "nodes" matches exactly nodes
	//   - "pods" matches exactly pods (for namespaced scope)
	//
	// Note: Subresources cannot be added here. Values containing "/" (for example
	// "pods/log" or "pods/*") are rejected by the API because subresources are
	// not supported for list/watch snapshot planning. Prefix/suffix wildcards
	// like "pod*" or "*.example.com" are NOT supported. Use exact matches or the
	// "*" wildcard for broad matching.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:Pattern=`^[^/]*$`
	Resources []string `json:"resources"`
}

// ClusterWatchRuleStatus defines the observed state of ClusterWatchRule.
type ClusterWatchRuleStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the ClusterWatchRule's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams is the bounded stream-readiness roll-up for the types this rule resolves.
	// +optional
	Streams *WatchRuleStreamsStatus `json:"streams,omitempty"`
}

// Cluster-scoped objects have no namespace, so no namespace policy bounds them: this is
// cluster-global, limited only by its source credential's RBAC. Isolating tenants takes separate
// ClusterProviders.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.gitTargetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="GitTargetReady",type=string,JSONPath=`.status.conditions[?(@.type=="GitTargetReady")].status`,priority=1
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWatchRule selects CLUSTER-SCOPED resources on the source cluster its GitTarget mirrors
// from — Nodes, PersistentVolumes, StorageClasses, ClusterRoles, CRDs, and the like. Scope is
// carried by the rule KIND, so it has no per-rule scope choice and no source-namespace selection.
//
// It is cluster-scoped and requires cluster-admin permissions. Its gitTargetRef names a GitTarget
// (namespace required), whose namespace must be admitted by that target's ClusterProvider. To
// mirror NAMESPACED resources use a WatchRule in the tenant namespace and set
// spec.rules[].sourceNamespace, whose "*" reaches every namespace the source credential can read.
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
