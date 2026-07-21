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
const (
	// WatchRuleReasonLegacySourceNamespace is the True reason when every item watches the rule's
	// own namespace against a GitTarget that declares no allowedSourceNamespaces policy.
	WatchRuleReasonLegacySourceNamespace = authz.ReasonLegacySourceNamespace
	// WatchRuleReasonSourceNamespaceAllowed is the True reason when every item is admitted and at
	// least one names a namespace other than the rule's own — an authorized override or wildcard,
	// or an own-namespace item a declared policy explicitly lists.
	WatchRuleReasonSourceNamespaceAllowed = authz.ReasonSourceNamespaceAllowed
	// WatchRuleReasonNoAdmittedSourceNamespaces is the True reason when every item is admitted but
	// the resolved scope is EMPTY. Not stalled — but not silently healthy either.
	WatchRuleReasonNoAdmittedSourceNamespaces = authz.ReasonNoAdmittedSourceNamespaces
	// WatchRuleReasonSourceNamespaceNotAllowed is the TERMINAL False reason for a refusal.
	WatchRuleReasonSourceNamespaceNotAllowed = authz.ReasonSourceNamespaceNotAllowed
	// WatchRuleReasonSourceNamespacePolicyUnavailable is the reason for a selector policy that
	// cannot be evaluated as written. It is False/Stalled while ESTABLISHING and Unknown while
	// MAINTAINING an already-resolved scope — same reason, different claim about the rule.
	WatchRuleReasonSourceNamespacePolicyUnavailable = authz.ReasonSourceNamespacePolicyUnavailable
	// WatchRuleReasonCheckingSourceNamespacePolicy is the Unknown reason while the answer is still
	// being established or a retryable source-cluster error is being retried.
	WatchRuleReasonCheckingSourceNamespacePolicy = authz.ReasonCheckingSourceNamespacePolicy
)

// gateSourceNamespace is the WatchRule source-namespace gate and the ONE place this controller
// compiles a rule. It runs after the GitTarget and GitProvider are resolved and instead of a bare
// AddOrUpdateWatchRule, so there is no ungated path from a WatchRule to a compiled rule.
//
// The gate is cross-object (WatchRule → GitTarget → ClusterProvider) and its selector half needs
// source-cluster state, so it is not expressible in CEL and is a reconciler check rather than a
// webhook, per docs/spec/where-validation-lives.md — the same shape and ordering as
// checkSourceAuthorization. Running it on every reconcile is what makes a policy TIGHTENED after a
// rule was accepted revoke that rule.
//
// Every item is resolved, and the aggregate is published as one condition per the status contract's
// reason precedence. A DENIED explicit item refuses the whole rule rather than being trimmed away:
// mirroring two of the three namespaces a rule asked for is worse than a loud failure.
//
// It returns handled=false when the rule compiled and the reconcile should continue; handled=true
// means the reconcile is over and the caller must return the accompanying result and error
// unchanged.
func (r *WatchRuleReconciler) gateSourceNamespace(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
	target configbutleraiv1alpha3.GitTarget,
	provider configbutleraiv1alpha3.GitProvider,
	log logr.Logger,
) (bool, ctrl.Result, error) {
	resolved, err := watch.CompileWatchRule(
		ctx, r.Client, r.RuleStore, r.sourceScope(), *watchRule, target, provider)
	if err != nil {
		// A transient apiserver failure must NOT tear down a running stream: CompileWatchRule left
		// the compiled rule in place, so requeue with the error and re-run the gate on real data.
		log.Error(err, "Failed to evaluate source-namespace authorization",
			"gitTargetName", target.Name, "gitTargetNamespace", target.Namespace)
		return true, ctrl.Result{}, err
	}

	switch {
	case resolved.Admitted():
		r.setTypedCondition(
			watchRule,
			ConditionTypeSourceNamespaceAuthorized,
			metav1.ConditionTrue,
			resolved.Reason,
			resolved.Message,
		)
		return false, ctrl.Result{}, nil

	case resolved.Terminal():
		result, refuseErr := r.refuseSourceNamespace(ctx, watchRule, resolved, log)
		return true, result, refuseErr

	default:
		// Cannot say yet — the cache is syncing, a retryable source error is being retried, or a
		// rule with an already-resolved scope is retaining it through an unevaluatable policy. In
		// every case this is PROGRESSING, not failed: turning a temporary connection problem into
		// a terminal Stalled=True would stop a stream over an outage nobody chose.
		result, updateErr := r.holdSourceNamespaceUnknown(ctx, watchRule, resolved)
		return true, result, updateErr
	}
}

// refuseSourceNamespace is the denial half of the gate.
//
// Order is the contract, not an implementation detail: CompileWatchRule has ALREADY removed the
// compiled rule, this replans the watch manager, and only then is the terminal status published. A
// gate that writes a condition while the stream keeps running is not a gate — so any test that
// asserts the terminal condition must also be able to assert the rule is already gone.
//
// The refusal is terminal (Stalled=True, Reconciling=False) rather than a retry: nothing this
// controller does will change the verdict. Recovery arrives as an EVENT — a ClusterProvider flag
// or policy change, a GitTarget policy edit, or a source-cluster Namespace label change — through
// the mappers and channel registered in SetupWithManager.
func (r *WatchRuleReconciler) refuseSourceNamespace(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
	resolved authz.ResolvedSourceScope,
	log logr.Logger,
) (ctrl.Result, error) {
	log.Info("Refusing WatchRule: its source-namespace scope is not authorized",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"reason", resolved.Reason,
		"message", resolved.Message)

	// The compiled rule is already out of the store; replan so the watch manager tears down any
	// stream this rule was keeping alive.
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after refusing WatchRule",
				"name", watchRule.Name, "namespace", watchRule.Namespace)
			// Don't fail the reconciliation: the rule is already out of the store, so the next
			// replan from any source converges. Publishing the refusal matters more than this.
		}
	}

	r.setTypedCondition(
		watchRule,
		ConditionTypeSourceNamespaceAuthorized,
		metav1.ConditionFalse,
		resolved.Reason,
		resolved.Message,
	)
	r.setTypedCondition(
		watchRule,
		ConditionTypeStreamsRunning,
		metav1.ConditionFalse,
		resolved.Reason,
		"No streams: the rule's source-namespace scope is not authorized",
	)
	r.setRuleStalled(watchRule, resolved.Reason, resolved.Message)

	return r.updateStatusAndRequeue(ctx, watchRule)
}

// holdSourceNamespaceUnknown publishes the "cannot say yet" state: SourceNamespaceAuthorized is
// Unknown and the rule is Reconciling, never Stalled.
//
// Nothing is compiled and nothing is removed. A rule still ESTABLISHING a grant runs nothing; a
// rule MAINTAINING an already-resolved scope keeps both its compiled rule and its streams and only
// moves this condition. Neither may narrow to the empty set — a narrowed set is the input to a
// resync sweep, so failing closed here would delete a tenant's Git content over a transient
// outage.
func (r *WatchRuleReconciler) holdSourceNamespaceUnknown(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
	resolved authz.ResolvedSourceScope,
) (ctrl.Result, error) {
	r.setTypedCondition(
		watchRule,
		ConditionTypeSourceNamespaceAuthorized,
		metav1.ConditionUnknown,
		resolved.Reason,
		resolved.Message,
	)
	r.setTypedCondition(
		watchRule,
		ConditionTypeStreamsRunning,
		metav1.ConditionUnknown,
		resolved.Reason,
		"Streams not re-evaluated while source-namespace authorization is unsettled",
	)
	r.setRuleKstatus(watchRule, "WatchRule source-namespace authorization is unsettled")

	if err := r.updateStatusWithRetry(ctx, watchRule); err != nil {
		return ctrl.Result{}, err
	}
	// Retry on the fast settle cadence: the answer usually arrives with the next source-cluster
	// refresh, and the enqueue edge may not fire when nothing observably changed.
	return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
}

// sourceScope returns the source-scope service, or nil when the data plane is not wired. A nil
// service degrades selector policies to "cannot say yet" and leaves exact-NAME policies fully
// working — never a denial, which would refuse rules for a reason that has nothing to do with
// their configuration.
func (r *WatchRuleReconciler) sourceScope() watch.SourceScopeService {
	if r.WatchManager == nil {
		return nil
	}
	return r.WatchManager.SourceScope()
}
