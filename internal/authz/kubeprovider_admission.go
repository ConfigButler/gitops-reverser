// SPDX-License-Identifier: Apache-2.0

// Package authz holds the namespace-admission decisions for the providers a GitTarget uses.
//
// A KubeProvider holds a credential that can read a lot of a source cluster; it admits its own
// namespace, and its spec.accessFrom admits the OTHER control-cluster namespaces permitted to use
// it. A GitProvider holds a write credential, and admits namespaces the same way. A GitReceiver
// admits the GitProviders it serves the same way too (gitreceiver_admission.go).
//
// The decision lives in its own package — outside both internal/controller and internal/watch —
// because more than one call site compiles rules or starts a data plane for a GitTarget: the
// GitTarget reconciler, the ClusterWatchRule reconciler, and the watch manager's startup
// bootstrap. A gate that only one of them runs is not a gate, so every such call site routes
// through GitTargetAdmitted and one policy read answers all of them identically.
package authz

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

const (
	// ReasonKubeProviderNotFound is the denial reason when a referenced KubeProvider does not
	// exist. This is a HARD GATE: a GitTarget may mirror a source cluster ONLY through an existing
	// KubeProvider. The operator never creates one, so a target whose provider was never declared
	// is denied rather than mirroring on an implicit identity.
	ReasonKubeProviderNotFound = "KubeProviderNotFound"

	// ReasonNamespaceNotAuthorized is the denial reason when the consumer's namespace is neither the
	// KubeProvider's own nor admitted by its spec.accessFrom — including the case where that policy
	// carries a selector the apiserver accepted but that does not convert to a selector.
	ReasonNamespaceNotAuthorized = "NamespaceNotAuthorized"

	// ReasonApplierNamespaceNotAllowed is the denial reason when a trigger names an applier in
	// another namespace than its own and its KubeProvider does not set
	// spec.allowCrossNamespace.appliers.
	ReasonApplierNamespaceNotAllowed = "ApplierNamespaceNotAllowed"

	// ReasonAuditRouteConflict is the denial reason when a KubeProvider's audit route is held by a
	// namespace other than its own (v1alpha3.AuditRouteHolder). Such a provider is not used at all:
	// mirroring through it would read the attribution facts of a tenant it was never admitted to.
	ReasonAuditRouteConflict = "AuditRouteConflict"
)

// Decision is the outcome of a KubeProvider namespace-admission check. A denial always carries
// a Reason and an operator-legible Message; an admission carries neither.
type Decision struct {
	// Allowed reports whether the consumer's namespace may use the KubeProvider.
	Allowed bool
	// Reason is a CamelCase condition reason, set only when Allowed is false.
	Reason string
	// Message explains the denial to an operator, set only when Allowed is false.
	Message string
}

// GitTargetAdmitted reports whether target's namespace is admitted by the KubeProvider that
// target references, reading both the provider and the target's Namespace through reader, and by
// the GitProvider of the GitBranch it writes through (gitBranchAdmitted).
//
// It is evaluated on every reconcile rather than only at admission, so a policy TIGHTENED after a
// GitTarget was created revokes it too. The two denial paths are deliberate and ordered: a missing
// provider is denied before any namespace policy is consulted, because an absent provider has no
// policy to consult and defaulting to "allow" would make an undeclared provider a bypass.
//
// A non-NotFound read error is returned as err so the caller requeues instead of tearing down a
// running data plane on a transient apiserver failure. A NotFound Namespace is NOT an error: the
// policy is then evaluated against empty labels, so a `names` entry still admits a namespace that
// has not been created yet while a selector correctly does not match it.
func GitTargetAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
) (Decision, error) {
	key := target.KubeProviderKey()
	decision, _, err := namespaceAdmitted(ctx, reader, target.Namespace, key, fmt.Sprintf(
		"referenced KubeProvider %s was not found; a GitTarget may mirror a source cluster only "+
			"through an existing KubeProvider, which the operator never creates", key))
	if err != nil || !decision.Allowed {
		return decision, err
	}
	return gitBranchAdmitted(ctx, reader, target)
}

// gitBranchAdmitted reports whether target's namespace may write through the GitBranch it
// references. One in its own namespace always may. One in another namespace may only while the
// GitBranch's GitProvider admits target's namespace in its spec.accessFrom, because the GitTarget
// then writes with that provider's credential: one policy governs both hops, so it cannot be split
// across two objects that disagree. A GitBranch or GitProvider that does not exist admits nothing
// and denies nothing here: there is no credential to wield, and their status reports them missing.
func gitBranchAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
) (Decision, error) {
	key := target.GitBranchKey()
	if key.Name == "" || key.Namespace == target.Namespace {
		return Decision{Allowed: true}, nil
	}
	branch := &configv1alpha3.GitBranch{}
	if err := reader.Get(ctx, key, branch); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("read GitBranch %s: %w", key, err)
	}
	providerKey := branch.GitProviderKey()
	provider := &configv1alpha3.GitProvider{}
	if err := reader.Get(ctx, providerKey, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("read GitProvider %s: %w", providerKey, err)
	}
	nsLabels, err := namespaceLabels(ctx, reader, target.Namespace)
	if err != nil {
		return Decision{}, err
	}
	allowed, selErr := provider.AdmitsNamespace(target.Namespace, nsLabels)
	if selErr != nil {
		return Decision{
			Reason:  ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf("GitProvider %s accessFrom selector is invalid: %v", providerKey, selErr),
		}, nil
	}
	if !allowed {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"namespace %q is not permitted to reference GitBranch %s: its GitProvider %s does not admit it "+
					"(spec.accessFrom)", target.Namespace, key, providerKey),
		}, nil
	}
	return Decision{Allowed: true}, nil
}

// ApplyTriggerAdmitted reports whether a trigger's namespace is admitted by the KubeProvider it
// names (spec.kubeProviderRef), and whether it may reach its applier there. The applier is
// reached with that provider's credential, so the provider's accessFrom decides who may wield it,
// exactly as for mirroring, and its allowCrossNamespace.appliers whether a namespace may suspend
// and apply another namespace's applier. The trigger's own namespace is always allowed; in a remote
// cluster that is the namespace of the same name, as it is for mirroring.
func ApplyTriggerAdmitted(
	ctx context.Context,
	reader client.Reader,
	trigger configv1alpha3.ApplyTrigger,
) (Decision, error) {
	key := trigger.KubeProviderKey()
	decision, provider, err := namespaceAdmitted(ctx, reader, trigger.GetNamespace(), key, fmt.Sprintf(
		"KubeProvider %s named by %s %s/%s spec.kubeProviderRef was not found",
		key, trigger.TriggerKind(), trigger.GetNamespace(), trigger.GetName()))
	if err != nil || !decision.Allowed {
		return decision, err
	}
	applier := trigger.Applier()
	if applier.Namespace != trigger.GetNamespace() && !provider.AllowsCrossNamespaceAppliers() {
		return Decision{
			Reason: ReasonApplierNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"%s %s is in another namespace than the trigger's own %q, and KubeProvider %s does not "+
					"set spec.allowCrossNamespace.appliers", trigger.ApplierKind(), applier, trigger.GetNamespace(),
				key),
		}, nil
	}
	return decision, nil
}

// ApplierAdmitted reports whether target may ask the applier its trigger holds to apply: its
// namespace is admitted by its own KubeProvider (GitTargetAdmitted) and by the trigger's
// (ApplyTriggerAdmitted). A GitTarget names a trigger in its own namespace only, so the second
// answers for the GitTarget's namespace too. A KubeProvider's client is shared by every object
// that uses the cluster, so a client being available is no permission to use it.
func ApplierAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
	trigger configv1alpha3.ApplyTrigger,
) (Decision, error) {
	decision, err := GitTargetAdmitted(ctx, reader, target)
	if err != nil || !decision.Allowed {
		return decision, err
	}
	if trigger.KubeProviderKey() == target.KubeProviderKey() && trigger.GetNamespace() == target.Namespace {
		return decision, nil
	}
	return ApplyTriggerAdmitted(ctx, reader, trigger)
}

// namespaceAdmitted is the check both admissions share: the provider exists, it admits namespace
// (its own, or through spec.accessFrom), and it holds its audit route. notFound is the denial
// message for a provider that does not exist. It returns the provider it read once it exists.
func namespaceAdmitted(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	key k8stypes.NamespacedName,
	notFound string,
) (Decision, *configv1alpha3.KubeProvider, error) {
	if key.Name == "" {
		return Decision{
			Reason:  ReasonKubeProviderNotFound,
			Message: "spec.kubeProviderRef names no KubeProvider",
		}, nil, nil
	}
	provider := &configv1alpha3.KubeProvider{}
	if err := reader.Get(ctx, key, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Reason: ReasonKubeProviderNotFound, Message: notFound}, nil, nil
		}
		return Decision{}, nil, fmt.Errorf("read KubeProvider %s: %w", key, err)
	}

	nsLabels, err := namespaceLabels(ctx, reader, namespace)
	if err != nil {
		return Decision{}, nil, err
	}

	allowed, selErr := provider.AdmitsNamespace(namespace, nsLabels)
	if selErr != nil {
		return Decision{
			Reason:  ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf("KubeProvider %s accessFrom selector is invalid: %v", key, selErr),
		}, provider, nil
	}
	if !allowed {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"namespace %q is not permitted to reference KubeProvider %s: it is not the provider's own "+
					"namespace, and spec.accessFrom does not admit it", namespace, key),
		}, provider, nil
	}

	routeDecision, err := AuditRouteHeld(ctx, reader, provider)
	if err != nil || !routeDecision.Allowed {
		return routeDecision, provider, err
	}
	return Decision{Allowed: true}, provider, nil
}

// AuditRouteHeld reports whether provider's namespace holds the audit route provider carries
// (v1alpha3.AuditRouteHolder). A provider whose route another namespace holds is denied with
// ReasonAuditRouteConflict: it is not used at all, by any consumer.
//
// It reads every KubeProvider through reader, which on the manager's client is the informer cache.
// The holder is recomputed on every call rather than kept anywhere, so it can never disagree with
// the providers as they are.
func AuditRouteHeld(
	ctx context.Context,
	reader client.Reader,
	provider *configv1alpha3.KubeProvider,
) (Decision, error) {
	var providers configv1alpha3.KubeProviderList
	if err := reader.List(ctx, &providers); err != nil {
		return Decision{}, fmt.Errorf("list KubeProviders: %w", err)
	}
	route := provider.AuditRoute()
	// The provider itself is always a carrier, even when the cache has not seen it yet.
	providers.Items = append(providers.Items, *provider)
	holder, held := configv1alpha3.AuditRouteHolder(route, providers.Items)
	if !held || holder == provider.Namespace {
		return Decision{Allowed: true}, nil
	}
	return Decision{
		Reason: ReasonAuditRouteConflict,
		Message: fmt.Sprintf(
			"KubeProvider %s carries audit route %q, which a KubeProvider in namespace %q holds, so it is "+
				"not used: its attribution facts belong to that namespace. Set spec.attribution.auditRoute to "+
				"another route, or omit it for the default %q",
			provider.Key(), route, holder, configv1alpha3.DefaultAuditRoute(provider.Key())),
	}, nil
}

// namespaceLabels reads a control-cluster Namespace's labels. A Namespace that does not exist has
// none: a names entry still admits it, and a selector does not.
func namespaceLabels(ctx context.Context, reader client.Reader, namespace string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, k8stypes.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read namespace %q: %w", namespace, err)
	}
	return ns.Labels, nil
}
