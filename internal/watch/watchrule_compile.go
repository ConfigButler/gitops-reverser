// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"

	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// SourceScopeService is the source-scope service as its consumers need it: the policy resolution
// and enumeration authz calls, plus the per-rule resolved-scope memory that separates ESTABLISHING
// a grant from MAINTAINING one. *Manager implements it; a nil value is legitimate and means "not
// wired yet" (a zero-value manager in tests, or a controller running before the data plane is up),
// which degrades to name-only policy evaluation rather than to a denial.
type SourceScopeService interface {
	authz.SourceNamespaceResolver

	// RetainedSourceScope reports the resolved scope last granted to a rule FOR A GIVEN SPEC, and
	// whether any grant was ever established for that spec.
	//
	// It is keyed by the rule's spec hash rather than by item index on purpose: retention applies
	// only while the spec is unchanged, so an edit discards the memory and re-establishes from
	// scratch, and a reorder can never let one item inherit another item's grant.
	RetainedSourceScope(rule k8stypes.NamespacedName, specHash string) ([][]string, bool)
	// RecordSourceScopeGrant remembers a successful whole-rule resolution.
	RecordSourceScopeGrant(rule k8stypes.NamespacedName, specHash string, namespaces [][]string)
	// ForgetSourceScopeGrant drops a rule's resolved scope on a refusal or a deletion.
	ForgetSourceScopeGrant(rule k8stypes.NamespacedName)
}

// CompileWatchRule is THE ONLY PATH from a WatchRule to a compiled rule. It resolves the whole
// per-item source-namespace scope first and compiles only on an admitted verdict.
//
// It is one function, called by both the WatchRule reconciler and the watch manager's startup
// bootstrap, because two call sites that each remember to check is an arrangement this codebase
// has already got wrong once. Bootstrap lists every WatchRule and seeds the store BEFORE the first
// reconcile, then marks the store ready — so a gate the reconciler alone enforced would be
// bypassed for the whole startup window, on EVERY restart, which is exactly when nobody is
// watching. Routing compilation through here closes that by construction rather than by
// discipline: there is no second place that can call AddOrUpdateWatchRule for a WatchRule.
//
// Its three outcomes map onto the three things the caller must do:
//
//   - ADMITTED — the rule is compiled with every item expanded to concrete namespaces, and the
//     resolved scope is recorded. The caller publishes SourceNamespaceAuthorized=True.
//   - TERMINAL (any item denied, or a permanently unevaluatable policy with no scope ever resolved
//     for this spec) — any previously compiled rule is REMOVED here, before the caller publishes
//     anything. A gate that only writes a condition is not a gate; the caller must still replan the
//     watch manager and then publish the Failed trio, in that order.
//   - CANNOT SAY YET (retryable), or a rule MAINTAINING an already-resolved scope through an
//     unevaluatable policy — nothing is compiled and nothing is removed. The caller leaves status
//     InProgress and retries. Never narrow to the empty set here: a narrowed set is the input to a
//     sweep, so failing closed while maintaining would delete a tenant's Git content over a
//     transient outage.
//
// Bootstrap cannot publish status (it runs before controllers start), so a rule denied there is
// simply not compiled and the first reconcile writes the terminal condition. That ordering — fail
// closed first, explain second — is correct, not a limitation.
func CompileWatchRule(
	ctx context.Context,
	reader client.Reader,
	store *rulestore.RuleStore,
	scope SourceScopeService,
	rule configv1alpha3.WatchRule,
	target configv1alpha3.GitTarget,
	provider configv1alpha3.GitProvider,
) (authz.ResolvedSourceScope, error) {
	key := k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}
	specHash := SourceScopeSpecHash(&rule)

	resolved, err := authz.ResolveWatchRuleSourceScope(ctx, reader, &rule, &target, resolverOf(scope))
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue. Tearing down a
		// running stream because the apiserver blipped is the failure this avoids.
		return resolved, err
	}

	if resolved.Admitted() {
		namespaces := itemNamespaces(resolved)
		store.AddOrUpdateWatchRule(
			rule,
			namespaces,
			target.Name, target.Namespace,
			provider.Name, provider.Namespace,
			target.Spec.Branch,
			target.Spec.Path,
		)
		if scope != nil {
			scope.RecordSourceScopeGrant(key, specHash, namespaces)
		}
		return resolved, nil
	}

	// An unevaluatable policy on a rule that ALREADY has a resolved scope FOR THIS SPEC is the
	// maintaining case: retain it. The rule keeps running on its last known-good grant and the
	// caller reports Unknown, not Failed — "cannot re-read the policy" is not "the policy says no".
	if resolved.Verdict == authz.SourceScopeUnavailable && retainsScope(scope, key, specHash) {
		resolved.Verdict = authz.SourceScopeUnknown
		return resolved, nil
	}

	if resolved.Terminal() {
		// Stop the data plane before the caller says anything about it.
		store.Delete(key)
		if scope != nil {
			scope.ForgetSourceScopeGrant(key)
		}
	}
	return resolved, nil
}

// itemNamespaces projects the resolved scope into the per-item slice the store compiles from.
func itemNamespaces(resolved authz.ResolvedSourceScope) [][]string {
	out := make([][]string, 0, len(resolved.Items))
	for _, item := range resolved.Items {
		out = append(out, item.Namespaces)
	}
	return out
}

// retainsScope reports whether a rule already holds a resolved grant for THIS spec. It is
// deliberately spec-specific: a rule whose items changed is establishing a new scope, not
// maintaining its old one, so a stale grant must not let an unevaluatable policy through.
func retainsScope(scope SourceScopeService, rule k8stypes.NamespacedName, specHash string) bool {
	if scope == nil {
		return false
	}
	_, ok := scope.RetainedSourceScope(rule, specHash)
	return ok
}

// resolverOf adapts a possibly-nil service to the resolver authz takes, preserving the nil so
// authz's own "no source-scope service is wired" path (which answers Unknown, never Denied) runs.
func resolverOf(scope SourceScopeService) authz.SourceNamespaceResolver {
	if scope == nil {
		return nil
	}
	return scope
}

// CompileClusterWatchRule is THE ONLY PATH from a ClusterWatchRule to a compiled cluster rule, and
// it is the compile-time half of the cluster-scope-only narrowing.
//
// Two refusals, both terminal, in this order:
//
//  1. the referenced GitTarget's namespace must be admitted by that target's ClusterProvider — a
//     ClusterWatchRule's targetRef carries a namespace, so it can name a target in ANY namespace
//     and widen that target's mirror scope cluster-wide;
//  2. the rule must not carry a stored scope other than "Cluster". Admission rejects the value on
//     write, but a pre-release object keeps it in etcd, and resolving it as if it had asked for
//     cluster scope would silently change what a running rule mirrors.
//
// Like CompileWatchRule it is shared by the reconciler and the startup bootstrap, so a restart
// cannot open an unauthorized or namespaced watch before the first reconcile can publish status.
func CompileClusterWatchRule(
	ctx context.Context,
	reader client.Reader,
	store *rulestore.RuleStore,
	rule configv1alpha3.ClusterWatchRule,
	target configv1alpha3.GitTarget,
	provider configv1alpha3.GitProvider,
) (ClusterWatchRuleDecision, error) {
	key := k8stypes.NamespacedName{Name: rule.Name}

	admitted, err := authz.GitTargetAdmitted(ctx, reader, &target)
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue.
		return ClusterWatchRuleDecision{}, err
	}
	if !admitted.Allowed {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{
			Reason: ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized,
			Message: fmt.Sprintf("ClusterWatchRule may not compile against GitTarget '%s/%s': %s",
				target.Namespace, target.Name, admitted.Message),
		}, nil
	}

	if rule.Spec.DeclaresNamespacedScope() {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{
			Reason:  ClusterWatchRuleReasonScopeNotSupported,
			Message: ClusterWatchRuleNamespacedScopeMessage,
		}, nil
	}

	store.AddOrUpdateClusterWatchRule(
		rule,
		target.Name, target.Namespace,
		provider.Name, provider.Namespace,
		target.Spec.Branch,
		target.Spec.Path,
	)
	return ClusterWatchRuleDecision{Admitted: true}, nil
}

// ClusterWatchRule compile refusal reasons. They live here rather than in the controller because
// bootstrap refuses on the same grounds without a controller in sight, and one vocabulary is what
// keeps the two from drifting.
const (
	// ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized is the terminal reason when the
	// referenced GitTarget's namespace is not admitted by that target's ClusterProvider — either
	// because spec.allowedNamespaces excludes it or because the provider does not exist at all.
	//
	// One rule-side reason covers both provider-side causes on purpose: from the ClusterWatchRule's
	// point of view the single fact that matters is that this rule may not compile against this
	// target. The Message carries which of the two it was.
	ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized = "GitTargetNamespaceNotAuthorized"

	// ClusterWatchRuleReasonScopeNotSupported is the terminal reason for a STORED ClusterWatchRule
	// that still selects namespaced resources through the removed scope choice.
	ClusterWatchRuleReasonScopeNotSupported = "ClusterScopeOnly"
)

// ClusterWatchRuleNamespacedScopeMessage is the operator-facing refusal for a stored
// scope: Namespaced. It names the replacement, because the migration is cross-kind and cannot be
// performed automatically.
const ClusterWatchRuleNamespacedScopeMessage = "ClusterWatchRule is cluster-scoped only; watch " +
	"namespaced resources with a WatchRule and `rules[].sourceNamespace`."

// ClusterWatchRuleDecision is the outcome of the shared ClusterWatchRule compile path.
type ClusterWatchRuleDecision struct {
	// Admitted reports whether the rule compiled.
	Admitted bool
	// Reason is the terminal condition reason when it did not.
	Reason string
	// Message explains the refusal to an operator.
	Message string
}
