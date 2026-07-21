// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultClusterProviderName is the conventionally opinionated ClusterProvider name that an
// omitted GitTarget.spec.clusterProviderRef points at. That defaulting is its ONLY special
// behavior: it is an ordinary user-created object that may omit kubeConfig (the operator's own
// in-cluster config) or set it to mirror a remote cluster, its audit route defaults to its name like
// every other provider's, and it is never created by the operator.
const DefaultClusterProviderName = "default"

// ClusterProviderReference references the cluster-scoped ClusterProvider a GitTarget sources
// FROM. It is the read-side peer of GitProviderReference (which names the WRITE destination):
// a GitTarget names one ClusterProvider by name and its author-attribution facts, kube client,
// and namespace authorization all follow from that single reference. Group and Kind are typed
// (with defaults) for consistency with the project's other typed references.
type ClusterProviderReference struct {
	// API Group of the referent.
	// +kubebuilder:default=configbutler.ai
	// +kubebuilder:validation:Enum=configbutler.ai
	Group string `json:"group,omitempty"`

	// Kind of the referent.
	// Optional because this reference currently only supports a single kind (ClusterProvider).
	// +optional
	// +kubebuilder:validation:Enum=ClusterProvider
	// +kubebuilder:default=ClusterProvider
	Kind string `json:"kind,omitempty"`

	// Name of the referent.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ClusterProviderSpec defines the desired state of ClusterProvider.
//
// kubeConfig is IMMUTABLE and OPTIONAL: which physical cluster a provider name means must not
// silently change under the GitTargets bound to it, and an OMITTED kubeConfig means the operator's
// own (in-cluster) cluster. That choice is free for EVERY name, "default" included — a provider
// named "default" may just as well carry a kubeConfig and mirror a remote cluster. The name is an
// identity, not a claim about which cluster it points at.
//
// +kubebuilder:validation:XValidation:rule="has(self.kubeConfig) == has(oldSelf.kubeConfig) && (!has(self.kubeConfig) || self.kubeConfig == oldSelf.kubeConfig)",message="spec.kubeConfig is immutable; delete and recreate the ClusterProvider to point a name at a different cluster"
//
// configMapRef (Flux workload-identity auth) is present in meta.KubeConfigReference's schema but
// deferred here; reject it so the v1alpha3 contract is "secretRef only".
// +kubebuilder:validation:XValidation:rule="!has(self.kubeConfig) || !has(self.kubeConfig.configMapRef)",message="spec.kubeConfig.configMapRef (workload-identity auth) is not yet supported; use secretRef"
//
// secretRef.name comes from the external meta.KubeConfigReference schema, which marks it required
// but permits the empty string; an empty name can never resolve a Secret, so reject it here.
// +kubebuilder:validation:XValidation:rule="!has(self.kubeConfig) || !has(self.kubeConfig.secretRef) || size(self.kubeConfig.secretRef.name) > 0",message="spec.kubeConfig.secretRef.name must not be empty"
type ClusterProviderSpec struct {
	// KubeConfig names the SOURCE CLUSTER this provider represents and the credentials to reach it
	// (Flux's meta.KubeConfigReference, embedded verbatim). OMITTED means the operator's own
	// in-cluster cluster, for any provider name. IMMUTABLE.
	//
	// The referenced Secret is resolved from the operator's namespace, so a cluster's credential
	// never has to live on that cluster. Only secretRef is honored; configMapRef is rejected. When
	// secretRef.key is empty the resolver reads "value" then "value.yaml" (Flux's order). Unsafe
	// kubeconfigs (exec auth, insecure-skip-tls-verify) are rejected with Validated=False unless the
	// operator opts in via flags.
	// +optional
	KubeConfig *meta.KubeConfigReference `json:"kubeConfig,omitempty"`

	// AllowedNamespaces is the deny-by-default policy for which CONTROL-CLUSTER namespaces may
	// reference this provider from a GitTarget. Empty (or omitted) means no namespace may
	// reference it. Its selector matches labels on Namespaces in the control cluster — the
	// cluster the operator's own CRs live in — never on the source cluster this provider names.
	// +optional
	AllowedNamespaces *NamespaceMatcher `json:"allowedNamespaces,omitempty"`

	// Design rationale, kept out of the generated CRD description by the blank line below.
	//
	// Remote and in-cluster providers use the same mechanism but deserve very different sign-off.
	// For a REMOTE provider the config-plane namespace and the source namespace are on different
	// clusters, so their sharing a name never was a boundary and naming one widens nothing. For an
	// IN-CLUSTER provider (kubeConfig omitted) the same-name coupling WAS the boundary: setting this
	// deliberately bypasses live namespace RBAC, letting the owner of an admitted GitTarget in one
	// namespace mirror another namespace's objects — read through the operator's own cluster-wide
	// credential — into a Git destination they control. That is legitimate for a cluster-admin to
	// grant on purpose, and must never happen by default or as a side effect of another field,
	// which is why this exists and defaults to false. LOCALITY is not the switch: in-cluster-ness
	// follows from spec.kubeConfig, and neither that nor the provider's name decides this.
	//
	// A wildcard needs the flag for the same reason a named namespace does: it requests the
	// target's policy SET, so a later policy edit could otherwise widen the watch with no
	// platform-admin opt-in.

	// AllowSourceNamespaceOverride delegates SOURCE-namespace selection to the GitTargets this
	// provider admits. While false (the default) a WatchRule mirroring through this provider may
	// watch only its OWN namespace, whatever any GitTarget policy says.
	//
	// It grants no access by itself: an admitted GitTarget must still admit the namespace in its
	// spec.allowedSourceNamespaces, and the source credential's own RBAC remains the hard maximum.
	// What it delegates is the AUTHORITY to choose, so set it only when the owners of admitted
	// GitTargets are trusted to pick a subset of what that credential may read. Every
	// cross-namespace request needs it, including a rules[].sourceNamespace of "*". It does not
	// apply to ClusterWatchRule, which selects no namespaces at all.
	// +optional
	// +kubebuilder:default=false
	AllowSourceNamespaceOverride bool `json:"allowSourceNamespaceOverride,omitempty"`

	// QPS overrides the operator's outgoing kube-client query-per-second throttle for this
	// cluster's watches and discovery. Omitted, the operator-wide --source-cluster-qps applies.
	// Ignored when kubeConfig is omitted (the in-cluster client is not per-provider).
	// +optional
	// +kubebuilder:validation:Minimum=1
	QPS *int32 `json:"qps,omitempty"`

	// Burst overrides the operator's outgoing kube-client burst for this cluster. Omitted, the
	// operator-wide --source-cluster-burst applies. Ignored when kubeConfig is omitted.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst *int32 `json:"burst,omitempty"`

	// Attribution groups this cluster's author-attribution settings. The block is spelled
	// "attribution" rather than "authorAttribution" even though the operator flags are
	// --author-attribution-*: the prefix groups a flat flag namespace, and a block on a
	// source-cluster object already supplies that scope.
	// +optional
	Attribution *ClusterProviderAttribution `json:"attribution,omitempty"`
}

// ClusterProviderAttribution holds the per-cluster author-attribution settings. It exists as a
// block so later per-cluster knobs (grace, mode) have a home beside auditRoute.
type ClusterProviderAttribution struct {
	// AuditRoute is the route this cluster's audit events arrive on. The sender is the API server's
	// webhook backend (https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend),
	// and this is the <name> segment its configured URL ends in: /audit-webhook/<name>. When several
	// logical clusters share one backend it is instead the value of the audit-event annotation named
	// by --author-attribution-audit-route-annotation-key.
	//
	// It partitions the attribution facts, so two ClusterProviders carrying the same route read one
	// cluster's facts, and two carrying different routes can never cross-credit an author.
	//
	// Empty means metadata.name. Set it when several ClusterProviders name one cluster, since an API
	// server has one webhook backend and so posts under one route: every other provider for that
	// cluster must be pointed at the same route or it resolves no authors at all. Set it also when
	// the events are labelled for something other than this object, such as a kcp logical cluster.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	AuditRoute string `json:"auditRoute,omitempty"`
}

// ClusterProviderStatus defines the observed state of ClusterProvider.
type ClusterProviderStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report the provider's readiness: Validated (kubeconfig inputs are safe and
	// resolvable, asserted without a network dial) plus the aggregated Ready and the kstatus
	// Reconciling/Stalled pair. Runtime reachability/discovery health and a last-audit-event
	// timestamp are deferred until authenticated remote ingest wires them from the watch engine.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Validated",type=string,JSONPath=`.status.conditions[?(@.type=="Validated")].status`,priority=1
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterProvider is the cluster-scoped, read-side peer of GitProvider: it names a SOURCE cluster a
// GitTarget mirrors FROM, and owns that cluster's connectivity credential (spec.kubeConfig),
// namespace-access authorization (spec.allowedNamespaces), and per-cluster status. Its NAME is the
// cluster's identity for the watch data plane, and the DEFAULT for its audit route: attribution
// facts are partitioned by spec.attribution.auditRoute, which falls back to this name. Several
// providers may name one cluster by declaring one route, which is what an API server with a single
// audit webhook backend requires. No name is special: "default" is merely what an omitted
// GitTarget.spec.clusterProviderRef points at, and it may just as well name a remote cluster, since
// in-cluster-ness follows from spec.kubeConfig (omitted = in-cluster) rather than from the name.
//
// It is cluster-scoped and requires platform-admin permissions to create. A GitTarget may reference
// it only from a namespace spec.allowedNamespaces admits — deny-by-default, enforced at admission
// and again before any watch starts.
type ClusterProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ClusterProvider.
	// +required
	Spec ClusterProviderSpec `json:"spec"`

	// status defines the observed state of ClusterProvider.
	// +optional
	Status ClusterProviderStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterProviderList contains a list of ClusterProvider.
type ClusterProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterProvider `json:"items"`
}

// IsInCluster reports whether this provider represents the operator's own (in-cluster) cluster —
// i.e. it has no kubeConfig. The provider name is irrelevant: any name, including "default", may
// either omit kubeConfig for the in-cluster client or set it for a remote cluster.
func (p *ClusterProvider) IsInCluster() bool {
	return p.Spec.KubeConfig == nil
}

// AuditRoute is the identity this provider's attribution facts are keyed under: the route its
// cluster's audit events arrive on, defaulting to the provider's own name. It is resolved through
// this method rather than read off the field so no caller ever handles the empty case, the same
// shape as (api/v1alpha3).GitTarget.SourceCluster().
//
// The default is what makes this change invisible to an existing install: one ClusterProvider per
// cluster already partitions its facts by name, so an unset field resolves exactly what it always
// resolved. Deliberately NOT conditional on locality: defaulting an in-cluster provider to the
// literal "default" would make that name reserved for the local cluster again, a rule this project
// enforced with CEL and then reversed before shipping (docs/finished/multi-cluster-author-attribution.md).
func (p *ClusterProvider) AuditRoute() string {
	if p.Spec.Attribution == nil || p.Spec.Attribution.AuditRoute == "" {
		return p.Name
	}
	return p.Spec.Attribution.AuditRoute
}

// AllowsNamespace reports whether a namespace (by name and labels) may reference this provider
// from a GitTarget, per spec.allowedNamespaces. It is DENY-BY-DEFAULT: a provider with no
// allowedNamespaces policy (neither names nor selector) admits no namespace. Names and selector
// are ORed. Enforced on every reconcile and NOWHERE else: checkSourceAuthorization in
// internal/controller/gittarget_source_cluster.go is the only non-test caller, and it returns
// before DeclareForGitTarget, so an unauthorized target starts no watch and writes no Git.
// Reconcile-time is deliberate rather than incidental — it re-evaluates continuously, so it also
// covers a policy tightened after the GitTarget was created, which an admission webhook could not
// see. There is no admission webhook for this (docs/spec/where-validation-lives.md). A malformed
// selector is a configuration error surfaced to the caller (not a silent allow).
// A malformed selector is a configuration error surfaced to the caller (not a silent allow).
//
// It is one of two thin wrappers over NamespaceMatcher.Matches — the other being
// GitTarget.AllowsSourceNamespace — so the control-cluster and source-cluster policies can never
// drift in their deny-by-default, names-OR-selector semantics. The labels passed here are always
// CONTROL-cluster Namespace labels.
func (p *ClusterProvider) AllowsNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	return p.Spec.AllowedNamespaces.Matches(nsName, nsLabels)
}

// AllowsSourceNamespaceOverride reports whether this provider delegates source-namespace selection
// to the GitTargets it admits. See the field's documentation: false (the default) means a WatchRule
// mirroring through this provider may watch only its own namespace.
func (p *ClusterProvider) AllowsSourceNamespaceOverride() bool {
	return p.Spec.AllowSourceNamespaceOverride
}

func init() {
	SchemeBuilder.Register(&ClusterProvider{}, &ClusterProviderList{})
}
