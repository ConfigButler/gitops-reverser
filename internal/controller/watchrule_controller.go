// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"sigs.k8s.io/controller-runtime/pkg/source"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// WatchRule status condition reasons.
const (
	WatchRuleReasonValidating            = "Validating"
	WatchRuleReasonGitProviderNotFound   = "GitRepoConfigNotFound"
	WatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	WatchRuleReasonAccessDenied          = "AccessDenied"
	WatchRuleReasonGitTargetNotFound     = "GitTargetNotFound"
	WatchRuleReasonGitDestinationInvalid = "GitDestinationInvalid"
	WatchRuleReasonReady                 = "Ready"
	WatchRuleReasonResourcesResolved     = "Resolved"
	WatchRuleReasonUnresolvedResources   = "UnresolvedResources"
)

// WatchRuleReconciler reconciles a WatchRule object.
type WatchRuleReconciler struct {
	client.Client

	Scheme       *runtime.Scheme
	RuleStore    *rulestore.RuleStore
	WatchManager WatchManagerInterface
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("WatchRuleReconciler")

	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the WatchRule instance
	var watchRule configbutleraiv1alpha3.WatchRule
	//nolint:nestif // Deletion handling requires nested error checks
	if err := r.Get(ctx, req.NamespacedName, &watchRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("WatchRule not found, was likely deleted", "namespacedName", req.NamespacedName)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.Delete(req.NamespacedName)
			log.Info("WatchRule deleted, removed from store", "name", req.Name, "namespace", req.Namespace)

			// Drop the retained source-scope grant with it. The grant is what tells the gate a rule
			// is MAINTAINING an already-resolved scope rather than ESTABLISHING one, and a rule that
			// no longer exists is neither. Left behind, it is inherited by the next rule created
			// under the same name and spec — a name a different tenant may now own — and an
			// unevaluatable policy then reads as "retaining a known-good scope" instead of "no
			// scope was ever established". The rule sits Unknown and Reconciling indefinitely
			// rather than publishing the terminal, actionable refusal that tells its owner the
			// policy cannot be evaluated. A recreated rule must establish from scratch.
			if scope := r.sourceScope(); scope != nil {
				scope.ForgetSourceScopeGrant(req.NamespacedName)
			}

			// Trigger WatchManager reconciliation for deletion
			if r.WatchManager != nil {
				if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
					log.Error(err, "Failed to reconcile watch manager after rule deletion")
					// Don't fail the reconciliation - log and continue
				}
			}

			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch WatchRule", "namespacedName", req.NamespacedName)
		return ctrl.Result{}, err
	}

	log.Info("Starting WatchRule validation",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"target", watchRule.Spec.TargetRef,
		"generation", watchRule.Generation,
		"resourceVersion", watchRule.ResourceVersion)
	watchRule.Status.ObservedGeneration = watchRule.Generation

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&watchRule, metav1.ConditionUnknown, //nolint:lll // Descriptive message
		WatchRuleReasonValidating, "Validating WatchRule configuration...")
	r.setTypedCondition(
		&watchRule,
		ConditionTypeStreamsRunning,
		metav1.ConditionUnknown,
		GitTargetStreamsRunningReasonNotReady,
		"Blocked by validation; streams not evaluated",
	)
	r.setTypedCondition(
		&watchRule,
		ConditionTypeGitTargetReady,
		metav1.ConditionUnknown,
		ReasonProgressing,
		"Blocked by validation; GitTarget not evaluated",
	)
	r.setTypedCondition(
		&watchRule,
		ConditionTypeSourceNamespaceAuthorized,
		metav1.ConditionUnknown,
		WatchRuleReasonCheckingSourceNamespacePolicy,
		"Blocked by validation; source namespace not evaluated",
	)
	r.setTypedCondition(
		&watchRule,
		ConditionTypeReconciling,
		metav1.ConditionTrue,
		ReasonChecking,
		"Validating WatchRule",
	)
	r.setTypedCondition(
		&watchRule,
		ConditionTypeStalled,
		metav1.ConditionFalse,
		ReasonChecking,
		"WatchRule is not stalled",
	)

	// Route by configuration surface (Target is required now)
	if watchRule.Spec.TargetRef.Name == "" {
		r.setCondition(
			&watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified",
		)
		r.setTypedCondition(
			&watchRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified",
		)
		r.setRuleStalled(&watchRule, WatchRuleReasonGitDestinationInvalid, "Target.name must be specified")
		return r.updateStatusAndRequeue(ctx, &watchRule)
	}
	return r.reconcileWatchRuleViaTarget(ctx, &watchRule)
}

// reconcileWatchRuleViaTarget validates and stores a WatchRule that references a GitTarget.
func (r *WatchRuleReconciler) reconcileWatchRuleViaTarget(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileWatchRuleViaTarget")

	// Determine target namespace (same as WatchRule's namespace)
	targetNS := watchRule.Namespace

	// Fetch GitTarget
	var target configbutleraiv1alpha3.GitTarget
	targetKey := types.NamespacedName{Name: watchRule.Spec.TargetRef.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", watchRule.Spec.TargetRef.Name,
			"gitTargetNamespace", targetNS)
		r.setCondition(
			watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitTargetNotFound,
			fmt.Sprintf(
				"Referenced GitTarget '%s/%s' not found: %v",
				targetNS,
				watchRule.Spec.TargetRef.Name,
				err,
			),
		)
		r.setTypedCondition(
			watchRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitTargetNotFound,
			"Referenced GitTarget not found",
		)
		r.setRuleStalled(watchRule, WatchRuleReasonGitTargetNotFound, "Referenced GitTarget not found")
		return r.updateStatusAndRequeue(ctx, watchRule)
	}
	r.setGitTargetReadyCondition(watchRule, target)

	// Resolve the GitProvider named by the target. A GitProviderReference is a
	// name-only reference to a GitProvider in the GitTarget's own namespace.
	providerName := target.Spec.ProviderRef.Name
	providerNS := target.Namespace // GitProvider is namespace-local to the GitTarget

	var provider configbutleraiv1alpha3.GitProvider
	providerKey := types.NamespacedName{Name: providerName, Namespace: providerNS}
	if err := r.Get(ctx, providerKey, &provider); err != nil {
		log.Error(err, "Failed to resolve GitProvider from GitTarget",
			"gitProviderName", providerName, "gitProviderNamespace", providerNS)
		r.setCondition(
			watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitProviderNotFound, // Reuse reason for now
			fmt.Sprintf(
				"GitProvider '%s/%s' (from GitTarget) not found: %v",
				providerNS,
				providerName,
				err,
			),
		)
		r.setTypedCondition(
			watchRule,
			ConditionTypeGitTargetReady,
			metav1.ConditionFalse,
			WatchRuleReasonGitProviderNotFound,
			"Referenced GitProvider not found",
		)
		r.setRuleStalled(watchRule, WatchRuleReasonGitProviderNotFound, "Referenced GitProvider not found")
		return r.updateStatusAndRequeue(ctx, watchRule)
	}

	// Ready check (GitProvider doesn't have status conditions yet in my implementation? I added them)
	// I added GitProviderStatus with Conditions.
	// TODO: Check GitProvider readiness. For now assume ready if found.

	// Source-namespace gate AND compilation, in that order and in one call — see
	// gateSourceNamespace. There is deliberately no AddOrUpdateWatchRule here: routing every
	// compilation through watch.CompileWatchRule is what stops the startup bootstrap from being a
	// second, ungated path into the store.
	if handled, result, err := r.gateSourceNamespace(ctx, watchRule, target, provider, log); handled {
		return result, err
	}

	// Trigger WatchManager reconciliation for new/updated rule
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after rule update")
			// Don't fail the reconciliation - the rule is valid, just log the watch manager issue
		}
		r.setResourceResolutionCondition(ctx, watchRule)
		r.setStreamsReadyCondition(watchRule, r.WatchManager.StreamSummaryForWatchRule(*watchRule))
	} else {
		r.setStreamsReadyCondition(watchRule, noResolvedStreamsSummary())
	}

	log.Info("WatchRule reconciliation via GitTarget successful", "name", watchRule.Name)
	return r.setReadyAndUpdateStatusWithTarget(ctx, watchRule, targetNS)
}

// setReadyAndUpdateStatusWithTarget sets Ready with target message and updates status with retry.
func (r *WatchRuleReconciler) setReadyAndUpdateStatusWithTarget(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
	targetNS string,
) (ctrl.Result, error) {
	msg := fmt.Sprintf(
		"WatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		targetNS,
		watchRule.Spec.TargetRef.Name,
	)
	r.setRuleKstatus(watchRule, msg)
	if err := r.updateStatusWithRetry(ctx, watchRule); err != nil {
		return ctrl.Result{}, err
	}
	if conditionIsFalse(watchRule.Status.Conditions, ConditionTypeResourcesResolved) {
		return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
	}
	if !conditionIsTrue(watchRule.Status.Conditions, ConditionTypeGitTargetReady) {
		return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
	}
	if !conditionIsTrue(watchRule.Status.Conditions, ConditionTypeStreamsRunning) {
		return ctrl.Result{RequeueAfter: RequeueStreamSettleInterval}, nil
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// setCondition sets or updates the Ready condition.
func (r *WatchRuleReconciler) setCondition( //nolint:lll // Function signature
	watchRule *configbutleraiv1alpha3.WatchRule, status metav1.ConditionStatus, reason, message string) {
	r.setTypedCondition(watchRule, ConditionTypeReady, status, reason, message)
}

func (r *WatchRuleReconciler) setGitTargetReadyCondition(
	watchRule *configbutleraiv1alpha3.WatchRule,
	target configbutleraiv1alpha3.GitTarget,
) {
	ready := gitTargetReadyCondition(target)
	r.setTypedCondition(watchRule, ConditionTypeGitTargetReady, ready.Status, ready.Reason, ready.Message)
}

func (r *WatchRuleReconciler) setResourceResolutionCondition(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
) {
	resolved, message := r.WatchManager.ResolveWatchRuleResources(ctx, *watchRule)
	status := metav1.ConditionFalse
	reason := WatchRuleReasonUnresolvedResources
	if resolved {
		status = metav1.ConditionTrue
		reason = WatchRuleReasonResourcesResolved
	}
	r.setTypedCondition(watchRule, ConditionTypeResourcesResolved, status, reason, message)
}

func (r *WatchRuleReconciler) setStreamsReadyCondition(
	watchRule *configbutleraiv1alpha3.WatchRule,
	streams watch.StreamSummary,
) {
	watchRule.Status.Streams = watchRuleStreamsStatus(streams)
	r.setTypedCondition(
		watchRule,
		ConditionTypeStreamsRunning,
		streamConditionStatus(streams),
		streams.Reason,
		streams.Message,
	)
}

func (r *WatchRuleReconciler) setRuleStalled(
	watchRule *configbutleraiv1alpha3.WatchRule,
	reason string,
	message string,
) {
	r.setTypedCondition(watchRule, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setTypedCondition(watchRule, ConditionTypeReconciling, metav1.ConditionFalse, reason, "Reconciliation is stalled")
	r.setTypedCondition(watchRule, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
}

func (r *WatchRuleReconciler) setRuleKstatus(
	watchRule *configbutleraiv1alpha3.WatchRule,
	readyMessage string,
) {
	applyRuleKstatus(
		watchRule.Status.Conditions,
		readyMessage,
		"WatchRule is not stalled",
		func(conditionType string, status metav1.ConditionStatus, reason, message string) {
			r.setTypedCondition(watchRule, conditionType, status, reason, message)
		},
		func(reason, message string) {
			r.setRuleStalled(watchRule, reason, message)
		},
	)
}

func (r *WatchRuleReconciler) setTypedCondition(
	watchRule *configbutleraiv1alpha3.WatchRule,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	watchRule.Status.Conditions = upsertCondition(
		watchRule.Status.Conditions,
		conditionType,
		status,
		reason,
		message,
		watchRule.Generation,
	)
}

func conditionIsFalse(conditions []metav1.Condition, conditionType string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionFalse
		}
	}
	return false
}

// updateStatusAndRequeue updates the status and requeues on the unified control-plane steady
// interval. The control plane no longer watches Secrets, so every status outcome falls back to
// this single cadence; see docs/rbac.md.
func (r *WatchRuleReconciler) updateStatusAndRequeue(
	ctx context.Context, watchRule *configbutleraiv1alpha3.WatchRule) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, watchRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *WatchRuleReconciler) updateStatusWithRetry(
	ctx context.Context,
	watchRule *configbutleraiv1alpha3.WatchRule,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"conditionsCount", len(watchRule.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha3.WatchRule{}
		key := client.ObjectKeyFromObject(watchRule)
		if err := r.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
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
		latest.Status = watchRule.Status

		log.Info("Attempting to update status",
			"conditionsCount", len(latest.Status.Conditions))

		// Attempt to update
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
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
func (r *WatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha3.WatchRule{}).
		// GenerationChangedPredicate keeps these watches reacting to a freshly
		// applied or spec-changed dependency while ignoring the status-only
		// updates the controllers write themselves — without it every GitTarget
		// or GitProvider heartbeat would re-list and re-enqueue all WatchRules.
		Watches(
			&configbutleraiv1alpha3.GitTarget{},
			handler.EnqueueRequestsFromMapFunc(r.gitTargetToWatchRules),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&configbutleraiv1alpha3.GitProvider{},
			handler.EnqueueRequestsFromMapFunc(r.gitProviderToWatchRules),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// React to a ClusterProvider's allowWatchRuleSourceNamespaceOverride (or allowedNamespaces)
		// changing. The GitTarget->WatchRules edge above CANNOT carry this: a ClusterProvider change
		// reaches the GitTarget as a STATUS update, which GenerationChangedPredicate deliberately
		// drops. Without this mapper, flipping the delegation flag would leave every affected
		// WatchRule un-reconciled until its periodic requeue — so a REVOCATION would take minutes.
		Watches(
			&configbutleraiv1alpha3.ClusterProvider{},
			handler.EnqueueRequestsFromMapFunc(r.clusterProviderToWatchRules),
			builder.WithPredicates(clusterProviderReadyOrSpecChanged()),
		).
		Named("watchrule")

	// React to a SOURCE-cluster Namespace label change, which grants or revokes any rule whose
	// GitTarget admits by selector. Those labels live in a cluster this controller has no client
	// for, so the watch manager observes them and pushes the affected GitTargets here; the
	// gitTargetToWatchRules mapper fans them out to the rules. See
	// internal/watch/source_namespace_scope.go.
	if r.WatchManager != nil {
		if events := r.WatchManager.SourceNamespaceEvents(); events != nil {
			b = b.WatchesRawSource(source.Channel(
				events,
				handler.EnqueueRequestsFromMapFunc(r.gitTargetToWatchRules),
			))
		}
	}

	return b.Complete(r)
}

// clusterProviderToWatchRules maps a ClusterProvider change to every WatchRule whose GitTarget
// mirrors through that provider, so a delegation grant or revocation converges on the event rather
// than on the periodic reconcile.
func (r *WatchRuleReconciler) clusterProviderToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	// A WatchRule's targetRef is a LocalTargetReference, so candidates always live in their
	// GitTarget's own namespace — collect the affected (namespace, target name) pairs.
	affected := make(map[types.NamespacedName]struct{}, len(targets.Items))
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.SourceCluster() != obj.GetName() {
			continue
		}
		affected[types.NamespacedName{Name: t.Name, Namespace: t.Namespace}] = struct{}{}
	}
	if len(affected) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		key := types.NamespacedName{Name: rule.Spec.TargetRef.Name, Namespace: rule.Namespace}
		if _, ok := affected[key]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}

// gitTargetToWatchRules maps a GitTarget event to every WatchRule in the
// GitTarget's namespace that references it. WatchRule.spec.targetRef is a
// LocalTargetReference, so candidates only live in the same namespace as the
// GitTarget.
func (r *WatchRuleReconciler) gitTargetToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.TargetRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}

// gitProviderToWatchRules maps a GitProvider event to every WatchRule (in the
// GitProvider's namespace) whose referenced GitTarget points at this provider.
// Mirrors the equivalent helper on ClusterWatchRuleReconciler so that an
// arriving provider doesn't have to wait for a separate GitTarget event to
// reach the rule.
func (r *WatchRuleReconciler) gitProviderToWatchRules(
	ctx context.Context,
	obj client.Object,
) []ctrlreconcile.Request {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "GitTargets", obj)
		return nil
	}

	matchingTargets := make(map[string]struct{})
	for i := range targets.Items {
		t := &targets.Items[i]
		if t.Spec.ProviderRef.Name == obj.GetName() {
			matchingTargets[t.Name] = struct{}{}
		}
	}
	if len(matchingTargets) == 0 {
		return nil
	}

	var rules configbutleraiv1alpha3.WatchRuleList
	if err := r.List(ctx, &rules, client.InNamespace(obj.GetNamespace())); err != nil {
		logDependencyListError(ctx, err, "WatchRules", obj)
		return nil
	}

	var requests []ctrlreconcile.Request
	for i := range rules.Items {
		rule := &rules.Items[i]
		if _, ok := matchingTargets[rule.Spec.TargetRef.Name]; !ok {
			continue
		}
		requests = append(requests, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: rule.Name, Namespace: rule.Namespace},
		})
	}
	return requests
}
