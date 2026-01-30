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

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

// ClusterWatchRule status condition reasons.
const (
	ClusterWatchRuleReasonValidating            = "Validating"
	ClusterWatchRuleReasonGitProviderNotFound   = "GitRepoConfigNotFound"
	ClusterWatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	ClusterWatchRuleReasonAccessDenied          = "AccessDenied"
	ClusterWatchRuleReasonGitTargetNotFound     = "GitTargetNotFound"
	ClusterWatchRuleReasonGitDestinationInvalid = "GitDestinationInvalid"
	ClusterWatchRuleReasonReady                 = "Ready"
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
	var clusterRule configbutleraiv1alpha1.ClusterWatchRule
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

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&clusterRule, metav1.ConditionUnknown,
		ClusterWatchRuleReasonValidating, "Validating ClusterWatchRule configuration...")

	// Delegate to target-based reconciliation
	return r.reconcileClusterWatchRuleViaTarget(ctx, &clusterRule)
}

// reconcileClusterWatchRuleViaTarget validates and stores a ClusterWatchRule that references a GitTarget.
func (r *ClusterWatchRuleReconciler) reconcileClusterWatchRuleViaTarget(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileClusterWatchRuleViaTarget")

	// Target is required
	if clusterRule.Spec.TargetRef.Name == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.name must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// For ClusterWatchRule, target namespace must be specified
	targetNS := clusterRule.Spec.TargetRef.Namespace
	if targetNS == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"Target.namespace must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// Fetch GitTarget
	var target configbutleraiv1alpha1.GitTarget
	targetKey := types.NamespacedName{Name: clusterRule.Spec.TargetRef.Name, Namespace: targetNS}
	if err := r.Get(ctx, targetKey, &target); err != nil {
		log.Error(err, "Failed to get referenced GitTarget",
			"gitTargetName", clusterRule.Spec.TargetRef.Name,
			"gitTargetNamespace", targetNS)
		r.setCondition(
			clusterRule,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitTargetNotFound,
			fmt.Sprintf("Referenced GitTarget '%s/%s' not found: %v",
				targetNS, clusterRule.Spec.TargetRef.Name, err),
		)
		return r.updateStatusAndRequeue(ctx, clusterRule)
	}

	// Resolve GitProvider from target
	providerName := target.Spec.ProviderRef.Name
	providerNS := target.Namespace // Same as GitTarget

	var provider configbutleraiv1alpha1.GitProvider
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
	}

	log.Info("ClusterWatchRule reconciliation via GitTarget successful", "name", clusterRule.Name)
	return r.setReadyAndUpdateStatusWithTarget(ctx, clusterRule)
}

// setReadyAndUpdateStatusWithTarget sets Ready with target message and updates status with retry.
func (r *ClusterWatchRuleReconciler) setReadyAndUpdateStatusWithTarget(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
) (ctrl.Result, error) {
	msg := fmt.Sprintf(
		"ClusterWatchRule is ready and monitoring resources via GitTarget '%s/%s'",
		clusterRule.Spec.TargetRef.Namespace,
		clusterRule.Spec.TargetRef.Name,
	)
	r.setCondition(
		clusterRule,
		metav1.ConditionTrue,
		ClusterWatchRuleReasonReady,
		msg,
	)
	if err := r.updateStatusWithRetry(ctx, clusterRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueMediumInterval}, nil
}

// setCondition sets or updates the Ready condition.
func (r *ClusterWatchRuleReconciler) setCondition(
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
	status metav1.ConditionStatus,
	reason, message string,
) {
	condition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range clusterRule.Status.Conditions {
		if existingCondition.Type == ConditionTypeReady {
			clusterRule.Status.Conditions[i] = condition
			return
		}
	}

	clusterRule.Status.Conditions = append(clusterRule.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *ClusterWatchRuleReconciler) updateStatusAndRequeue(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, clusterRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *ClusterWatchRuleReconciler) updateStatusWithRetry(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
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
		latest := &configbutleraiv1alpha1.ClusterWatchRule{}
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
		For(&configbutleraiv1alpha1.ClusterWatchRule{}).
		Named("clusterwatchrule").
		Complete(r)
}
