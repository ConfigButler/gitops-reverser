// SPDX-License-Identifier: Apache-2.0

// Package authz holds the namespace-admission decisions for the providers a GitTarget uses.
//
// A ClusterProvider is cluster-scoped and holds a credential that can read a lot of a source
// cluster; its spec.accessFrom is that provider's explicit, deny-by-default admission of
// the GitTarget NAMESPACES permitted to mirror through it. A GitProvider is namespaced and holds a
// write credential; its spec.accessFrom admits the OTHER namespaces whose GitTargets may write
// through it.
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
	// ReasonClusterProviderNotFound is the denial reason when a GitTarget's referenced
	// ClusterProvider does not exist. This is a HARD GATE: a GitTarget may mirror a source cluster
	// ONLY through an existing ClusterProvider, "default" included. The operator never creates one,
	// so a target whose provider was never declared is denied rather than mirroring on an implicit
	// local identity.
	ReasonClusterProviderNotFound = "ClusterProviderNotFound"

	// ReasonNamespaceNotAuthorized is the denial reason when the GitTarget's namespace is not
	// admitted by its ClusterProvider's spec.accessFrom — including the case where that
	// policy carries a selector the apiserver accepted but that does not convert to a selector.
	ReasonNamespaceNotAuthorized = "NamespaceNotAuthorized"

	// ReasonApplierNamespaceNotAllowed is the denial reason when a trigger names an applier in
	// another namespace than its own and its ClusterProvider does not set
	// spec.allowCrossNamespace.appliers.
	ReasonApplierNamespaceNotAllowed = "ApplierNamespaceNotAllowed"
)

// Decision is the outcome of a ClusterProvider namespace-admission check. A denial always carries
// a Reason and an operator-legible Message; an admission carries neither.
type Decision struct {
	// Allowed reports whether the GitTarget's namespace may mirror through its ClusterProvider.
	Allowed bool
	// Reason is a CamelCase condition reason, set only when Allowed is false.
	Reason string
	// Message explains the denial to an operator, set only when Allowed is false.
	Message string
}

// GitTargetAdmitted reports whether target's namespace is admitted by the ClusterProvider that
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
	providerName := target.SourceCluster()
	decision, _, err := namespaceAdmitted(ctx, reader, target.Namespace, providerName, fmt.Sprintf(
		"referenced ClusterProvider %q was not found; a GitTarget may mirror a source cluster "+
			"only through an existing ClusterProvider. The operator never creates one: declare it "+
			"yourself, or let the chart render %q via clusterProvider.createDefault",
		providerName, configv1alpha3.DefaultClusterProviderName))
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

// ApplyTriggerAdmitted reports whether a trigger's namespace is admitted by the ClusterProvider it
// names (spec.clusterProviderRef), and whether it may reach its applier there. The applier is
// reached with that provider's credential, so the provider's accessFrom decides who may wield it,
// exactly as for mirroring, and its allowCrossNamespace.appliers whether a namespace may suspend
// and apply another namespace's applier. The trigger's own namespace is always allowed; in a remote
// cluster that is the namespace of the same name, as it is for mirroring.
func ApplyTriggerAdmitted(
	ctx context.Context,
	reader client.Reader,
	trigger configv1alpha3.ApplyTrigger,
) (Decision, error) {
	providerName := trigger.ClusterProvider()
	decision, provider, err := namespaceAdmitted(ctx, reader, trigger.GetNamespace(), providerName, fmt.Sprintf(
		"ClusterProvider %q named by %s %s/%s spec.clusterProviderRef was not found",
		providerName, trigger.TriggerKind(), trigger.GetNamespace(), trigger.GetName()))
	if err != nil || !decision.Allowed {
		return decision, err
	}
	applier := trigger.Applier()
	if applier.Namespace != trigger.GetNamespace() && !provider.AllowsCrossNamespaceAppliers() {
		return Decision{
			Reason: ReasonApplierNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"%s %s is in another namespace than the trigger's own %q, and ClusterProvider %q does not "+
					"set spec.allowCrossNamespace.appliers", trigger.ApplierKind(), applier, trigger.GetNamespace(),
				providerName),
		}, nil
	}
	return decision, nil
}

// ApplierAdmitted reports whether target may ask the applier its trigger holds to apply: its
// namespace is admitted by its own ClusterProvider (GitTargetAdmitted) and by the trigger's
// (ApplyTriggerAdmitted). A GitTarget names a trigger in its own namespace only, so the second
// answers for the GitTarget's namespace too. A ClusterProvider's client is shared by every object
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
	if trigger.ClusterProvider() == target.SourceCluster() && trigger.GetNamespace() == target.Namespace {
		return decision, nil
	}
	return ApplyTriggerAdmitted(ctx, reader, trigger)
}

// namespaceAdmitted is the check both admissions share: the provider exists, and its accessFrom
// admits namespace. notFound is the denial message for a provider that does not exist. It returns
// the provider it read once it exists.
func namespaceAdmitted(
	ctx context.Context,
	reader client.Reader,
	namespace, providerName, notFound string,
) (Decision, *configv1alpha3.ClusterProvider, error) {
	provider := &configv1alpha3.ClusterProvider{}
	if err := reader.Get(ctx, k8stypes.NamespacedName{Name: providerName}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Reason: ReasonClusterProviderNotFound, Message: notFound}, nil, nil
		}
		return Decision{}, nil, fmt.Errorf("read ClusterProvider %q: %w", providerName, err)
	}

	nsLabels, err := namespaceLabels(ctx, reader, namespace)
	if err != nil {
		return Decision{}, nil, err
	}

	allowed, selErr := provider.AllowsNamespace(namespace, nsLabels)
	if selErr != nil {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"ClusterProvider %q accessFrom selector is invalid: %v", providerName, selErr),
		}, provider, nil
	}
	if !allowed {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"namespace %q is not permitted to reference ClusterProvider %q (spec.accessFrom)",
				namespace, providerName),
		}, provider, nil
	}

	return Decision{Allowed: true}, provider, nil
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
