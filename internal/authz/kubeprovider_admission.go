// SPDX-License-Identifier: Apache-2.0

// Package authz holds the admission decisions for the objects that lend something: a GitProvider's
// credential (spec.allowedGitBranches), a GitBranch (spec.allowedGitTargets), and a KubeProvider's
// cluster connection (spec.allowedGitTargets, spec.allowedApplierClaims). Each grant list lives on
// the referenced object, and nothing is admitted automatically, the object's own namespace included.
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
	"strconv"
	"strings"

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

	// ReasonNamespaceNotAuthorized is the denial reason when no grant on the referenced object admits
	// the referring object's namespace — including a grant whose selector the apiserver accepted but
	// that does not convert to a selector.
	ReasonNamespaceNotAuthorized = "NamespaceNotAuthorized"

	// ReasonBranchNotAllowed is the denial reason when a GitProvider admits a GitBranch's namespace,
	// but none of the grants that admit it allows the branch the GitBranch claims.
	ReasonBranchNotAllowed = "BranchNotAllowed"

	// ReasonApplierNamespaceNotAllowed is the denial reason when an applier claim names an applier in
	// a namespace outside the applierNamespaces its KubeProvider grants it.
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

// GitTargetAdmitted reports whether target may mirror through the KubeProvider it references
// (spec.allowedGitTargets) and write through the GitBranch it references (that GitBranch's
// spec.allowedGitTargets), reading the provider, the GitBranch and the target's Namespace through
// reader.
//
// It is evaluated on every reconcile rather than only at admission, so a policy TIGHTENED after a
// GitTarget was created revokes it too. The two denial paths are deliberate and ordered: a missing
// provider is denied before any namespace policy is consulted, because an absent provider has no
// policy to consult and defaulting to "allow" would make an undeclared provider a bypass.
//
// A non-NotFound read error is returned as err so the caller requeues instead of tearing down a
// running data plane on a transient apiserver failure. A NotFound Namespace is NOT an error: the
// policy is then evaluated against empty labels, so a Names grant still admits a namespace that has
// not been created yet while a selector correctly does not match it.
func GitTargetAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
) (Decision, error) {
	decision, _, err := GitTargetSourceScope(ctx, reader, target)
	return decision, err
}

// GitTargetSourceScope is GitTargetAdmitted, plus what the grants that admit target let it mirror:
// the source namespaces its WatchRules may watch and the cluster-scoped resources a ClusterWatchRule
// may mirror through it. The scope is meaningful only when the decision allows.
func GitTargetSourceScope(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
) (Decision, configv1alpha3.KubeProviderGitTargetScope, error) {
	key := target.KubeProviderKey()
	decision, provider, err := kubeProviderUsable(ctx, reader, key, fmt.Sprintf(
		"referenced KubeProvider %s was not found; a GitTarget may mirror a source cluster only "+
			"through an existing KubeProvider, which the operator never creates", key))
	if err != nil || !decision.Allowed {
		return decision, configv1alpha3.KubeProviderGitTargetScope{}, err
	}
	nsLabels, err := namespaceLabels(ctx, reader, target.Namespace)
	if err != nil {
		return Decision{}, configv1alpha3.KubeProviderGitTargetScope{}, err
	}
	scope, selErr := provider.AdmitsGitTarget(client.ObjectKeyFromObject(target), nsLabels)
	if !scope.Admitted {
		return notAdmitted("KubeProvider", key, provider.DescribeAllowedGitTargets(), "spec.allowedGitTargets",
			target.Namespace, selErr), configv1alpha3.KubeProviderGitTargetScope{}, nil
	}
	if decision, err = AuditRouteHeld(ctx, reader, provider); err != nil || !decision.Allowed {
		return decision, configv1alpha3.KubeProviderGitTargetScope{}, err
	}
	if decision, err = gitBranchAdmitted(ctx, reader, target, nsLabels); err != nil || !decision.Allowed {
		return decision, configv1alpha3.KubeProviderGitTargetScope{}, err
	}
	return Decision{Allowed: true}, scope, nil
}

// gitBranchAdmitted reports whether target may write through the GitBranch it references: one of
// that GitBranch's spec.allowedGitTargets admits target's namespace. The GitBranch's own namespace
// is admitted only by a grant too. A GitBranch that does not exist admits nothing and denies nothing
// here: there is nothing to write through, and the GitTarget's status reports it missing.
func gitBranchAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
	nsLabels map[string]string,
) (Decision, error) {
	key := target.GitBranchKey()
	if key.Name == "" {
		return Decision{Allowed: true}, nil
	}
	branch := &configv1alpha3.GitBranch{}
	if err := reader.Get(ctx, key, branch); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("read GitBranch %s: %w", key, err)
	}
	admitted, selErr := branch.AdmitsGitTarget(target.Namespace, nsLabels)
	if !admitted {
		return notAdmitted("GitBranch", key, branch.DescribeAllowedGitTargets(), "spec.allowedGitTargets",
			target.Namespace, selErr), nil
	}
	return Decision{Allowed: true}, nil
}

// GitBranchClaimAdmitted reports whether branch may claim its spec.branch through provider, the
// GitProvider it references: a grant in provider's spec.allowedGitBranches admits the GitBranch's
// namespace, and one of the patterns those grants give it matches the branch. A namespace no grant
// admits is ReasonNamespaceNotAuthorized; an admitted one asking for a branch outside its patterns
// is ReasonBranchNotAllowed. A non-NotFound read of the Namespace is returned as err, so a caller
// keeps its last answer rather than revoking on a transient failure.
func GitBranchClaimAdmitted(
	ctx context.Context,
	reader client.Reader,
	branch *configv1alpha3.GitBranch,
	provider *configv1alpha3.GitProvider,
) (Decision, error) {
	nsLabels, err := namespaceLabels(ctx, reader, branch.Namespace)
	if err != nil {
		return Decision{}, err
	}
	key := client.ObjectKeyFromObject(provider)
	admission, selErr := provider.AdmitsGitBranch(client.ObjectKeyFromObject(branch), nsLabels, branch.Spec.Branch)
	switch {
	case !admission.Admitted:
		return notAdmitted("GitProvider", key, provider.DescribeAllowedGitBranches(), "spec.allowedGitBranches",
			branch.Namespace, selErr), nil
	case !admission.Allowed:
		return Decision{
			Reason: ReasonBranchNotAllowed,
			Message: fmt.Sprintf(
				"namespace %q may claim %s through GitProvider %s (spec.allowedGitBranches); requested branch %q "+
					"is not allowed", branch.Namespace, quoteAll(admission.BranchPatterns), key, branch.Spec.Branch),
		}, nil
	}
	return Decision{Allowed: true}, nil
}

// ApplierClaimAdmitted reports whether an applier claim may hold its applier through the KubeProvider
// it names (spec.kubeProviderRef): a grant in that provider's spec.allowedApplierClaims admits the
// applier claim's namespace, and the applier lives in one of the applierNamespaces those grants
// give it. The applier is reached with that provider's credential, so admission to mirror through
// it is no admission to apply.
func ApplierClaimAdmitted(
	ctx context.Context,
	reader client.Reader,
	claim configv1alpha3.ApplierClaim,
) (Decision, error) {
	key := claim.KubeProviderKey()
	decision, provider, err := kubeProviderUsable(ctx, reader, key, fmt.Sprintf(
		"KubeProvider %s named by %s %s/%s spec.kubeProviderRef was not found",
		key, claim.ClaimKind(), claim.GetNamespace(), claim.GetName()))
	if err != nil || !decision.Allowed {
		return decision, err
	}
	nsLabels, err := namespaceLabels(ctx, reader, claim.GetNamespace())
	if err != nil {
		return Decision{}, err
	}
	scope, selErr := provider.AdmitsApplierClaim(client.ObjectKeyFromObject(claim), nsLabels)
	if !scope.Admitted {
		return notAdmitted("KubeProvider", key, provider.DescribeAllowedApplierClaims(), "spec.allowedApplierClaims",
			claim.GetNamespace(), selErr), nil
	}
	if decision, err = AuditRouteHeld(ctx, reader, provider); err != nil || !decision.Allowed {
		return decision, err
	}
	applier := claim.Applier()
	if !scope.AllowsApplierNamespace(applier.Namespace) {
		return Decision{
			Reason: ReasonApplierNamespaceNotAllowed,
			Message: fmt.Sprintf(
				"KubeProvider %s lets namespace %q hold appliers in %s (spec.allowedApplierClaims); %s %s is in "+
					"namespace %q, which is not allowed", key, claim.GetNamespace(),
				quoteAll(scope.ApplierNamespaces), claim.ApplierKind(), applier, applier.Namespace),
		}, nil
	}
	return Decision{Allowed: true}, nil
}

// ApplierAdmitted reports whether target may ask the applier its applier claim holds to apply:
// target may mirror (GitTargetAdmitted), and the applier claim may hold its applier
// (ApplierClaimAdmitted). Both are always checked, also when the two name one KubeProvider from one
// namespace: mirroring and applying are granted separately. A KubeProvider's client is shared by
// every object that uses the cluster, so a client being available is no permission to use it.
func ApplierAdmitted(
	ctx context.Context,
	reader client.Reader,
	target *configv1alpha3.GitTarget,
	claim configv1alpha3.ApplierClaim,
) (Decision, error) {
	decision, err := GitTargetAdmitted(ctx, reader, target)
	if err != nil || !decision.Allowed {
		return decision, err
	}
	return ApplierClaimAdmitted(ctx, reader, claim)
}

// kubeProviderUsable reads the KubeProvider at key. notFound is the denial message for a provider
// that does not exist. It returns the provider it read once it exists.
func kubeProviderUsable(
	ctx context.Context,
	reader client.Reader,
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
	return Decision{Allowed: true}, provider, nil
}

// notAdmitted is the NamespaceNotAuthorized refusal of a grant list: what it admits, the field, and
// the namespace that asked. selErr is a grant whose selector does not convert, which admitted nobody.
func notAdmitted(
	kind string, key k8stypes.NamespacedName, admits, field, namespace string, selErr error,
) Decision {
	message := fmt.Sprintf("%s %s %s in %s; namespace %q is not admitted", kind, key, admits, field, namespace)
	if selErr != nil {
		message += fmt.Sprintf(" (a grant's selector is invalid: %v)", selErr)
	}
	return Decision{Reason: ReasonNamespaceNotAuthorized, Message: message}
}

// quoteAll renders a scope as quoted, comma-separated values, or "nothing" when it is empty.
func quoteAll(values []string) string {
	if len(values) == 0 {
		return "nothing"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return strings.Join(quoted, ", ")
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
// none: a Names grant still admits it, and a selector does not.
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
