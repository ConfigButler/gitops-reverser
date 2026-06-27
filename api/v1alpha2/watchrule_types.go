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
// Omitted API groups and versions are resolved from the served Kubernetes API surface.
// All fields except Resources are optional.
type ResourceRule struct {
	// Operations to watch. If empty, watches all operations (CREATE, UPDATE, DELETE).
	// Supports: CREATE, UPDATE, DELETE, or * (wildcard for all operations).
	// Examples:
	//   - ["CREATE", "UPDATE"] watches only creation and updates, ignoring deletions
	//   - ["*"] or [] watches all operations
	// +optional
	Operations []OperationType `json:"operations,omitempty"`

	// APIGroups to match. Empty string ("") matches the core API group.
	// If omitted, GitOps Reverser resolves the resource name across all served API groups.
	// Wildcards supported: "*" matches all groups.
	// Examples:
	//   - [""] matches core API (pods, services, configmaps)
	//   - ["apps"] matches apps API group (deployments, statefulsets)
	//   - ["", "apps"] matches both core and apps groups
	//   - ["*"] matches all groups
	//   - [] resolves a named resource only when it is served by one API group
	// +optional
	APIGroups []string `json:"apiGroups,omitempty"`

	// APIVersions to match. If empty, uses the preferred served version for each group/resource.
	// Wildcards supported: "*" matches all versions.
	// Examples:
	//   - ["v1"] matches only v1 version
	//   - ["v1", "v1beta1"] matches both versions
	//   - ["*"] matches all served versions
	//   - [] matches the preferred served version
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`

	// Resources to match (plural names like "pods", "configmaps").
	// This field is required and determines which resource types trigger this rule.
	// Wildcard semantics follow Kubernetes admission webhook patterns:
	//   - "*" matches all resources
	//   - "pods" matches exactly pods (case-insensitive)
	//
	// For custom resources, use the exact plural resource name and set apiGroups
	// when more than one served API group exposes that name.
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
}

// WatchRuleStatus defines the observed state of WatchRule.
type WatchRuleStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of an object's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Streams is the bounded stream-readiness roll-up for the types this rule resolves.
	// +optional
	Streams *WatchRuleStreamsStatus `json:"streams,omitempty"`
}

// WatchRuleStreamsStatus is a bounded roll-up of the stream-readiness state for the
// types a WatchRule or ClusterWatchRule resolves.
type WatchRuleStreamsStatus struct {
	// Summary is the display-only ready/total ratio.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Total is how many types this rule resolves.
	Total int32 `json:"total"`

	// Ready is how many resolved types are Streaming.
	Ready int32 `json:"ready"`

	// Replaying is how many resolved types are still replaying their initial events.
	Replaying int32 `json:"replaying"`

	// Blocked is how many resolved types cannot currently be watched.
	Blocked int32 `json:"blocked"`

	// PendingSample is a bounded sample of types not yet ready.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	PendingSample []string `json:"pendingSample,omitempty"`

	// ObservedTime is when this roll-up was last computed.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reconciling",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciling")].status`
// +kubebuilder:printcolumn:name="Stalled",type=string,JSONPath=`.status.conditions[?(@.type=="Stalled")].status`
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
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
