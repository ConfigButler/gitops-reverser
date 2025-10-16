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
	ClusterWatchRuleReasonValidating             = "Validating"
	ClusterWatchRuleReasonGitRepoConfigNotFound  = "GitRepoConfigNotFound"
	ClusterWatchRuleReasonGitRepoConfigNotReady  = "GitRepoConfigNotReady"
	ClusterWatchRuleReasonAccessDenied           = "AccessDenied"
	ClusterWatchRuleReasonGitDestinationNotFound = "GitDestinationNotFound"
	ClusterWatchRuleReasonGitDestinationInvalid  = "GitDestinationInvalid"
	ClusterWatchRuleReasonReady                  = "Ready"
)

// ClusterWatchRuleReconciler reconciles a ClusterWatchRule object.
type ClusterWatchRuleReconciler struct {
	client.Client

	Scheme    *runtime.Scheme
	RuleStore *rulestore.RuleStore
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterwatchrules/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterWatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("ClusterWatchRuleReconciler")

	log.Info("Starting reconciliation", "name", req.Name)

	// Fetch the ClusterWatchRule instance
	var clusterRule configbutleraiv1alpha1.ClusterWatchRule
	if err := r.Get(ctx, req.NamespacedName, &clusterRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ClusterWatchRule not found, was likely deleted", "name", req.Name)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.DeleteClusterWatchRule(req.NamespacedName)
			log.Info("ClusterWatchRule deleted, removed from store", "name", req.Name)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterWatchRule", "name", req.Name)
		return ctrl.Result{}, err
	}

	log.Info("Starting ClusterWatchRule validation",
		"name", clusterRule.Name,
		"destinationRef", clusterRule.Spec.DestinationRef,
		"generation", clusterRule.Generation,
		"resourceVersion", clusterRule.ResourceVersion)

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&clusterRule, metav1.ConditionUnknown,
		ClusterWatchRuleReasonValidating, "Validating ClusterWatchRule configuration...")

	// Delegate to destination-based reconciliation
	return r.reconcileClusterWatchRuleViaDestination(ctx, &clusterRule)
}

// reconcileClusterWatchRuleViaDestination validates and stores a ClusterWatchRule that references a GitDestination.
func (r *ClusterWatchRuleReconciler) reconcileClusterWatchRuleViaDestination(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("reconcileClusterWatchRuleViaDestination")

	// DestinationRef is required
	if clusterRule.Spec.DestinationRef == nil || clusterRule.Spec.DestinationRef.Name == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"DestinationRef.name must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule, RequeueShortInterval)
	}

	// For ClusterWatchRule, destination namespace must be specified
	destNS := clusterRule.Spec.DestinationRef.Namespace
	if destNS == "" {
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitDestinationInvalid,
			"DestinationRef.namespace must be specified for ClusterWatchRule")
		return r.updateStatusAndRequeue(ctx, clusterRule, RequeueShortInterval)
	}

	// Fetch GitDestination
	var dest configbutleraiv1alpha1.GitDestination
	destKey := types.NamespacedName{Name: clusterRule.Spec.DestinationRef.Name, Namespace: destNS}
	if err := r.Get(ctx, destKey, &dest); err != nil {
		log.Error(err, "Failed to get referenced GitDestination",
			"gitDestinationName", clusterRule.Spec.DestinationRef.Name,
			"gitDestinationNamespace", destNS)
		r.setCondition(
			clusterRule,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitDestinationNotFound,
			fmt.Sprintf("Referenced GitDestination '%s/%s' not found: %v",
				destNS, clusterRule.Spec.DestinationRef.Name, err),
		)
		return r.updateStatusAndRequeue(ctx, clusterRule, RequeueShortInterval)
	}

	// Resolve GitRepoConfig from destination
	grcNS := dest.Spec.RepoRef.Namespace
	if grcNS == "" {
		grcNS = dest.Namespace
	}
	grcKey := configbutleraiv1alpha1.NamespacedName{Name: dest.Spec.RepoRef.Name, Namespace: grcNS}
	gitRepoConfig, err := r.getGitRepoConfig(ctx, grcKey)
	if err != nil {
		log.Error(err, "Failed to resolve GitRepoConfig from GitDestination",
			"gitRepoConfigName", dest.Spec.RepoRef.Name, "gitRepoConfigNamespace", grcNS)
		r.setCondition(
			clusterRule,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonGitRepoConfigNotFound,
			fmt.Sprintf("GitRepoConfig '%s/%s' (from GitDestination) not found: %v",
				grcNS, dest.Spec.RepoRef.Name, err),
		)
		return r.updateStatusAndRequeue(ctx, clusterRule, RequeueShortInterval)
	}

	// Ready check
	if !r.isGitRepoConfigReady(gitRepoConfig) {
		log.Info("Resolved GitRepoConfig is not ready", "gitRepoConfig", gitRepoConfig.Name)
		r.setCondition(clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitRepoConfigNotReady,
			fmt.Sprintf("Resolved GitRepoConfig '%s/%s' is not ready", grcNS, dest.Spec.RepoRef.Name))
		return r.updateStatusAndRequeue(ctx, clusterRule, time.Minute)
	}

	// Access policy: cluster rules must be explicitly allowed
	if gitRepoConfig.Spec.AccessPolicy == nil || !gitRepoConfig.Spec.AccessPolicy.AllowClusterRules {
		log.Info("GitRepoConfig does not allow cluster rules", "gitRepoConfig", gitRepoConfig.Name)

		message := fmt.Sprintf(
			"GitRepoConfig '%s/%s' does not allow cluster rules "+
				"(accessPolicy.allowClusterRules is false or missing)",
			grcNS,
			dest.Spec.RepoRef.Name,
		)

		r.setCondition(
			clusterRule,
			metav1.ConditionFalse,
			ClusterWatchRuleReasonAccessDenied,
			message,
		)
		return r.updateStatusAndRequeue(ctx, clusterRule, RequeueMediumInterval)
	}

	// Add or update in store with resolved values (including baseFolder)
	r.RuleStore.AddOrUpdateClusterWatchRuleResolved(*clusterRule, gitRepoConfig.Name, grcNS, dest.Spec.BaseFolder)

	log.Info("ClusterWatchRule reconciliation via GitDestination successful", "name", clusterRule.Name)
	return r.setReadyAndUpdateStatusWithDestination(ctx, clusterRule)
}

// setReadyAndUpdateStatusWithDestination sets Ready with destination message and updates status with retry.
func (r *ClusterWatchRuleReconciler) setReadyAndUpdateStatusWithDestination(
	ctx context.Context,
	clusterRule *configbutleraiv1alpha1.ClusterWatchRule,
) (ctrl.Result, error) {
	msg := fmt.Sprintf(
		"ClusterWatchRule is ready and monitoring resources via GitDestination '%s/%s'",
		clusterRule.Spec.DestinationRef.Namespace,
		clusterRule.Spec.DestinationRef.Name,
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

// getGitRepoConfig retrieves the referenced GitRepoConfig.
func (r *ClusterWatchRuleReconciler) getGitRepoConfig(
	ctx context.Context,
	ref configbutleraiv1alpha1.NamespacedName,
) (*configbutleraiv1alpha1.GitRepoConfig, error) {
	var gitRepoConfig configbutleraiv1alpha1.GitRepoConfig
	gitRepoConfigKey := types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}

	if err := r.Get(ctx, gitRepoConfigKey, &gitRepoConfig); err != nil {
		return nil, err
	}

	return &gitRepoConfig, nil
}

// isGitRepoConfigReady checks if the GitRepoConfig has a Ready condition with status True.
func (r *ClusterWatchRuleReconciler) isGitRepoConfigReady(
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
) bool {
	for _, condition := range gitRepoConfig.Status.Conditions {
		if condition.Type == ConditionTypeReady && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
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
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, clusterRule); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
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
