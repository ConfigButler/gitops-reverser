// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// AllowedNamespaces is the deny-by-default namespace-access policy a ClusterProvider carries.
// A cluster-scoped provider holds a credential that can read a lot of a remote cluster; any
// GitTarget that references it makes the operator mirror that cluster's state into the target's
// destination. So which namespaces may reference the provider is authorization, not routing: an
// empty policy (no names, no selector) means NO namespace may reference the provider. Names and
// selector are ORed — a namespace is allowed if it is listed OR matches the selector.
type AllowedNamespaces struct {
	// Names is an explicit allow-list of namespace names that may reference this provider.
	// +optional
	// +listType=set
	Names []string `json:"names,omitempty"`

	// Selector is a label selector matched against Namespace labels; a namespace whose labels
	// match may reference this provider. ORed with Names.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
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

	// AllowedNamespaces is the deny-by-default policy for which namespaces may reference this
	// provider from a GitTarget. Empty (or omitted) means no namespace may reference it.
	// +optional
	AllowedNamespaces *AllowedNamespaces `json:"allowedNamespaces,omitempty"`

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
func (p *ClusterProvider) AllowsNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	policy := p.Spec.AllowedNamespaces
	if policy == nil {
		return false, nil
	}
	for _, n := range policy.Names {
		if n == nsName {
			return true, nil
		}
	}
	if policy.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(policy.Selector)
		if err != nil {
			return false, err
		}
		if sel.Matches(labels.Set(nsLabels)) {
			return true, nil
		}
	}
	return false, nil
}

func init() {
	SchemeBuilder.Register(&ClusterProvider{}, &ClusterProviderList{})
}
