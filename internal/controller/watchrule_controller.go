/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
)

// WatchRule status condition reasons.
const (
	WatchRuleReasonValidating             = "Validating"
	WatchRuleReasonGitRepoConfigNotFound  = "GitRepoConfigNotFound"
	WatchRuleReasonGitRepoConfigNotReady  = "GitRepoConfigNotReady"
	WatchRuleReasonAccessDenied           = "AccessDenied"
	WatchRuleReasonGitDestinationNotFound = "GitDestinationNotFound"
	WatchRuleReasonGitDestinationInvalid  = "GitDestinationInvalid"
	WatchRuleReasonReady                  = "Ready"
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
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("WatchRuleReconciler")

	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the WatchRule instance
	var watchRule configbutleraiv1alpha1.WatchRule
	//nolint:nestif // Deletion handling requires nested error checks
	if err := r.Get(ctx, req.NamespacedName, &watchRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("WatchRule not found, was likely deleted", "namespacedName", req.NamespacedName)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.Delete(req.NamespacedName)
			log.Info("WatchRule deleted, removed from store", "name", req.Name, "namespace", req.Namespace)

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
		"target", watchRule.Spec.Target,
		"generation", watchRule.Generation,
		"resourceVersion", watchRule.ResourceVersion)

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&watchRule, metav1.ConditionUnknown, //nolint:lll // Descriptive message
		WatchRuleReasonValidating, "Validating WatchRule configuration...")

	// Route by configuration surface (Target is required now)
	if watchRule.Spec.Target.Name == "" {
		r.setCondition(
			&watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified",
		)
		return r.updateStatusAndRequeue(ctx, &watchRule, RequeueShortInterval)
	}
	return r.reconcileWatchRuleViaTarget(ctx, &watchRule)
}

// reconcileWatchRuleViaTarget validates and stores a WatchRule that references a GitTarget.
func (r *WatchRuleReconciler) reconcileWatchRuleViaTarget(
	ctx context.Context,
	watchRule *configbutleraiv1alpha1.WatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileWatchRuleViaTarget")

	// Determine target namespace (same as WatchRule's namespace)
	targetNS := watchRule.Namespace

	// Fetch GitTarget
	var target configbutleraiv1alpha1.GitTarget
	targetKey := types.NamespacedName{Name: watchRule.Spec.Target.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", watchRule.Spec.Target.Name,
			"gitTargetNamespace", targetNS)
		r.setCondition(
			watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitDestinationNotFound,
			fmt.Sprintf(
				"Referenced GitTarget '%s/%s' not found: %v",
				targetNS,
				watchRule.Spec.Target.Name,
				err,
			),
		)
		return r.updateStatusAndRequeue(ctx, watchRule, RequeueShortInterval)
	}

	// Resolve GitProvider from target.Provider
	// TODO: Handle Flux GitRepository
	if target.Spec.Provider.Kind != "GitProvider" {
		// For now, only GitProvider is supported
		log.Info("Unsupported provider kind", "kind", target.Spec.Provider.Kind)
		// Continue for now, assuming GitProvider if not specified or default
	}

	providerName := target.Spec.Provider.Name
	// Provider is cluster-scoped (or namespaced? GitProvider is Namespaced in my implementation? No, let's check)
	// GitProvider is Namespaced in my implementation (api/v1alpha1/gitprovider_types.go says +kubebuilder:resource:scope=Namespaced)
	// But wait, GitProvider is usually cluster scoped in many systems, but here it seems namespaced.
	// Let's assume it's in the same namespace as GitTarget for now, or we need to check if GitProviderReference has Namespace.
	// GitProviderReference in gittarget_types.go does NOT have Namespace.
	// So it must be in the same namespace as GitTarget.

	providerNS := target.Namespace // Same as GitTarget

	var provider configbutleraiv1alpha1.GitProvider
	providerKey := types.NamespacedName{Name: providerName, Namespace: providerNS}
	if err := r.Get(ctx, providerKey, &provider); err != nil {
		log.Error(err, "Failed to resolve GitProvider from GitTarget",
			"gitProviderName", providerName, "gitProviderNamespace", providerNS)
		r.setCondition(
			watchRule,
			metav1.ConditionFalse,
			WatchRuleReasonGitRepoConfigNotFound, // Reuse reason for now
			fmt.Sprintf(
				"GitProvider '%s/%s' (from GitTarget) not found: %v",
				providerNS,
				providerName,
				err,
			),
		)
		return r.updateStatusAndRequeue(ctx, watchRule, RequeueShortInterval)
	}

	// Ready check (GitProvider doesn't have status conditions yet in my implementation? I added them)
	// I added GitProviderStatus with Conditions.
	// TODO: Check GitProvider readiness. For now assume ready if found.

	// Add rule to store with GitTarget reference and resolved values
	r.RuleStore.AddOrUpdateWatchRule(
		*watchRule,
		target.Name, targetNS, // GitTarget reference (replaces GitDestination)
		provider.Name, providerNS, // GitProvider reference (replaces GitRepoConfig)
		target.Spec.Branch,
		target.Spec.Path,
	)

	// Trigger WatchManager reconciliation for new/updated rule
	if r.WatchManager != nil {
		if err := r.WatchManager.ReconcileForRuleChange(ctx); err != nil {
			log.Error(err, "Failed to reconcile watch manager after rule update")
			// Don't fail the reconciliation - the rule is valid, just log the watch manager issue
		}
	}

	log.Info("WatchRule reconciliation via GitTarget successful", "name", watchRule.Name)
	return r.setReadyAndUpdateStatusWithTarget(ctx, watchRule, targetNS)
}

// setReadyAndUpdateStatusWithTarget sets Ready with target message and updates status with retry.
func (r *WatchRuleReconciler) setReadyAndUpdateStatusWithTarget(
	ctx context.Context,
	watchRule *configbutleraiv1alpha1.WatchRule,
	targetNS string,
) (ctrl.Result, error) {
	msg := fmt.Sprintf(
		"WatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		targetNS,
		watchRule.Spec.Target.Name,
	)
	r.setCondition(
		watchRule,
		metav1.ConditionTrue,
		WatchRuleReasonReady,
		msg,
	)
	if err := r.updateStatusWithRetry(ctx, watchRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueMediumInterval}, nil
}

// setCondition sets or updates the Ready condition.
func (r *WatchRuleReconciler) setCondition( //nolint:lll // Function signature
	watchRule *configbutleraiv1alpha1.WatchRule, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range watchRule.Status.Conditions {
		if existingCondition.Type == ConditionTypeReady {
			watchRule.Status.Conditions[i] = condition
			return
		}
	}

	watchRule.Status.Conditions = append(watchRule.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *WatchRuleReconciler) updateStatusAndRequeue( //nolint:lll // Function signature
	ctx context.Context, watchRule *configbutleraiv1alpha1.WatchRule, requeueAfter time.Duration) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, watchRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *WatchRuleReconciler) updateStatusWithRetry(
	ctx context.Context,
	watchRule *configbutleraiv1alpha1.WatchRule,
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
		latest := &configbutleraiv1alpha1.WatchRule{}
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.WatchRule{}).
		Named("watchrule").
		Complete(r)
}
