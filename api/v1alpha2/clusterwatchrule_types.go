/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceScope defines the scope of resources.
// +kubebuilder:validation:Enum=Cluster;Namespaced
type ResourceScope string

const (
	// ResourceScopeCluster indicates cluster-scoped resources (Nodes, ClusterRoles, etc.).
	ResourceScopeCluster ResourceScope = "Cluster"

	// ResourceScopeNamespaced indicates namespaced resources (Pods, Deployments, etc.).
	ResourceScopeNamespaced ResourceScope = "Namespaced"
)

type NamespacedTargetReference struct {
	// API Group of the referent.
	// Kind of the referrer.
	// +kubebuilder:validation:Enum=configbutler.ai
	// +kubebuilder:default=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referrer.
	// Optional because this reference currently only supports a single kind (GitTarget).
	// Keeping it optional allows users to omit it while still benefiting from CRD defaulting.
	// +optional
	// +kubebuilder:validation:Enum=GitTarget
	// +kubebuilder:default=GitTarget
	Kind string `json:"kind,omitempty"`
	Name string `json:"name"`

	// Required because ClusterWatchRule has no namespace.
	// +required
	Namespace string `json:"namespace"`
}

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
	// TargetRef references the GitTarget to use.
	// Must specify namespace.
	// +required
	TargetRef NamespacedTargetReference `json:"targetRef"`

	// Rules define which resources to watch.
	// Multiple rules create a logical OR - a resource matching ANY rule is watched.
	// Each rule can specify cluster-scoped or namespaced resources.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterResourceRule `json:"rules"`
}

// ClusterResourceRule defines which resources to watch with scope control.
// Each rule independently specifies whether it watches cluster-scoped or
// namespaced resources.
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
	// +kubebuilder:validation:items:Pattern=`^[^/]*$`
	Resources []string `json:"resources"`

	// Scope defines whether this rule watches Cluster-scoped or Namespaced resources.
	// - "Cluster": For cluster-scoped resources (Nodes, ClusterRoles, CRDs, etc.).
	// - "Namespaced": For namespaced resources (Pods, Deployments, Secrets, etc.),
	//                 across all namespaces.
	// +required
	// +kubebuilder:validation:Enum=Cluster;Namespaced
	Scope ResourceScope `json:"scope"`
}

// ClusterWatchRuleStatus defines the observed state of ClusterWatchRule.
type ClusterWatchRuleStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the ClusterWatchRule's state.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams is the bounded stream-readiness roll-up for the types this rule resolves.
	// +optional
	Streams *WatchRuleStreamsStatus `json:"streams,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reconciling",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciling")].status`
// +kubebuilder:printcolumn:name="Stalled",type=string,JSONPath=`.status.conditions[?(@.type=="Stalled")].status`
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWatchRule watches resources across the entire cluster.
// It provides the ability to audit both cluster-scoped resources (Nodes, ClusterRoles, CRDs)
// and namespaced resources across multiple namespaces with per-rule filtering.
//
// Security model:
//   - ClusterWatchRule is cluster-scoped and requires cluster-admin permissions
//   - It references a GitTarget via targetRef (namespace required)
//   - Each rule can independently specify Cluster or Namespaced scope
//
// Use cases:
//   - Audit cluster infrastructure (Nodes, PersistentVolumes, StorageClasses)
//   - Audit RBAC changes (ClusterRoles, ClusterRoleBindings)
//   - Audit CRD installations and updates
//   - Audit resources across multiple namespaces (e.g., all production namespaces)
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
