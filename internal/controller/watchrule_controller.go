/*
Copyright 2025.

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

	"k8s.io/apimachinery/pkg/api/errors"
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
	WatchRuleReasonValidating            = "Validating"
	WatchRuleReasonGitRepoConfigNotFound = "GitRepoConfigNotFound"
	WatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	WatchRuleReasonReady                 = "Ready"
)

// WatchRuleReconciler reconciles a WatchRule object.
type WatchRuleReconciler struct {
	client.Client

	Scheme    *runtime.Scheme
	RuleStore *rulestore.RuleStore
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=watchrules/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("WatchRuleReconciler")

	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the WatchRule instance
	var watchRule configbutleraiv1alpha1.WatchRule
	if err := r.Get(ctx, req.NamespacedName, &watchRule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("WatchRule not found, was likely deleted", "namespacedName", req.NamespacedName)
			// Resource was deleted. Remove it from the store.
			r.RuleStore.Delete(req.NamespacedName)
			log.Info("WatchRule deleted, removed from store", "name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch WatchRule", "namespacedName", req.NamespacedName)
		return ctrl.Result{}, err
	}

	log.Info("Starting WatchRule validation",
		"name", watchRule.Name,
		"namespace", watchRule.Namespace,
		"gitRepoConfigRef", watchRule.Spec.GitRepoConfigRef,
		"generation", watchRule.Generation,
		"resourceVersion", watchRule.ResourceVersion)

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&watchRule, metav1.ConditionUnknown, //nolint:lll // Descriptive message
		WatchRuleReasonValidating, "Validating WatchRule configuration...")

	// Step 1: Verify that the referenced GitRepoConfig exists and is ready
	log.Info("Verifying GitRepoConfig reference", "gitRepoConfigRef", watchRule.Spec.GitRepoConfigRef)
	gitRepoConfig, err := r.getGitRepoConfig(ctx, watchRule.Spec.GitRepoConfigRef, watchRule.Namespace)
	if err != nil {
		log.Error(err, "Failed to get referenced GitRepoConfig", "gitRepoConfigRef", watchRule.Spec.GitRepoConfigRef)
		r.setCondition(&watchRule, metav1.ConditionFalse, WatchRuleReasonGitRepoConfigNotFound,
			fmt.Sprintf("Referenced GitRepoConfig '%s' not found: %v", watchRule.Spec.GitRepoConfigRef, err))
		return r.updateStatusAndRequeue(ctx, &watchRule, RequeueShortInterval)
	}

	log.Info("GitRepoConfig found, checking if it's ready", "gitRepoConfig", gitRepoConfig.Name)

	// Step 2: Check if GitRepoConfig is ready
	if !r.isGitRepoConfigReady(gitRepoConfig) {
		log.Info("Referenced GitRepoConfig is not ready", "gitRepoConfig", gitRepoConfig.Name)
		r.setCondition(&watchRule, metav1.ConditionFalse, WatchRuleReasonGitRepoConfigNotReady,
			fmt.Sprintf("Referenced GitRepoConfig '%s' is not ready", watchRule.Spec.GitRepoConfigRef))
		return r.updateStatusAndRequeue(ctx, &watchRule, time.Minute)
	}

	log.Info("GitRepoConfig is ready, adding WatchRule to store", "gitRepoConfig", gitRepoConfig.Name)

	// Step 3: Add or update the rule in the store
	r.RuleStore.AddOrUpdate(watchRule)

	// Step 4: Set ready condition
	log.Info("WatchRule validation successful")
	r.setCondition(
		&watchRule,
		metav1.ConditionTrue,
		WatchRuleReasonReady, //nolint:lll // Descriptive message
		fmt.Sprintf(
			"WatchRule is ready and monitoring resources with GitRepoConfig '%s'",
			watchRule.Spec.GitRepoConfigRef,
		),
	)

	log.Info("WatchRule reconciliation successful", "name", watchRule.Name)

	// Update status and schedule periodic revalidation
	log.Info("Updating status with success condition")
	if err := r.updateStatusWithRetry(ctx, &watchRule); err != nil {
		log.Error(err, "Failed to update WatchRule status")
		return ctrl.Result{}, err
	}

	log.Info("Status update completed successfully, scheduling requeue", "requeueAfter", RequeueMediumInterval)
	return ctrl.Result{RequeueAfter: RequeueMediumInterval}, nil
}

// getGitRepoConfig retrieves the referenced GitRepoConfig
//
//nolint:lll // Function signature
func (r *WatchRuleReconciler) getGitRepoConfig(
	ctx context.Context,
	gitRepoConfigName, namespace string,
) (*configbutleraiv1alpha1.GitRepoConfig, error) {
	var gitRepoConfig configbutleraiv1alpha1.GitRepoConfig
	gitRepoConfigKey := types.NamespacedName{
		Name:      gitRepoConfigName,
		Namespace: namespace,
	}

	if err := r.Get(ctx, gitRepoConfigKey, &gitRepoConfig); err != nil {
		return nil, err
	}

	return &gitRepoConfig, nil
}

// isGitRepoConfigReady checks if the GitRepoConfig has a Ready condition with status True.
func (r *WatchRuleReconciler) isGitRepoConfigReady(gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig) bool {
	for _, condition := range gitRepoConfig.Status.Conditions {
		if condition.Type == ConditionTypeReady && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
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
			if errors.IsNotFound(err) {
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
			if errors.IsConflict(err) {
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
