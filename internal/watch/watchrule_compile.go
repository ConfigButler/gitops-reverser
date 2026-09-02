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
// Its two outcomes map onto the two things the caller must do:
//
//   - ADMITTED — the rule is compiled with every item resolved to its source-namespace set. The
//     caller publishes SourceNamespaceAuthorized=True.
//   - DENIED — any previously compiled rule is REMOVED here, before the caller publishes anything.
//     A gate that only writes a condition is not a gate; the caller must still replan the watch
//     manager and then publish the Failed trio, in that order.
//
// There is no third "cannot say yet" outcome, and there used to be. It existed because the gate's
// selector half read Namespace labels in ANOTHER cluster, which could be syncing, unreachable or
// Forbidden, and a rule with an already-resolved scope had to RETAIN it through such a gap rather
// than narrow to the empty set (a narrowed set is the input to a sweep, so failing closed there
// would have deleted a tenant's Git content over an outage). Every input is now a control-plane
// object this reconcile already read, so there is nothing left to retain a scope through, and the
// per-rule grant memory that existed only to serve that retention is gone with it.
//
// Bootstrap cannot publish status (it runs before controllers start), so a rule denied there is
// simply not compiled and the first reconcile writes the terminal condition. That ordering — fail
// closed first, explain second — is correct, not a limitation.
func CompileWatchRule(
	ctx context.Context,
	reader client.Reader,
	store *rulestore.RuleStore,
	rule configv1alpha3.WatchRule,
	target configv1alpha3.GitTarget,
	provider configv1alpha3.GitProvider,
) (authz.ResolvedSourceScope, error) {
	key := k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}

	// A GitTarget still carrying a field this release removed compiles NOTHING. The check lives
	// here rather than only on the GitTarget's Validated gate because bootstrap seeds the store
	// before the first reconcile, on every restart, and a gate the reconciler alone enforced would
	// be bypassed for that whole window — the same reason the source-namespace gate is here.
	if refusal := authz.SupersededFieldRefusal(&target); refusal != "" {
		store.Delete(key)
		return authz.ResolvedSourceScope{
			Reason:  authz.ReasonSupersededFieldStored,
			Message: refusal,
		}, nil
	}

	resolved, err := authz.ResolveWatchRuleSourceScope(ctx, reader, &rule, &target)
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue. Tearing down a
		// running stream because the apiserver blipped is the failure this avoids.
		return resolved, err
	}

	if resolved.Admitted() {
		store.AddOrUpdateWatchRule(
			rule,
			itemNamespaces(resolved),
			target.Name, target.Namespace,
			provider.Name, provider.Namespace,
			target.Spec.Branch,
			target.Spec.Path,
		)
		return resolved, nil
	}

	// Stop the data plane before the caller says anything about it.
	store.Delete(key)
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

	// Same refusal as the WatchRule path, and for the same bootstrap reason. A ClusterWatchRule
	// selects cluster-scoped objects, which the removed field never bounded, so this does not
	// change what it mirrors — it refuses to run against a target nobody has migrated, so the
	// operator fixes one object rather than discovering it kind by kind.
	if refusal := authz.SupersededFieldRefusal(&target); refusal != "" {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{
			Reason:  authz.ReasonSupersededFieldStored,
			Message: refusal,
		}, nil
	}

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
	// because spec.accessFrom excludes it or because the provider does not exist at all.
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
