// SPDX-License-Identifier: Apache-2.0

// Package authz holds the ClusterProvider namespace-admission decision.
//
// A ClusterProvider is cluster-scoped and holds a credential that can read a lot of a source
// cluster; its spec.allowedNamespaces is that provider's explicit, deny-by-default admission of
// the GitTarget NAMESPACES permitted to mirror through it.
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
	// admitted by its ClusterProvider's spec.allowedNamespaces — including the case where that
	// policy carries a selector the apiserver accepted but that does not convert to a selector.
	ReasonNamespaceNotAuthorized = "NamespaceNotAuthorized"
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
// target references, reading both the provider and the target's Namespace through reader.
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

	var provider configv1alpha3.ClusterProvider
	if err := reader.Get(ctx, k8stypes.NamespacedName{Name: providerName}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{
				Reason: ReasonClusterProviderNotFound,
				Message: fmt.Sprintf(
					"referenced ClusterProvider %q was not found; a GitTarget may mirror a source cluster "+
						"only through an existing ClusterProvider. The operator never creates one: declare it "+
						"yourself, or let the chart render %q via clusterProvider.createDefault",
					providerName, configv1alpha3.DefaultClusterProviderName),
			}, nil
		}
		return Decision{}, fmt.Errorf("read ClusterProvider %q: %w", providerName, err)
	}

	nsLabels := map[string]string{}
	var ns corev1.Namespace
	if err := reader.Get(ctx, k8stypes.NamespacedName{Name: target.Namespace}, &ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return Decision{}, fmt.Errorf("read namespace %q: %w", target.Namespace, err)
		}
	} else {
		nsLabels = ns.Labels
	}

	allowed, selErr := provider.AllowsNamespace(target.Namespace, nsLabels)
	if selErr != nil {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"ClusterProvider %q allowedNamespaces selector is invalid: %v", providerName, selErr),
		}, nil
	}
	if !allowed {
		return Decision{
			Reason: ReasonNamespaceNotAuthorized,
			Message: fmt.Sprintf(
				"namespace %q is not permitted to reference ClusterProvider %q (spec.allowedNamespaces)",
				target.Namespace, providerName),
		}, nil
	}

	return Decision{Allowed: true}, nil
}
