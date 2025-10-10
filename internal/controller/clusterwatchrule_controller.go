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

// ClusterWatchRule status condition reasons.
const (
	ClusterWatchRuleReasonValidating            = "Validating"
	ClusterWatchRuleReasonGitRepoConfigNotFound = "GitRepoConfigNotFound"
	ClusterWatchRuleReasonGitRepoConfigNotReady = "GitRepoConfigNotReady"
	ClusterWatchRuleReasonAccessDenied          = "AccessDenied"
	ClusterWatchRuleReasonInvalidSelector       = "InvalidSelector"
	ClusterWatchRuleReasonReady                 = "Ready"
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
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
//nolint:funlen // Controller reconcile functions are inherently long
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
		"gitRepoConfigRef", clusterRule.Spec.GitRepoConfigRef.Name,
		"namespace", clusterRule.Spec.GitRepoConfigRef.Namespace,
		"generation", clusterRule.Generation,
		"resourceVersion", clusterRule.ResourceVersion)

	// Set initial validating status
	log.Info("Setting initial validating status")
	r.setCondition(&clusterRule, metav1.ConditionUnknown,
		ClusterWatchRuleReasonValidating, "Validating ClusterWatchRule configuration...")

	// Step 1: Verify that the referenced GitRepoConfig exists
	log.Info("Verifying GitRepoConfig reference",
		"name", clusterRule.Spec.GitRepoConfigRef.Name,
		"namespace", clusterRule.Spec.GitRepoConfigRef.Namespace)

	gitRepoConfig, err := r.getGitRepoConfig(ctx, clusterRule.Spec.GitRepoConfigRef)
	if err != nil {
		log.Error(err, "Failed to get referenced GitRepoConfig",
			"name", clusterRule.Spec.GitRepoConfigRef.Name,
			"namespace", clusterRule.Spec.GitRepoConfigRef.Namespace)
		r.setCondition(&clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitRepoConfigNotFound,
			fmt.Sprintf("Referenced GitRepoConfig '%s/%s' not found: %v",
				clusterRule.Spec.GitRepoConfigRef.Namespace,
				clusterRule.Spec.GitRepoConfigRef.Name, err))
		return r.updateStatusAndRequeue(ctx, &clusterRule, RequeueShortInterval)
	}

	log.Info("GitRepoConfig found, checking if it's ready", "gitRepoConfig", gitRepoConfig.Name)

	// Step 2: Check if GitRepoConfig is ready
	if !r.isGitRepoConfigReady(gitRepoConfig) {
		log.Info("Referenced GitRepoConfig is not ready", "gitRepoConfig", gitRepoConfig.Name)
		r.setCondition(&clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonGitRepoConfigNotReady,
			fmt.Sprintf("Referenced GitRepoConfig '%s/%s' is not ready",
				clusterRule.Spec.GitRepoConfigRef.Namespace,
				clusterRule.Spec.GitRepoConfigRef.Name))
		return r.updateStatusAndRequeue(ctx, &clusterRule, time.Minute)
	}

	log.Info("GitRepoConfig is ready, checking access policy", "gitRepoConfig", gitRepoConfig.Name)

	// Step 3: Check if GitRepoConfig allows cluster rules
	if gitRepoConfig.Spec.AccessPolicy != nil {
		if !gitRepoConfig.Spec.AccessPolicy.AllowClusterRules {
			log.Info("GitRepoConfig does not allow cluster rules", "gitRepoConfig", gitRepoConfig.Name)
			r.setCondition(
				&clusterRule,
				metav1.ConditionFalse,
				ClusterWatchRuleReasonAccessDenied,
				fmt.Sprintf(
					"GitRepoConfig '%s/%s' does not allow cluster rules (accessPolicy.allowClusterRules is false)",
					clusterRule.Spec.GitRepoConfigRef.Namespace,
					clusterRule.Spec.GitRepoConfigRef.Name,
				),
			)
			return r.updateStatusAndRequeue(ctx, &clusterRule, RequeueMediumInterval)
		}
	} else {
		// Default: do not allow cluster rules (explicit opt-in required)
		log.Info("GitRepoConfig has no accessPolicy (defaults to no cluster rules)", "gitRepoConfig", gitRepoConfig.Name)
		r.setCondition(&clusterRule, metav1.ConditionFalse,
			ClusterWatchRuleReasonAccessDenied,
			fmt.Sprintf(
				"GitRepoConfig '%s/%s' must explicitly allow cluster rules via accessPolicy.allowClusterRules",
				clusterRule.Spec.GitRepoConfigRef.Namespace,
				clusterRule.Spec.GitRepoConfigRef.Name))
		return r.updateStatusAndRequeue(ctx, &clusterRule, RequeueMediumInterval)
	}

	log.Info("GitRepoConfig allows cluster rules, validating namespace selectors")

	// Step 4: Validate namespace selectors for Namespaced rules
	for i, rule := range clusterRule.Spec.Rules {
		if rule.Scope == configbutleraiv1alpha1.ResourceScopeNamespaced && rule.NamespaceSelector != nil {
			// Validate selector is well-formed
			_, err := metav1.LabelSelectorAsSelector(rule.NamespaceSelector)
			if err != nil {
				log.Error(err, "Invalid namespaceSelector", "ruleIndex", i)
				r.setCondition(&clusterRule, metav1.ConditionFalse, ClusterWatchRuleReasonInvalidSelector,
					fmt.Sprintf("Invalid namespaceSelector in rule %d: %v", i, err))
				return r.updateStatusAndRequeue(ctx, &clusterRule, RequeueShortInterval)
			}
		}
	}

	log.Info("All validations passed, adding ClusterWatchRule to store")

	// Step 5: Add or update the rule in the store
	r.RuleStore.AddOrUpdateClusterWatchRule(clusterRule)

	// Step 6: Set ready condition
	log.Info("ClusterWatchRule validation successful")
	r.setCondition(
		&clusterRule,
		metav1.ConditionTrue,
		ClusterWatchRuleReasonReady,
		fmt.Sprintf(
			"ClusterWatchRule is ready and monitoring resources with GitRepoConfig '%s/%s'",
			clusterRule.Spec.GitRepoConfigRef.Namespace,
			clusterRule.Spec.GitRepoConfigRef.Name,
		),
	)

	log.Info("ClusterWatchRule reconciliation successful", "name", clusterRule.Name)

	// Update status and schedule periodic revalidation
	log.Info("Updating status with success condition")
	if err := r.updateStatusWithRetry(ctx, &clusterRule); err != nil {
		log.Error(err, "Failed to update ClusterWatchRule status")
		return ctrl.Result{}, err
	}

	log.Info("Status update completed successfully, scheduling requeue", "requeueAfter", RequeueMediumInterval)
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
		latest.Status = clusterRule.Status

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
func (r *ClusterWatchRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.ClusterWatchRule{}).
		Named("clusterwatchrule").
		Complete(r)
}
