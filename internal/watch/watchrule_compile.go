// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"

	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// SourceScopeService is the source-scope service as its consumers need it: the policy resolution
// authz calls, plus the per-rule resolved-scope memory that separates ESTABLISHING a grant from
// MAINTAINING one. *Manager implements it; a nil value is legitimate and means "not wired yet"
// (a zero-value manager in tests, or a controller running before the data plane is up), which
// degrades to name-only policy evaluation rather than to a denial.
type SourceScopeService interface {
	authz.SourceNamespaceResolver

	// RetainedSourceNamespace reports the source namespace last granted to a rule, and whether
	// any grant was ever established for it.
	RetainedSourceNamespace(rule k8stypes.NamespacedName) (string, bool)
	// RecordSourceNamespaceGrant remembers a successful grant.
	RecordSourceNamespaceGrant(rule k8stypes.NamespacedName, namespace string)
	// ForgetSourceNamespaceGrant drops a rule's resolved scope on a refusal or a deletion.
	ForgetSourceNamespaceGrant(rule k8stypes.NamespacedName)
}

// CompileWatchRule is THE ONLY PATH from a WatchRule to a compiled rule. It runs the
// source-namespace gate first and compiles only on an admitted verdict.
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
//   - ADMITTED — the rule is compiled and its grant recorded. The caller publishes
//     SourceNamespaceAuthorized=True.
//   - TERMINAL (denied, or a permanently unevaluatable policy with no scope ever resolved) — any
//     previously compiled rule is REMOVED here, before the caller publishes anything. A gate that
//     only writes a condition is not a gate; the caller must still replan the watch manager and
//     then publish the Failed trio, in that order.
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
) (authz.SourceNamespaceDecision, error) {
	key := k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}

	decision, err := authz.WatchRuleSourceNamespaceAdmitted(ctx, reader, &rule, &target, resolverOf(scope))
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue. Tearing down a
		// running stream because the apiserver blipped is the failure this avoids.
		return decision, err
	}

	if decision.Admitted() {
		store.AddOrUpdateWatchRule(
			rule,
			target.Name, target.Namespace,
			provider.Name, provider.Namespace,
			target.Spec.Branch,
			target.Spec.Path,
		)
		if scope != nil {
			scope.RecordSourceNamespaceGrant(key, decision.Namespace)
		}
		return decision, nil
	}

	// An unevaluatable policy on a rule that ALREADY has a resolved scope is the maintaining case:
	// retain it. The rule keeps running on its last known-good grant and the caller reports
	// Unknown, not Failed — "cannot re-read the policy" is not "the policy says no".
	if decision.Verdict == authz.SourceScopeUnavailable && retainsScope(scope, key, decision.Namespace) {
		decision.Verdict = authz.SourceScopeUnknown
		return decision, nil
	}

	if decision.Terminal() {
		// Stop the data plane before the caller says anything about it.
		store.Delete(key)
		if scope != nil {
			scope.ForgetSourceNamespaceGrant(key)
		}
	}
	return decision, nil
}

// retainsScope reports whether a rule already holds a resolved grant for this same namespace. It
// is deliberately namespace-specific: a rule that EDITS spec.sourceNamespace is establishing a new
// grant, not maintaining its old one, so a stale grant for a different namespace must not let an
// unevaluatable policy through.
func retainsScope(scope SourceScopeService, rule k8stypes.NamespacedName, namespace string) bool {
	if scope == nil {
		return false
	}
	granted, ok := scope.RetainedSourceNamespace(rule)
	return ok && granted == namespace
}

// resolverOf adapts a possibly-nil service to the resolver authz takes, preserving the nil so
// authz's own "no source-scope service is wired" path (which answers Unknown, never Denied) runs.
func resolverOf(scope SourceScopeService) authz.SourceNamespaceResolver {
	if scope == nil {
		return nil
	}
	return scope
}
