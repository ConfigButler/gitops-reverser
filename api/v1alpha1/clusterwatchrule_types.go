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

package v1alpha1

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

// ClusterWatchRuleSpec defines the desired state of ClusterWatchRule.
type ClusterWatchRuleSpec struct {
	// GitRepoConfigRef references the GitRepoConfig to use.
	// Since ClusterWatchRule is cluster-scoped and GitRepoConfig is namespace-scoped,
	// both name and namespace must be specified.
	// +required
	GitRepoConfigRef NamespacedName `json:"gitRepoConfigRef"`

	// Rules define which resources to watch.
	// Multiple rules create a logical OR - a resource matching ANY rule is watched.
	// Each rule can specify cluster-scoped or namespaced resources.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterResourceRule `json:"rules"`
}

// ClusterResourceRule defines which resources to watch with scope control.
// Each rule can independently specify whether it watches cluster-scoped or
// namespaced resources, with optional namespace filtering for namespaced resources.
type ClusterResourceRule struct {
	// Operations to watch. If empty, watches all operations (CREATE, UPDATE, DELETE).
	// Supports: CREATE, UPDATE, DELETE, or * (wildcard for all operations).
	// Examples:
	//   - ["CREATE", "UPDATE"] watches only creation and updates
	//   - ["*"] or [] watches all operations
	// +optional
	Operations []OperationType `json:"operations,omitempty"`

	// APIGroups to match. Empty string ("") matches the core API group.
	// If empty, matches all API groups.
	// Wildcards supported: "*" matches all groups.
	// Examples:
	//   - [""] matches core API (nodes, namespaces)
	//   - ["rbac.authorization.k8s.io"] matches RBAC resources
	//   - ["*"] or [] matches all groups
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions to match. If empty, matches all versions.
	// Wildcards supported: "*" matches all versions.
	// Examples:
	//   - ["v1"] matches only v1 version
	//   - ["*"] or [] matches all versions
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources to match (plural names like "nodes", "clusterroles").
	// This field is required and determines which resource types trigger this rule.
	// Wildcard semantics follow Kubernetes admission webhook patterns:
	//   - "*" matches all resources
	//   - "nodes" matches exactly nodes
	//   - "pods" matches exactly pods (for namespaced scope)
	// +required
	// +kubebuilder:validation:MinItems=1
	Resources []string `json:"resources"`

	// Scope defines whether this rule watches Cluster-scoped or Namespaced resources.
	// - "Cluster": For cluster-scoped resources (Nodes, ClusterRoles, CRDs, etc.).
	//              The namespaceSelector field is ignored for cluster-scoped rules.
	// - "Namespaced": For namespaced resources (Pods, Deployments, Secrets, etc.).
	//                 Optionally filtered by namespaceSelector.
	//                 If namespaceSelector is omitted, watches resources in ALL namespaces.
	// +required
	// +kubebuilder:validation:Enum=Cluster;Namespaced
	Scope ResourceScope `json:"scope"`

	// NamespaceSelector filters which namespaces to watch.
	// Only evaluated when Scope is "Namespaced".
	// If omitted for Namespaced scope, watches resources in ALL namespaces.
	// If specified, only watches resources in namespaces matching the selector.
	// Ignored when Scope is "Cluster".
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// ClusterWatchRuleStatus defines the observed state of ClusterWatchRule.
type ClusterWatchRuleStatus struct {
	// Conditions represent the latest available observations of the ClusterWatchRule's state.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="GitRepoConfig",type=string,JSONPath=`.spec.gitRepoConfigRef.name`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.gitRepoConfigRef.namespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterWatchRule watches resources across the entire cluster.
// It provides the ability to audit both cluster-scoped resources (Nodes, ClusterRoles, CRDs)
// and namespaced resources across multiple namespaces with per-rule filtering.
//
// Security model:
//   - ClusterWatchRule is cluster-scoped and requires cluster-admin permissions
//   - Referenced GitRepoConfig must have accessPolicy.allowClusterRules set to true
//   - Each rule can independently specify Cluster or Namespaced scope
//   - Namespaced rules can optionally filter by namespace labels
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
