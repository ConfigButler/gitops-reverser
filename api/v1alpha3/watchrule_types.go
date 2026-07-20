// SPDX-License-Identifier: Apache-2.0

package v1alpha3

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
	// +kubebuilder:validation:Enum=configbutler.ai
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
}

// WatchRuleSpec defines the desired state of WatchRule.
// WatchRule watches resources in ONE namespace of its GitTarget's source cluster: its own
// namespace by default, or the namespace spec.sourceNamespace names when authorized.
type WatchRuleSpec struct {
	// TargetRef references the GitTarget to use.
	// Must be in the same namespace.
	// +required
	TargetRef LocalTargetReference `json:"targetRef"`

	// SourceNamespace is the namespace to watch IN THE SOURCE CLUSTER the referenced GitTarget
	// mirrors from. Omitted, it is this WatchRule's own namespace — the legacy behavior, which
	// needs no authorization as long as the GitTarget declares no allowedSourceNamespaces policy.
	//
	// Naming a DIFFERENT namespace requires all three of:
	//
	//  1. the GitTarget's namespace is admitted by its ClusterProvider (spec.allowedNamespaces);
	//  2. that ClusterProvider sets spec.allowWatchRuleSourceNamespaceOverride; and
	//  3. the GitTarget's spec.allowedSourceNamespaces admits this namespace.
	//
	// The outcome is published as the SourceNamespaceAuthorized condition. Once the GitTarget
	// declares a policy that policy is exhaustive, so even an OMITTED sourceNamespace is then
	// checked against it — the rule's own namespace gets no implicit carve-out.
	//
	// This changes only which namespace is WATCHED. It never changes where objects are written:
	// Git placement follows each mirrored object's OWN namespace, so a rule in "tenant-acme"
	// watching "repo-config" writes under repo-config/…, not tenant-acme/….
	// +optional
	// +kubebuilder:validation:MinLength=1
	SourceNamespace string `json:"sourceNamespace,omitempty"`

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
	//
	// Multi-version note: the built-in cold-start Git path is versionless, so two
	// objects that differ only by API version resolve to the same file. To watch
	// several versions of a group/resource and keep them in separate files, give the
	// GitTarget a placement template that includes {version} (see GitTargetPlacementSpec).
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
	// +kubebuilder:validation:items:MinLength=1
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
	// +listType=map
	// +listMapKey=type
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
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Streams",type=string,JSONPath=`.status.streams.summary`
// +kubebuilder:printcolumn:name="GitTargetReady",type=string,JSONPath=`.status.conditions[?(@.type=="GitTargetReady")].status`,priority=1
// +kubebuilder:printcolumn:name="StreamsRunning",type=string,JSONPath=`.status.conditions[?(@.type=="StreamsRunning")].status`,priority=1
// +kubebuilder:printcolumn:name="SourceAuthorized",type=string,JSONPath=`.status.conditions[?(@.type=="SourceNamespaceAuthorized")].status`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WatchRule watches namespaced resources in ONE namespace of the source cluster its GitTarget
// mirrors from. It provides fine-grained control over which resources trigger Git commits,
// with filtering by operation type, API group, version, and labels.
//
// Security model:
//   - WatchRule is namespace-scoped and watches its OWN namespace unless spec.sourceNamespace
//     names another one AND that override passes the three-part gate described on that field —
//     provider admission, an explicit provider-side delegation flag, and the GitTarget's
//     allowedSourceNamespaces. The gate is deny-by-default and re-evaluated on every reconcile,
//     so a policy tightened later revokes a running rule.
//   - Use ClusterWatchRule for watching cluster-scoped resources (Nodes, ClusterRoles, etc.)
//   - RBAC controls who can create/modify WatchRules per namespace. Note that where the source is
//     the operator's OWN cluster, an authorized override deliberately bypasses live namespace
//     RBAC — which is why it takes an explicit platform-admin delegation to enable.
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

// EffectiveSourceNamespace is the source-cluster namespace this rule actually watches:
// spec.sourceNamespace when set, and the rule's OWN namespace otherwise.
//
// It is controller logic rather than an API-server default because an apiserver default cannot
// refer to metadata.namespace. Every consumer must call it instead of reading either field
// directly — the compiled rule, the watch selection, and the fingerprint that decides whether the
// watched-type table is re-projected all key on this value, and a site that reads
// metadata.namespace instead produces a STALE WATCH rather than a visible failure.
func (w *WatchRule) EffectiveSourceNamespace() string {
	if w.Spec.SourceNamespace != "" {
		return w.Spec.SourceNamespace
	}
	return w.Namespace
}

// OverridesSourceNamespace reports whether this rule asks for a source namespace OTHER than its
// own — the case that needs the ClusterProvider's delegation flag. A spec.sourceNamespace that
// merely restates the rule's own namespace is not an override and is treated as the legacy case.
func (w *WatchRule) OverridesSourceNamespace() bool {
	return w.EffectiveSourceNamespace() != w.Namespace
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
