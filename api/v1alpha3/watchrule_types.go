// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"fmt"

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
// WatchRule selects NAMESPACED resources on its GitTarget's source cluster. Each rules[] item
// carries its own source namespace: omitted for this WatchRule's own namespace, an explicit name,
// or "*" for every namespace the GitTarget admits.
// +kubebuilder:validation:XValidation:rule="!has(self.sourceNamespace)",message="spec.sourceNamespace moved to spec.rules[].sourceNamespace"
type WatchRuleSpec struct {
	// TargetRef references the GitTarget to use.
	// Must be in the same namespace.
	// +required
	TargetRef LocalTargetReference `json:"targetRef"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// The field is retained in the schema purely so that re-applying a manifest that still sets it
	// FAILS. Deleting it outright would be worse and silent: CRD pruning happens on write, so an
	// unrecognised top-level field is dropped without an error and the rule would quietly watch its
	// own namespace instead of the one it asked for. The XValidation rule on spec rejects any value
	// at admission, and the compile path refuses a stored one with the same message.

	// SourceNamespace is REMOVED. It moved to spec.rules[].sourceNamespace, so that the source
	// namespace sits beside the resource selector it applies to. Setting it is rejected; the field
	// remains in the schema for one release only so that doing so fails loudly.
	//
	// Deprecated: use spec.rules[].sourceNamespace. Removed one release from now, or at v1beta1.
	// +optional
	// +kubebuilder:validation:MinLength=1
	SourceNamespace string `json:"sourceNamespace,omitempty"`

	// Rules define which resources to watch, and in which source namespaces.
	// Multiple rules create a logical OR - a resource matching ANY rule is watched.
	// Each rule can specify operations, API groups, versions, resource types, and a source namespace.
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

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// Every item's outcome is aggregated into the ONE SourceNamespaceAuthorized condition, so
	// automation has a single condition to inspect. A denied explicit name refuses the whole
	// WatchRule rather than silently trimming that item: mirroring two of the three namespaces a
	// rule asked for is worse than a loud failure. A "*" that currently admits nothing is not a
	// refusal — it is valid, starts no stream, and says so as NoAdmittedSourceNamespaces, because a
	// rule that mirrors nothing while reporting Ready with no explanation is a silent no-op.
	//
	// Cost: a "*" item opens one watch stream per (matched type × admitted namespace) and one
	// resync scope each, rather than one cluster-wide stream. That is deliberate — it keeps every
	// replay scoped to a single namespace — but it is a real fan-out on a broad policy.

	// SourceNamespace is the namespace this item watches IN THE SOURCE CLUSTER its GitTarget
	// mirrors from: omitted for this WatchRule's own namespace, an exact name for one other, or
	// "*" for every namespace the GitTarget's spec.allowedSourceNamespaces currently admits.
	//
	// "*" never means "every namespace that exists" — it expands to exactly what that policy
	// admits, so a target declaring no policy denies it. Naming any namespace other than this
	// rule's own, "*" included, additionally requires the GitTarget's ClusterProvider to admit the
	// target's namespace AND to set spec.allowSourceNamespaceOverride. Once the GitTarget declares
	// a policy it is exhaustive, so even an omitted sourceNamespace is checked against it.
	//
	// This changes only which namespace is WATCHED, never where objects are written: Git placement
	// follows each mirrored object's own namespace.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(\*|[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`
	SourceNamespace string `json:"sourceNamespace,omitempty"`
}

// SourceNamespaceWildcard is the literal rules[].sourceNamespace token meaning "every source
// namespace this rule's GitTarget admits" — resolved live through
// GitTarget.spec.allowedSourceNamespaces, never "every namespace that exists".
const SourceNamespaceWildcard = "*"

// EffectiveSourceNamespace is the source-cluster namespace this ITEM names, given the namespace of
// the WatchRule that carries it: spec.rules[].sourceNamespace when set, and the rule's OWN
// namespace otherwise. For a wildcard item it returns "*" — the caller must expand that through
// the GitTarget's policy rather than treat it as a namespace name.
//
// It is controller logic rather than an API-server default because an apiserver default cannot
// refer to metadata.namespace.
func (r *ResourceRule) EffectiveSourceNamespace(ruleNamespace string) string {
	if r.SourceNamespace != "" {
		return r.SourceNamespace
	}
	return ruleNamespace
}

// IsSourceNamespaceWildcard reports whether this item asks to follow its GitTarget's admitted set.
func (r *ResourceRule) IsSourceNamespaceWildcard() bool {
	return r.SourceNamespace == SourceNamespaceWildcard
}

// OverridesSourceNamespace reports whether this item asks for a source namespace OTHER than the
// WatchRule's own — the case that needs the ClusterProvider's delegation flag. A sourceNamespace
// that merely restates the rule's own namespace is not an override and stays the legacy case; "*"
// always is one, even against a policy that happens to list only that namespace, because a later
// policy edit would otherwise widen the watch with no platform-admin opt-in.
func (r *ResourceRule) OverridesSourceNamespace(ruleNamespace string) bool {
	return r.IsSourceNamespaceWildcard() || r.EffectiveSourceNamespace(ruleNamespace) != ruleNamespace
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

// Design rationale, kept out of the generated CRD description by the blank line below.
//
// The source-namespace gate is deny-by-default and re-evaluated on EVERY reconcile, which is what
// makes a policy tightened after a rule was accepted revoke that rule rather than grandfather it.
// Where the source is the operator's OWN cluster, an authorized override deliberately bypasses live
// namespace RBAC — the operator reads through its own cluster-wide credential — which is why it
// takes an explicit platform-admin delegation on the ClusterProvider to enable at all.

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

// WatchRule selects NAMESPACED resources on the source cluster its GitTarget mirrors from, with
// filtering by operation, API group, version, and source namespace. Scope is carried by the rule
// KIND: a WatchRule never selects cluster-scoped types — use a ClusterWatchRule for those.
//
// Each spec.rules[] item watches its own source namespace: this WatchRule's OWN namespace when
// omitted, an explicit name, or "*" for every namespace the GitTarget admits. Anything other than
// its own namespace passes the gate described on rules[].sourceNamespace. RBAC controls who may
// create or modify WatchRules per namespace.
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

// DeclaresRemovedSourceNamespace reports whether a STORED WatchRule still carries the removed
// top-level spec.sourceNamespace. Admission rejects it, but an object written before this release
// keeps its value in etcd, so the compile path must refuse it too — otherwise the rule would
// silently watch its own namespace instead of the one it asked for.
func (w *WatchRule) DeclaresRemovedSourceNamespace() bool {
	return w.Spec.SourceNamespace != ""
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
