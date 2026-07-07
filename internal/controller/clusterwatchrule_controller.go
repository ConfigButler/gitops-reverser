// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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
	ClusterWatchRuleReasonReady                 = "Ready"
	ClusterWatchRuleReasonResourcesResolved     = "Resolved"
	ClusterWatchRuleReasonUnresolvedResources   = "UnresolvedResources"
)

// ClusterWatchRuleReconciler reconciles a ClusterWatchRule object.
type ClusterWatchRuleReconciler struct {
	client.Client

	Scheme       *runtime.Scheme
	RuleStore    *rulestore.RuleStore
	WatchManager WatchManagerInterface
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch

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
	clusterRule.Status.ObservedGeneration = clusterRule.Generation

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&clusterRule, metav1.ConditionUnknown,
		ClusterWatchRuleReasonValidating, "Validating ClusterWatchRule configuration...")
	r.setTypedCondition(
		&clusterRule,
		ConditionTypeStreamsRunning,
		metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady,
		"Blocked by validation; streams not evaluated",
	)
	r.setTypedCondition(
		&clusterRule,
		ConditionTypeGitTargetReady,
		metav1.ConditionUnknown,
		ReasonProgressing,
		"Blocked by validation; GitTarget not evaluated",
	)
	r.setTypedCondition(&clusterRule, ConditionTypeReconciling, metav1.ConditionTrue, ReasonChecking,
		"Validating ClusterWatchRule")
	r.setTypedCondition(&clusterRule, ConditionTypeStalled, metav1.ConditionFalse, ReasonChecking,
		"ClusterWatchRule is not stalled")

	// Delegate to target-based reconciliation
	return r.reconcileClusterWatchRuleViaTarget(ctx, &clusterRule)
}

// reconcileClusterWatchRuleViaTarget validates and stores a ClusterWatchRule that references a GitTarget.
func (r *ClusterWatchRuleReconciler) reconcileClusterWatchRuleViaTarget(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileClusterWatchRuleViaTarget")

	// Target is required
	if clusterRule.Spec.TargetRef.Name == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule")
		r.setTypedCondition(
			clusterRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule",
		)
		r.setRuleStalled(clusterRule, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// For ClusterWatchRule, target namespace must be specified
	targetNS := clusterRule.Spec.TargetRef.Namespace
	if targetNS == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule")
		r.setTypedCondition(
			clusterRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule",
		)
		r.setRuleStalled(clusterRule, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// Fetch GitTarget
	var target configbutleraiv1alpha3.GitTarget
	targetKey := types.NamespacedName{Name: clusterRule.Spec.TargetRef.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", clusterRule.Spec.TargetRef.Name,
			"gitTargetNamespace", targetNS)
		r.setGitTargetNotFound(clusterRule, targetNS, err)
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}
	r.setGitTargetReadyCondition(clusterRule, target)

	// Resolve GitProvider from target
	providerName := target.Spec.ProviderRef.Name
	providerNS := target.Namespace // Same as GitTarget

	var provider configbutleraiv1alpha3.GitProvider
	providerKey := types.NamespacedName{Name: providerName, Namespace: providerNS}
	if err := r.Get(ctx, providerKey, &provider); err != nil {
		log.Error(err, "Failed to resolve GitProvider from GitTarget",
			"gitProviderName", providerName, "gitProviderNamespace", providerNS)
		r.setCondition(
			clusterRule,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitProviderNotFound,
			fmt.Sprintf("GitProvider '%s/%s' (from GitTarget) not found: %v",
				providerNS, providerName, err),
		)
		r.setTypedCondition(
			clusterRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitProviderNotFound,
			"Referenced GitProvider not found",
		)
		r.setRuleStalled(clusterRule, ClusterWatchRuleReasonGitProviderNotFound, "Referenced GitProvider not found")
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// Ready check
	// TODO: Check GitProvider readiness

	// Add rule to store with GitTarget reference and resolved values
	r.RuleStore.AddOrUpdateClusterWatchRule(
		*clusterRule,
		target.Name, targetNS, // GitTarget reference
		provider.Name, providerNS, // GitProvider reference
		target.Spec.Branch,
		target.Spec.Path,
	)

	// Trigger WatchManager reconciliation for new/updated rule
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after cluster rule update")
			// Don't fail the reconciliation - the rule is valid, just log the watch manager issue
		}
		r.setResourceResolutionCondition(ctx, clusterRule)
		r.setStreamsReadyCondition(clusterRule, r.WatchManager.StreamSummaryForClusterWatchRule(*clusterRule))
	} else {
		r.setStreamsReadyCondition(clusterRule, noResolvedStreamsSummary())
	}

	log.Info("ClusterWatchRule reconciliation via GitTarget successful", "name", clusterRule.Name)
	return r.setReadyAndUpdateStatusWithTarget(ctx, clusterRule)
}

// setReadyAndUpdateStatusWithTarget sets Ready with target message and updates status with retry.
func (r *ClusterWatchRuleReconciler) setReadyAndUpdateStatusWithTarget(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) (ctrl.Result, error) {
	msg := fmt.Sprintf(
		"ClusterWatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		clusterRule.Spec.TargetRef.Namespace,
		clusterRule.Spec.TargetRef.Name,
	)
	r.setRuleKstatus(clusterRule, msg)
	if err := r.updateStatusWithRetry(ctx, clusterRule); err != nil {
		return ctrl.Result{}, err
	}
	if conditionIsFalse(clusterRule.Status.Conditions, ConditionTypeResourcesResolved) {
		return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
	}
	if !conditionIsTrue(clusterRule.Status.Conditions, ConditionTypeGitTargetReady) {
		return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
	}
	if !conditionIsTrue(clusterRule.Status.Conditions, ConditionTypeStreamsRunning) {
		return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// setCondition sets or updates the Ready condition.
func (r *ClusterWatchRuleReconciler) setCondition(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	status metav1.ConditionStatus,
	reason, message string,
) {
	r.setTypedCondition(clusterRule, ConditionTypeReady, status, reason, message)
}

func (r *ClusterWatchRuleReconciler) setGitTargetNotFound(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	targetNS string,
	err error,
) {
	r.setCondition(
		clusterRule,
		metav1.ConditionFalse,
		ClusterWatchRuleReasonGitTargetNotFound,
		fmt.Sprintf("Referenced GitTarget '%s/%s' not found: %v", targetNS, clusterRule.Spec.TargetRef.Name, err),
	)
	r.setTypedCondition(
		clusterRule,
		ConditionTypeGitTargetReady,
		metav1.ConditionFalse,
		ClusterWatchRuleReasonGitTargetNotFound,
		"Referenced GitTarget not found",
	)
	r.setRuleStalled(clusterRule, ClusterWatchRuleReasonGitTargetNotFound, "Referenced GitTarget not found")
}

func (r *ClusterWatchRuleReconciler) setGitTargetReadyCondition(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	target configbutleraiv1alpha3.GitTarget,
) {
	ready := gitTargetReadyCondition(target)
	r.setTypedCondition(clusterRule, ConditionTypeGitTargetReady, ready.Status, ready.Reason, ready.Message)
}

func (r *ClusterWatchRuleReconciler) setResourceResolutionCondition(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) {
	resolved, message := r.WatchManager.ResolveClusterWatchRuleResources(ctx, *clusterRule)
	status := metav1.ConditionFalse
	reason := ClusterWatchRuleReasonUnresolvedResources
	if resolved {
		status = metav1.ConditionTrue
		reason = ClusterWatchRuleReasonResourcesResolved
	}
	r.setTypedCondition(clusterRule, ConditionTypeResourcesResolved, status, reason, message)
}

func (r *ClusterWatchRuleReconciler) setStreamsReadyCondition(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	streams watch.StreamSummary,
) {
	clusterRule.Status.Streams = watchRuleStreamsStatus(streams)
	r.setTypedCondition(
		clusterRule,
		ConditionTypeStreamsRunning,
		streamConditionStatus(streams),
		streams.Reason,
		streams.Message,
	)
}

func (r *ClusterWatchRuleReconciler) setRuleStalled(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	reason string,
	message string,
) {
	r.setTypedCondition(clusterRule, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setTypedCondition(
		clusterRule,
		ConditionTypeReconciling,
		metav1.ConditionFalse,
		reason,
		"Reconciliation is stalled",
	)
	r.setTypedCondition(clusterRule, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
}

func (r *ClusterWatchRuleReconciler) setRuleKstatus(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	readyMessage string,
) {
	applyRuleKstatus(
		clusterRule.Status.Conditions,
		readyMessage,
		ClusterWatchRuleReasonReady,
		"ClusterWatchRule is not stalled",
		func(conditionType string, status metav1.ConditionStatus, reason, message string) {
			r.setTypedCondition(clusterRule, conditionType, status, reason, message)
		},
		func(reason, message string) {
			r.setRuleStalled(clusterRule, reason, message)
		},
	)
}

func (r *ClusterWatchRuleReconciler) setTypedCondition(
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	clusterRule.Status.Conditions = upsertCondition(
		clusterRule.Status.Conditions,
		conditionType,
		status,
		reason,
		message,
		clusterRule.Generation,
	)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *ClusterWatchRuleReconciler) updateStatusAndRequeue(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, clusterRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *ClusterWatchRuleReconciler) updateStatusWithRetry(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha3.ClusterWatchRule,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", clusterRule.Name,
		"conditionsCount", len(clusterRule.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha3.ClusterWatchRule{}
		key := client.ObjectKeyFromObject(clusterRule)
		if err := r.Get(ctx, key, latest); err != nil {
			if k8serrors.IsNotFound(err) {
				log.Info("Resource was deleted, nothing to update")
				return true, nil
			}
			log.Error(err, "Failed to get latest resource version")
			return false, err
		}

		log.Info("Got latest resource version",
			"generation", latest.Generation,
			"resourceVersion", latest.ResourceVersion)

		// Copy our status to the latest version
		latest.Status = clusterRule.Status

		log.Info("Attempting to update status",
			"conditionsCount", len(latest.Status.Conditions))

		// Attempt to update
		if err := r.Status().Update(ctx, latest); err != nil {
			if k8serrors.IsConflict(err) {
				log.Info("Resource version conflict, retrying")
				return false, nil
			}
			log.Error(err, "Failed to update status")
			return false, err
		}

		log.Info("Status update successful")
		return true, nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterWatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.ClusterWatchRule{}).
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
		Named("clusterwatchrule").
		Complete(r)
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
