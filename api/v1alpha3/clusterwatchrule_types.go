// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceScope names a Kubernetes resource's scope. It is an INTERNAL matching vocabulary: the
// resolver uses both constants to align a rule's selector with the discovered scope of each type
// (a WatchRule always resolves Namespaced records, a ClusterWatchRule always Cluster ones). The
// only field that still exposes it — ClusterResourceRule.scope — is narrowed to Cluster alone, so
// "Namespaced" is no longer a public choice anywhere in the API.
type ResourceScope string

const (
	// ResourceScopeCluster indicates cluster-scoped resources (Nodes, ClusterRoles, etc.).
	ResourceScopeCluster ResourceScope = "Cluster"

	// ResourceScopeNamespaced indicates namespaced resources (Pods, Deployments, etc.).
	ResourceScopeNamespaced ResourceScope = "Namespaced"
)

type NamespacedTargetReference struct {
	// API Group of the referent.
	// +kubebuilder:validation:Enum=configbutler.ai
	// +kubebuilder:default=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// Optional because this reference currently only supports a single kind (GitTarget).
	// Keeping it optional allows users to omit it while still benefiting from CRD defaulting.
	// +optional
	// +kubebuilder:validation:Enum=GitTarget
	// +kubebuilder:default=GitTarget
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Required because ClusterWatchRule has no namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
	// TargetRef references the GitTarget to use.
	// Must specify namespace.
	// +required
	TargetRef NamespacedTargetReference `json:"targetRef"`

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

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// The field is retained in the schema purely so that re-applying a manifest that still says
	// "Namespaced" FAILS. Deleting it outright would be worse and silent twice over: CRD pruning
	// happens on write, so the value would be dropped without an error and the rule would quietly
	// stop mirroring namespaced objects; and a stored pre-release object would keep its value in
	// etcd with no Go field left to read, leaving the controller nothing to refuse. The narrowed
	// enum rejects it at admission, and the compile path refuses a stored value.

	// Scope is REMOVED as a choice: a ClusterWatchRule is cluster-scoped only, so "Cluster" is the
	// only accepted value and also the default, making the field omittable. To watch NAMESPACED
	// resources, use a WatchRule in the tenant namespace and set spec.rules[].sourceNamespace.
	//
	// Deprecated: ClusterWatchRule is cluster-scope-only; use WatchRule with
	// spec.rules[].sourceNamespace for namespaced resources. Removed one release from now, or at
	// v1beta1.
	// +optional
	// +kubebuilder:default=Cluster
	// +kubebuilder:validation:Enum=Cluster
	Scope ResourceScope `json:"scope,omitempty"`
}

// DeclaresNamespacedScope reports whether a STORED ClusterWatchRule still selects namespaced
// resources through the removed scope choice. Admission rejects the value, but an object written
// before this release keeps it in etcd, so the compile path must refuse it rather than let the
// rule resolve as if it had asked for cluster scope.
//
// It keys on the STORED value, not on what the selector happens to resolve: `resources: ["*"]`
// legitimately resolves cluster-scoped records, so inferring the refusal from the resolution would
// be ambiguous exactly where it matters.
func (s *ClusterWatchRuleSpec) DeclaresNamespacedScope() bool {
	for i := range s.Rules {
		if s.Rules[i].Scope != "" && s.Rules[i].Scope != ResourceScopeCluster {
			return true
		}
	}
	return false
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

// Design rationale, kept out of the generated CRD description by the blank line below.
//
// Cluster-scoped objects have no namespace, so GitTarget.spec.allowedSourceNamespaces is neither
// consulted nor a bound for them: a ClusterWatchRule is intentionally cluster-global and is limited
// only by its source credential's Kubernetes RBAC. Isolating cluster-scoped objects between tenants
// therefore takes separate credentials/ClusterProviders, not a namespace allow-list.

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
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
// It is cluster-scoped and requires cluster-admin permissions. Its targetRef names a GitTarget
// (namespace required), whose namespace must be admitted by that target's ClusterProvider. To
// mirror NAMESPACED resources use a WatchRule in the tenant namespace and set
// spec.rules[].sourceNamespace, whose "*" reaches every namespace the GitTarget admits.
type ClusterWatchRule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

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
