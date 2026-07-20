// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultClusterProviderName is the conventionally opinionated ClusterProvider name that an
// omitted GitTarget.spec.clusterProviderRef points at. That defaulting is its ONLY special
// behavior: it is an ordinary user-created object that may omit kubeConfig (the operator's own
// in-cluster config) or set it to mirror a remote cluster, it is existence-gated on its
// /audit-webhook/default route like every other name, and it is never created by the operator.
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
	// KubeConfig names the SOURCE CLUSTER this provider represents and the credentials to reach
	// it (Flux's meta.KubeConfigReference, embedded verbatim). OMITTED means the operator's own
	// in-cluster cluster, for any provider name. IMMUTABLE. The referenced Secret is
	// resolved from the operator's namespace — the credential for a cluster never has to live on
	// that cluster. When secretRef.key is empty the resolver reads "value" then "value.yaml"
	// (Flux's order). Only secretRef is honored (configMapRef is rejected); unsafe kubeconfigs
	// (exec auth, insecure-skip-tls-verify) are rejected with a Validated=False reason unless the
	// operator opts in via flags.
	// +optional
	KubeConfig *meta.KubeConfigReference `json:"kubeConfig,omitempty"`

	// AllowedNamespaces is the deny-by-default policy for which CONTROL-CLUSTER namespaces may
	// reference this provider from a GitTarget. Empty (or omitted) means no namespace may
	// reference it. Its selector matches labels on Namespaces in the control cluster — the
	// cluster the operator's own CRs live in — never on the source cluster this provider names.
	// +optional
	AllowedNamespaces *NamespaceMatcher `json:"allowedNamespaces,omitempty"`

	// AllowWatchRuleSourceNamespaceOverride delegates SOURCE-namespace selection to the GitTargets
	// this provider admits. While false (the default) a WatchRule mirroring through this provider
	// may watch only its OWN namespace, whatever any GitTarget policy says.
	//
	// The flag does not grant access by itself — an admitted GitTarget must still list the
	// namespace in spec.allowedSourceNamespaces. What it delegates is the AUTHORITY to choose: a
	// target owner may then configure a broad allow-list, including one matching every source
	// namespace, so the source credential's own RBAC remains the hard maximum. Set it only when
	// the owners of admitted GitTargets are trusted to pick a subset of what that credential
	// may read.
	//
	// It gates GRANTING only. spec.allowedSourceNamespaces plays two roles — widening a WatchRule
	// beyond its own namespace, and narrowing a ClusterWatchRule below cluster-wide — and only the
	// widening one is an authority grant. Gating a RESTRICTION behind a delegation flag would mean
	// an admin has to grant extra authority in order to reduce scope.
	//
	// Remote and in-cluster providers use the same mechanism but deserve very different sign-off.
	// For a REMOTE provider the config-plane namespace and the source namespace are on different
	// clusters, so their sharing a name never was a boundary and naming one widens nothing. For an
	// IN-CLUSTER provider (kubeConfig omitted) the same-name coupling WAS the boundary: setting
	// this deliberately bypasses live namespace RBAC, letting the owner of an admitted GitTarget
	// in one namespace mirror another namespace's objects — read through the operator's own
	// cluster-wide credential — into a Git destination they control. That is legitimate for a
	// cluster-admin to grant on purpose, and must never happen by default or as a side effect of
	// another field, which is why this exists and defaults to false.
	//
	// Note that LOCALITY is not the switch: whether a provider is in-cluster follows from
	// spec.kubeConfig, and neither that nor the provider's name decides this. Only this flag does.
	// +optional
	// +kubebuilder:default=false
	AllowWatchRuleSourceNamespaceOverride bool `json:"allowWatchRuleSourceNamespaceOverride,omitempty"`

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

// ClusterProvider is the cluster-scoped, read-side peer of GitProvider: it names a SOURCE cluster
// a GitTarget mirrors FROM, and is the home for that cluster's connectivity credential
// (spec.kubeConfig), namespace-access authorization (spec.allowedNamespaces), and per-cluster
// status. Its NAME is the cluster's identity everywhere: the /audit-webhook/<name> ingress route
// and the attribution fact-index key. No name is special: "default" is merely the name
// GitTarget.spec.clusterProviderRef defaults to when omitted. Whether a provider is the operator's
// own cluster or a remote follows from spec.kubeConfig (omitted = in-cluster), not from its name,
// so "default" may just as well name a remote cluster.
//
// Security model:
//   - ClusterProvider is cluster-scoped and requires platform-admin permissions to create.
//   - A GitTarget may reference it only from a namespace its spec.allowedNamespaces admits
//     (deny-by-default), enforced at admission AND before any watch starts.
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

// AllowsWatchRuleSourceNamespaceOverride reports whether this provider delegates source-namespace
// selection to the GitTargets it admits. See the field's documentation: false (the default) means
// a WatchRule mirroring through this provider may watch only its own namespace.
func (p *ClusterProvider) AllowsWatchRuleSourceNamespaceOverride() bool {
	return p.Spec.AllowWatchRuleSourceNamespaceOverride
}

func init() {
	SchemeBuilder.Register(&ClusterProvider{}, &ClusterProviderList{})
}
