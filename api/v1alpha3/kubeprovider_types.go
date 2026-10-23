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
// Exactly one credential arm is set, and there is no third: an omitted, malformed or refused
// credential must never fall through to the operator's own identity, so that identity is an arm
// of its own (inCluster) rather than what an absent secretRef means.
// +kubebuilder:validation:XValidation:rule="has(self.secretRef) != has(self.inCluster)",message="set exactly one of spec.secretRef and spec.inCluster"
//
// The in-cluster client is the operator's own and is shared by everything that uses it, so a
// per-provider throttle cannot apply to it. Refused rather than ignored.
// +kubebuilder:validation:XValidation:rule="!has(self.inCluster) || !has(self.client)",message="spec.client is refused beside spec.inCluster: the operator's own client is not throttled per provider"
//
// Which cluster a provider means must not silently change under the GitTargets mirroring through
// it, so the credential is immutable, including switching arms. A rotated kubeconfig is a change
// to the Secret, which stays allowed.
// +kubebuilder:validation:XValidation:rule="has(self.inCluster) == has(oldSelf.inCluster) && (!has(self.secretRef) || self.secretRef == oldSelf.secretRef)",message="spec.secretRef and spec.inCluster are immutable; delete and recreate the KubeProvider to point it at a different cluster"
type KubeProviderSpec struct {
	// SecretRef names a Secret in this KubeProvider's namespace holding a kubeconfig for the source
	// cluster. When key is empty the kubeconfig is read from "value", then "value.yaml" (Flux's
	// order). Unsafe kubeconfigs (exec auth, insecure-skip-tls-verify, credentials named by file
	// path) are refused with Validated=False unless the operator opts in through its flags.
	// Exactly one of secretRef and inCluster is set. Immutable.
	// +optional
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="spec.secretRef.name must not be empty"
	SecretRef *meta.SecretKeyReference `json:"secretRef,omitempty"`

	// InCluster makes this provider use the operator's own identity in the cluster it runs in. It is
	// honored only for a KubeProvider the installation lists in --in-cluster-kube-providers; any
	// other reports Stalled=True with reason InClusterNotAuthorized and is never used. Exactly one
	// of secretRef and inCluster is set. Immutable.
	// +optional
	InCluster *KubeProviderInCluster `json:"inCluster,omitempty"`

	// The policy that decides who may WIELD the credential: source-cluster RBAC bounds what it may
	// read, and cannot say which control-cluster tenant may use it. The selector reads control-cluster
	// labels locally, so it has no degradation path.

	// AccessFrom admits other control-cluster namespaces to reference this provider. Its own
	// namespace is always admitted; omitted, no other namespace is. Its selector matches labels on
	// Namespaces in the control cluster, never on the source cluster this provider names.
	// +optional
	AccessFrom *NamespaceMatcher `json:"accessFrom,omitempty"`

	// AllowCrossNamespace grants what a namespace this provider admits may reach outside itself,
	// through this provider's credential. Omitted, it may reach nothing outside itself.
	// +optional
	AllowCrossNamespace *KubeProviderCrossNamespace `json:"allowCrossNamespace,omitempty"`

	// Client overrides the operator's outgoing kube-client throttles for this cluster. Omitted,
	// the built-in defaults apply (20 QPS, burst 30). Refused beside inCluster.
	// +optional
	Client *KubeProviderClient `json:"client,omitempty"`

	// Attribution groups this cluster's author-attribution settings. The block is spelled
	// "attribution" rather than "authorAttribution" even though the operator flags are
	// --author-attribution-*: the prefix groups a flat flag namespace, and a block on a
	// source-cluster object already supplies that scope.
	// +optional
	Attribution *KubeProviderAttribution `json:"attribution,omitempty"`
}

// KubeProviderInCluster selects the operator's own identity. It has no fields: the arm being set
// is the whole statement.
type KubeProviderInCluster struct{}

// Each field is a power of its own, granted on purpose: sources lets a namespace READ other
// namespaces' objects into its own Git, appliers lets it suspend and apply other namespaces'
// deployments. On an inCluster provider same-name coupling WAS the boundary, so either one
// bypasses live namespace RBAC: the owner of an admitted object acts on another namespace through
// the operator's own credential. It is a delegation, not owner consent: it does not prove that the
// namespace owns the objects it reaches, or the repository it writes.

// KubeProviderCrossNamespace is what a namespace a KubeProvider admits may reach outside itself.
// The trust is in every admitted namespace, for everything in reach of the provider's identity; it
// does not prove that the namespace owns what it reaches.
type KubeProviderCrossNamespace struct {
	// Sources lets a WatchRule mirroring through this provider watch namespaces other than its own,
	// including a rules[].sourceNamespace of "*", which reaches every namespace the credential can
	// read. False (the default): a WatchRule watches only its own namespace. It grants no access by
	// itself: the source credential's RBAC remains the hard maximum. It does not apply to
	// ClusterWatchRule, which selects no namespaces at all.
	// +optional
	Sources bool `json:"sources,omitempty"`

	// Appliers lets a FluxClaim or ArgoCDClaim reaching its applier through this provider name a
	// Kustomization or Application in another namespace than its own. False (the default): only
	// the applier claim's own namespace, which in a remote cluster is the namespace of the same name; an
	// applier claim naming another is Configured=False (ApplierNamespaceNotAllowed).
	// +optional
	Appliers bool `json:"appliers,omitempty"`
}

// KubeProviderClient holds the per-cluster kube-client throttles.
type KubeProviderClient struct {
	// QPS overrides the operator's outgoing kube-client query-per-second throttle for this
	// cluster's watches and discovery. Omitted, the built-in default of 20 applies.
	// +optional
	// +kubebuilder:validation:Minimum=1
	QPS *int32 `json:"qps,omitempty"`

	// Burst overrides the operator's outgoing kube-client burst for this cluster. Omitted, the
	// built-in default of 30 applies.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst *int32 `json:"burst,omitempty"`
}

// KubeProviderAttribution holds the per-cluster author-attribution settings. It exists as a block
// so later per-cluster knobs (grace, mode) have a home beside auditRoute.
type KubeProviderAttribution struct {
	// AuditRoute is the route this cluster's audit events arrive on. The sender is the API server's
	// webhook backend (https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend),
	// and this is the <route> segment its configured URL ends in: /audit-webhook/<route>. When
	// several logical clusters share one backend it is instead the value of the audit-event
	// annotation named by --author-attribution-audit-route-annotation-key.
	//
	// It partitions the attribution facts, so two KubeProviders carrying the same route read one
	// cluster's facts, and two carrying different routes can never cross-credit an author.
	//
	// Empty means <namespace>.<name>, which no provider in another namespace can resolve to. Set it
	// when several KubeProviders in this namespace name one cluster, since an API server has one
	// webhook backend and so posts under one route. A route is held by one namespace: providers in
	// another namespace that carry it report Stalled=True with reason AuditRouteConflict and are not
	// used. The namespace a route starts with (<namespace>.) holds it when a provider there carries
	// it; otherwise the namespace of the oldest provider carrying it does.
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

	// AuditRoute is the route this provider's attribution facts are partitioned under, as resolved:
	// spec.attribution.auditRoute, or <namespace>.<name> when that is empty. It is the route the
	// AuditFactsReceived condition describes, so when it changes that condition starts over —
	// proof that one route delivered says nothing about another.
	// +optional
	AuditRoute string `json:"auditRoute,omitempty"`

	// Conditions report the provider's readiness: Validated (the credential is safe and
	// resolvable, asserted without a network dial) plus the aggregated Ready and the kstatus
	// Reconciling/Stalled pair. Stalled=True with reason InClusterNotAuthorized or
	// AuditRouteConflict means the provider is not used at all.
	//
	// AuditFactsReceived reports whether an attribution fact has ever been accepted and recorded on
	// this provider's audit route. It is True (Received) from the first one on and never goes back:
	// later silence is a quiet cluster, not a fault. Until then it is Unknown, with the reason
	// naming which silence it is and therefore where to look: TransportUnavailable (the fact
	// transport is refusing writes, so no route can be judged), NoAuditDelivery (nothing is posting
	// audit to this operator at all), or RouteUnused (audit is arriving and has never carried this
	// route). It is absent when the operator runs with author attribution disabled. It is not part
	// of Ready.
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

// KubeProvider is a connection to a Kubernetes cluster and the identity used there. A GitTarget
// mirrors from it; an applier claim reaches its applier through it. It holds the credential that reaches it (a kubeconfig Secret in its own
// namespace, or the operator's own identity where the installation allows it), which control-cluster
// namespaces may use it (its own, and those spec.accessFrom admits), and the audit route its
// attribution facts are partitioned under.
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

// AdmitsNamespace reports whether a consumer in namespace (by name and labels) may use this
// provider: its own namespace always, another one only as spec.accessFrom admits it. A malformed
// selector is an error, never a silent allow. The labels are always CONTROL-cluster Namespace
// labels.
func (p *KubeProvider) AdmitsNamespace(nsName string, nsLabels map[string]string) (bool, error) {
	if nsName == p.Namespace {
		return true, nil
	}
	return p.Spec.AccessFrom.Matches(nsName, nsLabels)
}

// AllowsCrossNamespaceSources reports whether a WatchRule mirroring through this provider may
// watch namespaces other than its own (spec.allowCrossNamespace.sources).
func (p *KubeProvider) AllowsCrossNamespaceSources() bool {
	return p.Spec.AllowCrossNamespace != nil && p.Spec.AllowCrossNamespace.Sources
}

// AllowsCrossNamespaceAppliers reports whether an applier claim reaching its applier through this provider
// may name one in another namespace than its own (spec.allowCrossNamespace.appliers).
func (p *KubeProvider) AllowsCrossNamespaceAppliers() bool {
	return p.Spec.AllowCrossNamespace != nil && p.Spec.AllowCrossNamespace.Appliers
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
