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

// OperationType specifies the type of operation that triggers a watch event.
// +kubebuilder:validation:Enum=CREATE;UPDATE;DELETE;*
type OperationType string

const (
	// OperationCreate matches resource creation events.
	OperationCreate OperationType = "CREATE"
	// OperationUpdate matches resource update events.
	OperationUpdate OperationType = "UPDATE"
	// OperationDelete matches resource deletion events.
	OperationDelete OperationType = "DELETE"
	// OperationAll matches all operation types.
	OperationAll OperationType = "*"
)

type LocalTargetReference struct {
	// API Group of the referent.
	// +kubebuilder:default=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// Optional because this reference currently only supports a single kind (GitTarget).
	// Keeping it optional allows users to omit it while still benefiting from CRD defaulting.
	// +optional
	// +kubebuilder:validation:Enum=GitTarget
	// +kubebuilder:default=GitTarget
	Kind string `json:"kind,omitempty"`
	Name string `json:"name"`
}

// WatchRuleSpec defines the desired state of WatchRule.
// WatchRule watches resources ONLY within its own namespace.
type WatchRuleSpec struct {
	// TargetRef references the GitTarget to use.
	// Must be in the same namespace.
	// +required
	TargetRef LocalTargetReference `json:"targetRef"`

	// Rules define which resources to watch within this namespace.
	// Multiple rules create a logical OR - a resource matching ANY rule is watched.
	// Each rule can specify operations, API groups, versions, and resource types.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []ResourceRule `json:"rules"`
}

// ResourceRule defines a set of namespaced resources to watch.
// This follows Kubernetes admission control semantics but simplified for our use case.
// All fields except Resources are optional and default to matching all when not specified.
type ResourceRule struct {
	// Operations to watch. If empty, watches all operations (CREATE, UPDATE, DELETE).
	// Supports: CREATE, UPDATE, DELETE, or * (wildcard for all operations).
	// Examples:
	//   - ["CREATE", "UPDATE"] watches only creation and updates, ignoring deletions
	//   - ["*"] or [] watches all operations
	// +optional
	Operations []OperationType `json:"operations,omitempty"`

	// APIGroups to match. Empty string ("") matches the core API group.
	// If empty, matches all API groups.
	// Wildcards supported: "*" matches all groups.
	// Examples:
	//   - [""] matches core API (pods, services, configmaps)
	//   - ["apps"] matches apps API group (deployments, statefulsets)
	//   - ["", "apps"] matches both core and apps groups
	//   - ["*"] or [] matches all groups
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions to match. If empty, matches all versions.
	// Wildcards supported: "*" matches all versions.
	// Examples:
	//   - ["v1"] matches only v1 version
	//   - ["v1", "v1beta1"] matches both versions
	//   - ["*"] or [] matches all versions
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources to match (plural names like "pods", "configmaps").
	// This field is required and determines which resource types trigger this rule.
	// Wildcard semantics follow Kubernetes admission webhook patterns:
	//   - "*" matches all resources
	//   - "pods" matches exactly pods (case-insensitive)
	//   - "pods/*" matches all pod subresources (e.g., pods/log, pods/status)
	//   - "pods/log" matches specific subresource
	//
	// For custom resources, use exact group-qualified names:
	//   - "myapps.example.com" matches MyApp CRD
	//
	// Note: Prefix/suffix wildcards like "pod*" or "*.example.com" are NOT supported.
	// Use exact matches or the "*" wildcard for broad matching.
	// +required
	// +kubebuilder:validation:MinItems=1
	Resources []string `json:"resources"`
}

// WatchRuleStatus defines the observed state of WatchRule.
type WatchRuleStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Destination",type=string,JSONPath=`.spec.destinationRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WatchRule watches namespaced resources within its own namespace.
// It provides fine-grained control over which resources trigger Git commits,
// with filtering by operation type, API group, version, and labels.
//
// Security model:
//   - WatchRule is namespace-scoped and can only watch resources in its own namespace
//   - Use ClusterWatchRule for watching cluster-scoped resources (Nodes, ClusterRoles, etc.)
//   - RBAC controls who can create/modify WatchRules per namespace
type WatchRule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of WatchRule
	// +required
	Spec WatchRuleSpec `json:"spec"`

	// status defines the observed state of WatchRule
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
