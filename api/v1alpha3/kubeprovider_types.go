// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"sort"
	"strings"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// KubeProviderSpec defines the desired state of KubeProvider.
//
// Exactly one credential arm is set, and there is no third: an omitted,
// malformed or refused credential must never fall through to the operator's
// own identity, so that identity is an arm of its own (inCluster) rather than
// what an absent secretRef means.
// +kubebuilder:validation:XValidation:rule="has(self.secretRef) != has(self.inCluster)",message="set exactly one of spec.secretRef and spec.inCluster"
//
// The in-cluster client is the operator's own and is shared by everything that
// uses it, so a per-provider throttle cannot apply to it. Refused rather than
// ignored.
// +kubebuilder:validation:XValidation:rule="!has(self.inCluster) || !has(self.client)",message="spec.client is refused beside spec.inCluster: the operator's own client is not throttled per provider"
//
// Which cluster a provider means must not silently change under the GitTargets
// mirroring through it, so the credential is immutable, including switching
// arms. A rotated kubeconfig is a change to the Secret, which stays allowed.
// +kubebuilder:validation:XValidation:rule="has(self.inCluster) == has(oldSelf.inCluster) && (!has(self.secretRef) || self.secretRef == oldSelf.secretRef)",message="spec.secretRef and spec.inCluster are immutable; delete and recreate the KubeProvider to point it at a different cluster"
type KubeProviderSpec struct {
	// SecretRef names the Secret, in this namespace, that holds a kubeconfig for
	// the cluster. With no key, the kubeconfig is read from "value", then
	// "value.yaml". A kubeconfig that uses exec auth, insecure-skip-tls-verify or
	// credentials named by file path is refused with Validated=False unless the
	// installation allows it. Set exactly one of secretRef and inCluster.
	// Immutable: delete and recreate the KubeProvider to change it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="spec.secretRef.name must not be empty"
	SecretRef *meta.SecretKeyReference `json:"secretRef,omitempty"`

	// InCluster makes this KubeProvider use the operator's own identity in the
	// cluster it runs in. It is honored only for a KubeProvider the installation
	// lists in --in-cluster-kube-providers; any other reports Stalled=True, reason
	// InClusterNotAuthorized, and is never used. Set exactly one of secretRef and
	// inCluster. Immutable: delete and recreate the KubeProvider to change it.
	// +optional
	InCluster *KubeProviderInCluster `json:"inCluster,omitempty"`

	// AllowedGitTargets says which GitTargets may mirror through this
	// KubeProvider, and what. Each grant allows the GitTargets of some
	// namespaces, bounds which source namespaces their WatchRules may read
	// (sourceNamespaces), and lists the cluster-scoped types their
	// ClusterWatchRules may mirror (clusterResources).
	//
	// - Omitted: the GitTargets in this KubeProvider's namespace, reading only
	//   the source namespace of the same name and no cluster-scoped type.
	// - Empty list ([]): no GitTarget.
	// - A list of grants: the GitTargets that at least one grant allows, with
	//   what every grant that allows them permits.
	//
	// A list replaces the default: the GitTargets in this namespace keep access
	// only if at least one grant allows them.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={{consumerNamespaces: {from: Same}, sourceNamespaces: {"{consumer.namespace}"}}}
	AllowedGitTargets []KubeProviderGitTargetGrant `json:"allowedGitTargets"`

	// AllowedApplierClaims says which FluxClaims and ArgoCDClaims may hold an
	// applier through this KubeProvider, and in which namespaces that applier
	// may be (applierNamespaces). A namespace in allowedGitTargets gets no
	// applier access from that.
	//
	// - Omitted: the claims in this KubeProvider's namespace, holding appliers
	//   in the namespace of the same name.
	// - Empty list ([]): no claim.
	// - A list of grants: the claims that at least one grant allows, holding
	//   appliers in the applierNamespaces of every grant that allows them.
	//
	// A list replaces the default: the claims in this namespace keep access
	// only if at least one grant allows them.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={{consumerNamespaces: {from: Same}, applierNamespaces: {"{consumer.namespace}"}}}
	AllowedApplierClaims []KubeProviderApplierClaimGrant `json:"allowedApplierClaims"`

	// Client sets the request rate limits for this cluster. Omitted, they are 20
	// QPS with a burst of 30. Refused beside inCluster.
	// +optional
	Client *KubeProviderClient `json:"client,omitempty"`

	// The block is spelled "attribution" rather than "authorAttribution" even though the operator
	// flags are --author-attribution-*: the prefix groups a flat flag namespace, and a block on a
	// source-cluster object already supplies that scope.

	// Attribution configures how commit authors are found from this cluster's
	// audit events.
	// +optional
	Attribution *KubeProviderAttribution `json:"attribution,omitempty"`

	// RemoteSaveRequests says whether GitOps Reverser serves the
	// RemoteSaveRequests (source.configbutler.ai) created on this cluster.
	// "Enabled" watches them, saves the commit windows of the GitTargets that
	// mirror each one's namespace through this KubeProvider and admit it in
	// spec.allowedRemoteSaveRequests, and writes each one's status back.
	// Defaults to "Disabled".
	//
	// "Enabled" writes to this cluster: the credential needs get, list, watch
	// and delete on remotesaverequests, and update on
	// remotesaverequests/status. The RemoteSaveRequest CRD must be installed
	// on the cluster.
	// +optional
	// +kubebuilder:validation:Enum=Disabled;Enabled
	// +kubebuilder:default=Disabled
	RemoteSaveRequests RemoteSaveRequestsMode `json:"remoteSaveRequests,omitempty"`
}

// RemoteSaveRequestsMode is whether a KubeProvider's RemoteSaveRequests are
// served.
type RemoteSaveRequestsMode string

const (
	// RemoteSaveRequestsDisabled leaves the cluster's RemoteSaveRequests alone.
	RemoteSaveRequestsDisabled RemoteSaveRequestsMode = "Disabled"
	// RemoteSaveRequestsEnabled serves the cluster's RemoteSaveRequests.
	RemoteSaveRequestsEnabled RemoteSaveRequestsMode = "Enabled"
)

// KubeProviderInCluster selects the operator's own identity. It has no fields:
// the arm being set is the whole statement.
type KubeProviderInCluster struct{}

// KubeProviderClient holds the per-cluster kube-client throttles.
type KubeProviderClient struct {
	// QPS is the sustained request rate to this cluster, in queries per second.
	// Defaults to 20.
	// +optional
	// +kubebuilder:validation:Minimum=1
	QPS *int32 `json:"qps,omitempty"`

	// Burst is how many requests to this cluster may exceed qps at once. Defaults
	// to 30.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst *int32 `json:"burst,omitempty"`
}

// KubeProviderAttribution holds the per-cluster author-attribution settings.
// It exists as a block so later per-cluster knobs (grace, mode) have a home
// beside auditRoute.
type KubeProviderAttribution struct {
	// AuditRoute is the route this cluster's audit events arrive on: the <route>
	// in the API server's audit webhook URL, /audit-webhook/<route>. When several
	// logical clusters share one webhook backend, it is instead the value of the
	// audit-event annotation that --author-attribution-audit-route-annotation-key
	// names. KubeProviders with the same route read the same cluster's
	// attribution facts.
	//
	// Empty means <namespace>.<name>. Set it when several KubeProviders in this
	// namespace connect to one cluster, since an API server posts under one
	// route.
	//
	// A route belongs to one namespace:
	//
	// - the namespace the route starts with (<namespace>.), when a KubeProvider
	//   there carries it;
	// - otherwise, the namespace of the oldest KubeProvider carrying it.
	//
	// KubeProviders in another namespace that carry the route report
	// Stalled=True, reason AuditRouteConflict, and are not used.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	AuditRoute string `json:"auditRoute,omitempty"`
}

// KubeProviderStatus defines the observed state of KubeProvider.
type KubeProviderStatus struct {
	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AuditFactsReceived is separate from Ready because a provider with no facts yet mirrors
	// correctly; only the commit AUTHOR is lost. Reporting it through Ready would tell every
	// kstatus reader the object is broken.

	// AuditRoute exists because the AuditFactsReceived latch needs a KEY. spec.attribution.auditRoute
	// is mutable, and without recording which route earned the latch, a provider repointed at a new
	// route would carry True forward and report facts it has never received there.

	// AuditRoute is the route in use: spec.attribution.auditRoute, or
	// <namespace>.<name> when that is empty. The AuditFactsReceived condition
	// describes this route, and starts over when it changes.
	// +optional
	AuditRoute string `json:"auditRoute,omitempty"`

	// Conditions are the latest observations of this KubeProvider's state. Ready,
	// Reconciling and Stalled follow kstatus; Stalled=True with reason
	// InClusterNotAuthorized or AuditRouteConflict means the KubeProvider is not
	// used at all. Validated says whether the credential is safe and readable,
	// checked without connecting.
	//
	// AuditFactsReceived, which is not part of Ready, says whether an attribution
	// fact has ever arrived on status.auditRoute. It is True (Received) from the
	// first one on and never goes back. Until then it is Unknown, and the reason
	// says where to look: TransportUnavailable (the fact store refuses writes),
	// NoAuditDelivery (no audit events arrive at all), or RouteUnused (audit
	// events arrive, but never on this route). It is absent when author
	// attribution is off.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="FactsReceived",type=string,JSONPath=`.status.conditions[?(@.type=="AuditFactsReceived")].status`
// +kubebuilder:printcolumn:name="Route",type=string,JSONPath=`.status.auditRoute`,priority=1
// +kubebuilder:printcolumn:name="Validated",type=string,JSONPath=`.status.conditions[?(@.type=="Validated")].status`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KubeProvider is a connection to a Kubernetes cluster and the identity used
// there: a kubeconfig Secret in its namespace, or the operator's own identity
// where the installation allows it. GitTargets mirror from it and applier
// claims reach their applier through it, as its spec.allowedGitTargets and
// spec.allowedApplierClaims allow.
type KubeProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of KubeProvider.
	// +required
	Spec KubeProviderSpec `json:"spec"`

	// status defines the observed state of KubeProvider.
	// +optional
	Status KubeProviderStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// KubeProviderList contains a list of KubeProvider.
type KubeProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []KubeProvider `json:"items"`
}

// Key is this provider's namespace and name, the identity the watch data plane keys its source
// cluster on.
func (p *KubeProvider) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
}

// IsInCluster reports whether this provider asks for the operator's own identity (spec.inCluster).
// Whether it may have it is the installation's decision, not this object's.
func (p *KubeProvider) IsInCluster() bool {
	return p.Spec.InCluster != nil
}

// AuditRoute is the route this provider's attribution facts are keyed under: spec.attribution.auditRoute,
// or DefaultAuditRoute when that is empty. It is resolved through this method rather than read off
// the field so no caller ever handles the empty case.
func (p *KubeProvider) AuditRoute() string {
	if p.Spec.Attribution == nil || p.Spec.Attribution.AuditRoute == "" {
		return DefaultAuditRoute(p.Key())
	}
	return p.Spec.Attribution.AuditRoute
}

// DefaultAuditRoute is the audit route of a provider that sets none: <namespace>.<name>. A namespace
// name has no dots, so the first segment always names the provider's own namespace, and no provider
// in another namespace can resolve to it by default.
func DefaultAuditRoute(provider types.NamespacedName) string {
	return provider.Namespace + "." + provider.Name
}

// AuditRouteHolder returns the namespace that holds route among providers, and false when none of
// them carries it. A route is held by one namespace, so a provider in another namespace cannot read
// facts that belong to a tenant it was never admitted to:
//
//  1. The namespace the route names, its segment before the first dot, holds it when one of its
//     providers carries it. This is what keeps a provider's default route (<namespace>.<name>) its
//     own, even against an older provider elsewhere that spelled it out first.
//  2. Otherwise the namespace of the oldest carrier (creationTimestamp, then UID) holds it.
//
// A provider being deleted carries nothing, so its route is free the moment it goes.
func AuditRouteHolder(route string, providers []KubeProvider) (string, bool) {
	carriers := make([]*KubeProvider, 0, len(providers))
	for i := range providers {
		p := &providers[i]
		if p.DeletionTimestamp != nil || p.AuditRoute() != route {
			continue
		}
		carriers = append(carriers, p)
	}
	if len(carriers) == 0 {
		return "", false
	}
	named, _, _ := strings.Cut(route, ".")
	for _, p := range carriers {
		if p.Namespace == named {
			return named, true
		}
	}
	sort.Slice(carriers, func(i, j int) bool {
		a, b := carriers[i], carriers[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		if a.UID != b.UID {
			return a.UID < b.UID
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	return carriers[0].Namespace, true
}

func init() {
	SchemeBuilder.Register(&KubeProvider{}, &KubeProviderList{})
}
