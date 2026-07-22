// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// ClusterWatchRule status condition reasons.
const (
	ClusterWatchRuleReasonValidating            = "Validating"
	ClusterWatchRuleReasonGitProviderNotFound   = "GitRepoConfigNotFound"
	ClusterWatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	ClusterWatchRuleReasonAccessDenied          = "AccessDenied"
	ClusterWatchRuleReasonGitTargetNotFound     = "GitTargetNotFound"
	ClusterWatchRuleReasonGitDestinationInvalid = "GitDestinationInvalid"
	ClusterWatchRuleReasonReady                 = ReasonSucceeded
	ClusterWatchRuleReasonResourcesResolved     = "Resolved"
	ClusterWatchRuleReasonUnresolvedResources   = "UnresolvedResources"

	// ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized is the terminal reason when the
	// referenced GitTarget's namespace is not admitted by that target's ClusterProvider. It is
	// re-exported from internal/watch, where the shared compile path both bootstrap and this
	// reconciler call decides it, so the two can never drift.
	ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized = watch.ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized

	// ClusterWatchRuleReasonScopeNotSupported is the terminal reason for a STORED ClusterWatchRule
	// that still selects namespaced resources through the removed scope choice.
	ClusterWatchRuleReasonScopeNotSupported = watch.ClusterWatchRuleReasonScopeNotSupported
)

// ClusterWatchRuleReconciler reconciles a ClusterWatchRule object.
type ClusterWatchRuleReconciler struct {
	client.Client

	Scheme       *runtime.Scheme
	RuleStore    *rulestore.RuleStore
	WatchManager WatchManagerInterface
	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterWatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("ClusterWatchRuleReconciler")

	log.Info("Starting reconciliation", "name", req.Name)

	// Fetch the ClusterWatchRule instance
	var clusterRule configbutleraiv1alpha3.ClusterWatchRule
	//nolint:nestif // Deletion handling requires nested error checks
	if err := r.Get(ctx, req.NamespacedName, &clusterRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ClusterWatchRule not found, was likely deleted", "name", req.Name)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.DeleteClusterWatchRule(req.NamespacedName)
			log.Info("ClusterWatchRule deleted, removed from store", "name", req.Name)

			// Trigger WatchManager reconciliation for deletion
			if r.WatchManager != nil {
				if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
					log.Error(err, "Failed to reconcile watch manager after cluster rule deletion")
					// Don't fail the reconciliation - log and continue
				}
			}

			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterWatchRule", "name", req.Name)
		return ctrl.Result{}, err
	}

	log.Info("Starting ClusterWatchRule validation",
		"name", clusterRule.Name,
		"target", clusterRule.Spec.TargetRef,
		"generation", clusterRule.Generation,
		"resourceVersion", clusterRule.ResourceVersion)
	st := beginStatus(r.Client, r.Recorder, &clusterRule, &clusterRule.Status.Conditions)
	clusterRule.Status.ObservedGeneration = clusterRule.Generation

	// Seed the axis conditions as not-yet-evaluated. There is deliberately no placeholder write of
	// the Ready/Reconciling/Stalled trio here: every path below ends in exactly one applyReadiness.
	st.set(
		ConditionTypeStreamsRunning,
		metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady,
		"Blocked by validation; streams not evaluated",
	)
	st.set(
		ConditionTypeGitTargetReady,
		metav1.ConditionUnknown,
		ReasonProgressing,
		"Blocked by validation; GitTarget not evaluated",
	)

	// Delegate to target-based reconciliation
	return r.reconcileClusterWatchRuleViaTarget(ctx, st, &clusterRule)
}

// reconcileClusterWatchRuleViaTarget validates and stores a ClusterWatchRule that references a GitTarget.
func (r *ClusterWatchRuleReconciler) reconcileClusterWatchRuleViaTarget(
	ctx context.Context,
	st *reconcileStatus,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileClusterWatchRuleViaTarget")

	// Target is required
	if clusterRule.Spec.TargetRef.Name == "" {
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule",
		)
		return r.stallRule(ctx, st, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule")
	}

	// For ClusterWatchRule, target namespace must be specified
	targetNS := clusterRule.Spec.TargetRef.Namespace
	if targetNS == "" {
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule",
		)
		return r.stallRule(ctx, st, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule")
	}

	// Fetch GitTarget
	var target configbutleraiv1alpha3.GitTarget
	targetKey := types.NamespacedName{Name: clusterRule.Spec.TargetRef.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", clusterRule.Spec.TargetRef.Name,
			"gitTargetNamespace", targetNS)
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitTargetNotFound,
			fmt.Sprintf("Referenced GitTarget '%s/%s' not found: %v",
				targetNS, clusterRule.Spec.TargetRef.Name, err),
		)
		return r.stallRule(ctx, st, ClusterWatchRuleReasonGitTargetNotFound, "Referenced GitTarget not found")
	}
	r.setGitTargetReadyCondition(st, target)

	// Resolve GitProvider from target
	providerName := target.Spec.ProviderRef.Name
	providerNS := target.Namespace // Same as GitTarget

	var provider configbutleraiv1alpha3.GitProvider
	providerKey := types.NamespacedName{Name: providerName, Namespace: providerNS}
	if err := r.Get(ctx, providerKey, &provider); err != nil {
		log.Error(err, "Failed to resolve GitProvider from GitTarget",
			"gitProviderName", providerName, "gitProviderNamespace", providerNS)
		st.set(
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitProviderNotFound,
			fmt.Sprintf("GitProvider '%s/%s' (from GitTarget) not found: %v",
				providerNS, providerName, err),
		)
		return r.stallRule(ctx, st, ClusterWatchRuleReasonGitProviderNotFound, "Referenced GitProvider not found")
	}

	// Ready check
	// TODO: Check GitProvider readiness

	// Admission AND compilation, in that order and in one call — see gateClusterWatchRule. There is
	// deliberately no AddOrUpdateClusterWatchRule here: routing every compilation through
	// watch.CompileClusterWatchRule is what stops the startup bootstrap from being a second,
	// ungated path into the store.
	if handled, result, err := r.gateClusterWatchRule(ctx, st, clusterRule, target, provider, log); handled {
		return result, err
	}

	// Trigger WatchManager reconciliation for new/updated rule
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after cluster rule update")
			// Don't fail the reconciliation - the rule is valid, just log the watch manager issue
		}
		r.setResourceResolutionCondition(ctx, st, clusterRule)
		r.setStreamsReadyCondition(st, clusterRule, r.WatchManager.StreamSummaryForClusterWatchRule(*clusterRule))
	} else {
		r.setStreamsReadyCondition(st, clusterRule, noResolvedStreamsSummary())
	}

	log.Info("ClusterWatchRule reconciliation via GitTarget successful", "name", clusterRule.Name)

	msg := fmt.Sprintf(
		"ClusterWatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		clusterRule.Spec.TargetRef.Namespace,
		clusterRule.Spec.TargetRef.Name,
	)
	return r.commitRule(ctx, st, ruleReadiness(clusterRule.Status.Conditions, "ClusterWatchRule", msg))
}

// gateClusterWatchRule is the ClusterWatchRule gate and the ONE place this controller compiles a
// cluster rule. It runs the shared compile path, which applies two refusals in order:
//
//  1. the ClusterProvider namespace admission of the referenced GitTarget. A ClusterWatchRule is
//     cluster-scoped and its targetRef carries a REQUIRED namespace, so it may name a GitTarget in
//     ANY namespace and widen that target's mirror scope cluster-wide. Compiling such a rule
//     without re-applying the target's own provider admission would let it mirror through a
//     credential whose allowedNamespaces never admitted that target.
//  2. the cluster-scope-only narrowing: a STORED rule that still says `scope: Namespaced` compiles
//     no stream. Admission rejects the value on write, but a pre-release object keeps it in etcd.
//
// Both live in internal/watch rather than here because the startup bootstrap must apply exactly the
// same refusals BEFORE the first reconcile — otherwise every restart reopens the window they close.
//
// It returns handled=false when the rule compiled and the reconcile should continue; handled=true
// means the reconcile is over and the caller must return the accompanying result and error
// unchanged.
func (r *ClusterWatchRuleReconciler) gateClusterWatchRule(
	ctx context.Context,
	st *reconcileStatus,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	target configbutleraiv1alpha3.GitTarget,
	provider configbutleraiv1alpha3.GitProvider,
	log logr.Logger,
) (bool, ctrl.Result, error) {
	decision, err := watch.CompileClusterWatchRule(
		ctx, r.Client, r.RuleStore, *clusterRule, target, provider)
	if err != nil {
		// A transient apiserver failure must NOT tear down a running stream: leave the compiled
		// rule in place and requeue with the error so the gate re-runs on real data.
		log.Error(err, "Failed to evaluate admission for ClusterWatchRule",
			"gitTargetName", target.Name, "gitTargetNamespace", target.Namespace)
		return true, ctrl.Result{}, err
	}
	if decision.Admitted {
		return false, ctrl.Result{}, nil
	}

	result, refuseErr := r.refuseClusterWatchRule(ctx, st, clusterRule, decision, log)
	return true, result, refuseErr
}

// refuseClusterWatchRule is the denial half of the ClusterWatchRule gate.
//
// Order is the contract, not an implementation detail: CompileClusterWatchRule has ALREADY removed
// the compiled rule, this replans the watch manager, and only then is the terminal status
// published. A gate that only writes a condition is not a gate — it leaves the stream running while
// announcing that it is not. Any test that asserts the terminal condition must therefore also be
// able to assert the rule is already gone.
//
// The refusal is terminal (Stalled=True, Reconciling=False) rather than a retry: nothing this
// controller does will change the verdict. Recovery arrives as an event — a ClusterProvider policy
// change or a Namespace label change — through the mappers registered in SetupWithManager, or as an
// edit converting the rule to the cluster-only model.
func (r *ClusterWatchRuleReconciler) refuseClusterWatchRule(
	ctx context.Context,
	st *reconcileStatus,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	decision watch.ClusterWatchRuleDecision,
	log logr.Logger,
) (ctrl.Result, error) {
	log.Info("Refusing ClusterWatchRule",
		"name", clusterRule.Name,
		"reason", decision.Reason,
		"message", decision.Message)

	// The compiled rule is already out of the store; replan so the watch manager tears down a
	// stream this rule was keeping alive.
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after refusing cluster rule",
				"name", clusterRule.Name)
			// Don't fail the reconciliation: the rule is already out of the store, so the next
			// replan from any source converges. Publishing the refusal matters more than this.
		}
	}

	st.set(
		ConditionTypeGitTargetReady,
		metav1.ConditionFalse,
		decision.Reason,
		decision.Message,
	)
	st.set(
		ConditionTypeStreamsRunning,
		metav1.ConditionFalse,
		decision.Reason,
		"No streams: the ClusterWatchRule was refused",
	)

	return r.stallRule(ctx, st, decision.Reason, decision.Message)
}

// stallRule publishes a terminal ClusterWatchRule outcome and ends the reconcile.
func (r *ClusterWatchRuleReconciler) stallRule(
	ctx context.Context,
	st *reconcileStatus,
	reason, message string,
) (ctrl.Result, error) {
	rd := newRuleReadiness("ClusterWatchRule", "")
	rd.stalled(reason, message)
	return r.commitRule(ctx, st, rd)
}

// commitRule writes the trio, persists the status, and picks the requeue cadence from the same
// verdict — so the cadence can never disagree with what status says.
func (r *ClusterWatchRuleReconciler) commitRule(
	ctx context.Context,
	st *reconcileStatus,
	rd *readiness,
) (ctrl.Result, error) {
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueFor(rd)}, nil
}

func (r *ClusterWatchRuleReconciler) setGitTargetReadyCondition(
	st *reconcileStatus,
	target configbutleraiv1alpha3.GitTarget,
) {
	ready := gitTargetReadyCondition(target)
	st.setValue(ConditionTypeGitTargetReady, ready)
}

func (r *ClusterWatchRuleReconciler) setResourceResolutionCondition(
	ctx context.Context,
	st *reconcileStatus,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) {
	resolved, message := r.WatchManager.ResolveClusterWatchRuleResources(ctx, *clusterRule)
	status := metav1.ConditionFalse
	reason := ClusterWatchRuleReasonUnresolvedResources
	if resolved {
		status = metav1.ConditionTrue
		reason = ClusterWatchRuleReasonResourcesResolved
	}
	st.set(ConditionTypeResourcesResolved, status, reason, message)
}

func (r *ClusterWatchRuleReconciler) setStreamsReadyCondition(
	st *reconcileStatus,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	streams watch.StreamSummary,
) {
	clusterRule.Status.Streams = watchRuleStreamsStatus(streams)
	st.set(ConditionTypeStreamsRunning, streamConditionStatus(streams), streams.Reason, streams.Message)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterWatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// A For() predicate is not an optimisation here, it closes a self-triggering edge: a status
		// write bumps resourceVersion and fires an Update watch event that EnqueueRequestForObject
		// turns straight back into a queued request, un-rate-limited. reconcileStatus.commit()
		// already suppresses no-op writes, so the loop has no fuel; this makes it structural, and
		// matches what GitProvider and ClusterProvider already do.
		For(&configbutleraiv1alpha3.ClusterWatchRule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// GenerationChangedPredicate keeps these watches reacting to a freshly
		// applied or spec-changed dependency while ignoring the status-only
		// updates the controllers write themselves — without it every GitTarget
		// or GitProvider heartbeat would re-list and re-enqueue all
		// ClusterWatchRules.
		Watches(
			&configbutleraiv1alpha3.GitTarget{},
			handler.EnqueueRequestsFromMapFunc(r.gitTargetToClusterWatchRules),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToClusterWatchRules),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to a ClusterProvider's allowedNamespaces changing. Without this, REVOKING a
		// namespace stops the GitTarget (which does watch ClusterProvider) but leaves this rule's
		// compiled entry resident until the next periodic reconcile, so the admission gate would
		// converge on a ~10m delay instead of on the event. The GitTarget's own status flip cannot
		// carry it: that is a status-only update, which GenerationChangedPredicate above drops.
		Watches(
			&configbutleraiv1alpha3.ClusterProvider{},
			handler.EnqueueRequestsFromMapFunc(r.clusterProviderToClusterWatchRules),
			builder.WithPredicates(clusterProviderReadyOrSpecChanged()),
		).
		// React to a Namespace's LABELS changing: allowedNamespaces may admit by selector, so a
		// label change on a GitTarget's namespace grants or revokes every ClusterWatchRule pointing
		// at a target in it. LabelChangedPredicate ignores unrelated namespace churn.
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToClusterWatchRules),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		Named("clusterwatchrule").
		Complete(r)
}

// clusterProviderToClusterWatchRules maps a ClusterProvider policy change to every ClusterWatchRule
// whose referenced GitTarget mirrors through that provider, so an admission grant or revocation
// converges on the event rather than on the periodic reconcile.
func (r *ClusterWatchRuleReconciler) clusterProviderToClusterWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	affected := make(map[types.NamespacedName]struct{}, len(targets.Items))
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.SourceCluster() != obj.GetName() {
			continue
		}
		affected[types.NamespacedName{Name: t.Name, Namespace: t.Namespace}] = struct{}{}
	}

	return r.clusterWatchRulesTargeting(ctx, affected, obj)
}

// namespaceToClusterWatchRules maps a Namespace label change to every ClusterWatchRule whose
// referenced GitTarget lives in that namespace — the selector half of allowedNamespaces.
func (r *ClusterWatchRuleReconciler) namespaceToClusterWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetName())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	affected := make(map[types.NamespacedName]struct{}, len(targets.Items))
	for i := range targets.Items {
		t := &targets.Items[i]
		affected[types.NamespacedName{Name: t.Name, Namespace: t.Namespace}] = struct{}{}
	}

	return r.clusterWatchRulesTargeting(ctx, affected, obj)
}

// clusterWatchRulesTargeting returns a request for every ClusterWatchRule whose targetRef names one
// of the given GitTargets. Requests carry a name only — ClusterWatchRule is cluster-scoped.
func (r *ClusterWatchRuleReconciler) clusterWatchRulesTargeting(
	ctx context.Context,
	targets map[types.NamespacedName]struct{},
	obj client.Object,
) []ctrlreconcile.Request {
	if len(targets) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.ClusterWatchRuleList
	if err := r.List(ctx, &rules); err != nil {
		logDependencyListError(ctx, err, "ClusterWatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		key := types.NamespacedName{
			Name:      rule.Spec.TargetRef.Name,
			Namespace: rule.Spec.TargetRef.Namespace,
		}
		if _, ok := targets[key]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name},
		})
	}
	return requests
}

// gitTargetToClusterWatchRules maps a GitTarget event to every ClusterWatchRule
// whose targetRef matches it. ClusterWatchRule is cluster-scoped, so the lookup
// is cluster-wide.
func (r *ClusterWatchRuleReconciler) gitTargetToClusterWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var rules configbutleraiv1alpha3.ClusterWatchRuleList
	if err := r.List(ctx, &rules); err != nil {
		logDependencyListError(ctx, err, "ClusterWatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.TargetRef.Name != obj.GetName() {
			continue
		}
		if rule.Spec.TargetRef.Namespace != obj.GetNamespace() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name},
		})
	}
	return requests
}

// gitProviderToClusterWatchRules maps a GitProvider event to every
// ClusterWatchRule whose referenced GitTarget points at this provider. This
// closes the gap where a freshly-arrived GitProvider needs to nudge rules whose
// own GitTarget event would otherwise not fire (e.g. the GitTarget already
// existed but its provider lookup was failing).
func (r *ClusterWatchRuleReconciler) gitProviderToClusterWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	matchingTargets := make(map[types.NamespacedName]struct{})
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.Spec.ProviderRef.Name == obj.GetName() {
			matchingTargets[types.NamespacedName{Name: t.Name, Namespace: t.Namespace}] = struct{}{}
		}
	}
	if len(matchingTargets) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.ClusterWatchRuleList
	if err := r.List(ctx, &rules); err != nil {
		logDependencyListError(ctx, err, "ClusterWatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		key := types.NamespacedName{
			Name:      rule.Spec.TargetRef.Name,
			Namespace: rule.Spec.TargetRef.Namespace,
		}
		if _, ok := matchingTargets[key]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name},
		})
	}
	return requests
}
