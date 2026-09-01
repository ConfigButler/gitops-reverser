// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// SourceNamespaceAuthorized condition reasons, re-exported from internal/authz so the decision and
// the status surface can never drift apart.
//
// Three of the six went with the source-side selector: the condition can no longer be Unknown,
// because the gate reads only control-plane objects this reconcile already has, and it can no
// longer report an empty resolved scope, because "*" is one cluster-wide stream rather than a set
// that could come back empty.
const (
	// WatchRuleReasonLegacySourceNamespace is the True reason when every item watches the rule's
	// own namespace.
	WatchRuleReasonLegacySourceNamespace = authz.ReasonLegacySourceNamespace
	// WatchRuleReasonSourceNamespaceAllowed is the True reason when every item is admitted and at
	// least one names a namespace other than the rule's own — an authorized override or the
	// cluster-wide wildcard.
	WatchRuleReasonSourceNamespaceAllowed = authz.ReasonSourceNamespaceAllowed
	// WatchRuleReasonSourceNamespaceNotAllowed is the TERMINAL False reason for a refusal.
	WatchRuleReasonSourceNamespaceNotAllowed = authz.ReasonSourceNamespaceNotAllowed
)

// gateSourceNamespace is the WatchRule source-namespace gate and the ONE place this controller
// compiles a rule. It runs after the GitTarget and GitProvider are resolved and instead of a bare
// AddOrUpdateWatchRule, so there is no ungated path from a WatchRule to a compiled rule.
//
// The gate is cross-object (WatchRule → GitTarget → ClusterProvider), so it is not expressible in
// CEL and is a reconciler check rather than a webhook, per docs/spec/where-validation-lives.md —
// the same shape and ordering as checkSourceAuthorization. Running it on every reconcile is what
// makes a delegation WITHDRAWN after a rule was accepted revoke that rule.
//
// Every item is resolved, and the aggregate is published as one condition. A DENIED item refuses
// the whole rule rather than being trimmed away: mirroring two of the three namespaces a rule asked
// for is worse than a loud failure.
//
// It returns handled=false when the rule compiled and the reconcile should continue; handled=true
// means the reconcile is over and the caller must return the accompanying result and error
// unchanged.
func (r *WatchRuleReconciler) gateSourceNamespace(
	ctx context.Context,
	st *reconcileStatus,
	watchRule *configbutleraiv1alpha3.WatchRule,
	target configbutleraiv1alpha3.GitTarget,
	provider configbutleraiv1alpha3.GitProvider,
	log logr.Logger,
) (bool, ctrl.Result, error) {
	resolved, err := watch.CompileWatchRule(ctx, r.Client, r.RuleStore, *watchRule, target, provider)
	if err != nil {
		// A transient apiserver failure must NOT tear down a running stream: CompileWatchRule left
		// the compiled rule in place, so requeue with the error and re-run the gate on real data.
		log.Error(err, "Failed to evaluate source-namespace authorization",
			"gitTargetName", target.Name, "gitTargetNamespace", target.Namespace)
		return true, ctrl.Result{}, err
	}

	if resolved.Admitted() {
		st.set(
			ConditionTypeSourceNamespaceAuthorized,
			metav1.ConditionTrue,
			resolved.Reason,
			resolved.Message,
		)
		return false, ctrl.Result{}, nil
	}

	result, refuseErr := r.refuseSourceNamespace(ctx, st, watchRule, resolved, log)
	return true, result, refuseErr
}

// refuseSourceNamespace is the denial half of the gate.
//
// Order is the contract, not an implementation detail: CompileWatchRule has ALREADY removed the
// compiled rule, this replans the watch manager, and only then is the terminal status published. A
// gate that writes a condition while the stream keeps running is not a gate — so any test that
// asserts the terminal condition must also be able to assert the rule is already gone.
//
// The refusal is terminal (Stalled=True, Reconciling=False) rather than a retry: nothing this
// controller does will change the verdict. Recovery arrives as an EVENT — a ClusterProvider flag or
// accessFrom change — through the mappers registered in SetupWithManager.
func (r *WatchRuleReconciler) refuseSourceNamespace(
	ctx context.Context,
	st *reconcileStatus,
	watchRule *configbutleraiv1alpha3.WatchRule,
	resolved authz.ResolvedSourceScope,
	log logr.Logger,
) (ctrl.Result, error) {
	log.Info("Refusing WatchRule: its source-namespace scope is not authorized",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"reason", resolved.Reason,
		"message", resolved.Message)

	// The compiled rule is already out of the store; trigger a replan so the watch manager tears
	// down any stream this rule was keeping alive.
	if r.WatchManager != nil {
		r.WatchManager.TriggerRuleChange(watchRuleGitTarget(watchRule))
	}

	st.set(
		ConditionTypeSourceNamespaceAuthorized,
		metav1.ConditionFalse,
		resolved.Reason,
		resolved.Message,
	)
	st.set(
		ConditionTypeStreamsRunning,
		metav1.ConditionFalse,
		resolved.Reason,
		"No streams: the rule's source-namespace scope is not authorized",
	)

	return r.stallRule(ctx, st, resolved.Reason, resolved.Message)
}
