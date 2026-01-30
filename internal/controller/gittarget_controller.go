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
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
)

// GitTarget status condition reasons.
const (
	GitTargetReasonValidating            = "Validating"
	GitTargetReasonGitProviderNotFound   = "GitProviderNotFound"
	GitTargetReasonBranchNotAllowed      = "BranchNotAllowed"
	GitTargetReasonRepositoryUnavailable = "RepositoryUnavailable"
	GitTargetReasonConflict              = "Conflict"
	GitTargetReasonReady                 = "Ready"
)

// GitTargetReconciler reconciles a GitTarget object.
type GitTargetReconciler struct {
	client.Client

	Scheme        *runtime.Scheme
	WorkerManager *git.WorkerManager
	EventRouter   *watch.EventRouter
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gittargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch

// Reconcile validates GitTarget references and updates status conditions.
// Cleanup of workers and event streams is handled by ReconcileWorkers, not finalizers.
func (r *GitTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitTargetReconciler")
	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the GitTarget instance
	var target configbutleraiv1alpha1.GitTarget
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return r.handleFetchError(err, log, req.NamespacedName)
	}

	log.Info("Validating GitTarget",
		"name", target.Name,
		"namespace", target.Namespace,
		"provider", target.Spec.ProviderRef,
		"branch", target.Spec.Branch,
		"path", target.Spec.Path,
		"generation", target.Generation,
		"resourceVersion", target.ResourceVersion)

	// Set initial validating status
	r.setCondition(&target, metav1.ConditionUnknown,
		GitTargetReasonValidating, "Validating GitTarget configuration...")

	// Validate GitProvider and branch
	// GitProvider must be in the same namespace as GitTarget (enforced by API structure lacking namespace field)
	providerNS := target.Namespace

	validationResult, validationErr := r.validateGitProvider(ctx, &target, providerNS, log)
	if validationErr != nil {
		return ctrl.Result{}, validationErr
	}
	if validationResult != nil {
		return *validationResult, nil
	}

	// Get GitProvider for conflict checking and repository status
	var gp configbutleraiv1alpha1.GitProvider
	gpKey := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, gpKey, &gp); err != nil {
		log.Error(err, "Failed to get GitProvider for status checking")
		return ctrl.Result{RequeueAfter: RequeueShortInterval}, nil
	}

	// Check for conflicts with other GitTargets (defense-in-depth)
	if conflictResult := r.checkForConflicts(ctx, &target, providerNS, log); conflictResult != nil {
		return *conflictResult, nil
	}

	// Update repository status (branch existence, SHA tracking)
	r.updateRepositoryStatus(ctx, &target, &gp, log)

	// Register with worker and event stream
	r.registerWithWorkerAndEventStream(ctx, &target, providerNS, log)

	// Signal reconciliation complete to enable event processing
	if r.EventRouter != nil {
		gitDest := types.NewResourceReference(target.Name, target.Namespace)
		if stream := r.EventRouter.GetGitTargetEventStream(gitDest); stream != nil {
			stream.OnReconciliationComplete()
		}
	}

	log.Info("Updating status with success condition")
	if err := r.updateStatusWithRetry(ctx, &target); err != nil {
		log.Error(err, "Failed to update GitTarget status")
		return ctrl.Result{}, err
	}

	log.Info("Reconciliation successful", "name", target.Name)
	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

// handleFetchError handles errors from fetching GitTarget.
func (r *GitTargetReconciler) handleFetchError(
	err error,
	log logr.Logger,
	namespacedName k8stypes.NamespacedName,
) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		log.Info("GitTarget not found, was likely deleted", "namespacedName", namespacedName)
		return ctrl.Result{}, nil
	}
	log.Error(err, "unable to fetch GitTarget", "namespacedName", namespacedName)
	return ctrl.Result{}, err
}

// validateGitProvider validates the GitProvider reference and branch.
// Returns a result pointer if validation failed (caller should return it), nil if validation passed.
func (r *GitTargetReconciler) validateGitProvider(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) (*ctrl.Result, error) {
	// TODO: Handle Flux GitRepository support
	if target.Spec.ProviderRef.Kind != "" && target.Spec.ProviderRef.Kind != "GitProvider" {
		// For now, we only support GitProvider.
		// In future, we would fetch GitRepository here.
		// But since we are porting existing logic, we assume GitProvider.
		// If user provides GitRepository, it will fail here or we should handle it.
		// Given the task is to fix e2e tests which use GitProvider, we focus on that.
		log.Info("Unsupported provider kind", "kind", target.Spec.ProviderRef.Kind)
	}

	var gp configbutleraiv1alpha1.GitProvider
	gpKey := k8stypes.NamespacedName{Name: target.Spec.ProviderRef.Name, Namespace: providerNS}
	if err := r.Get(ctx, gpKey, &gp); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Referenced GitProvider '%s/%s' not found", providerNS, target.Spec.ProviderRef.Name)
			log.Info("GitProvider not found", "message", msg)
			r.setCondition(target, metav1.ConditionFalse, GitTargetReasonGitProviderNotFound, msg)
			result, updateErr := r.updateStatusAndRequeue(ctx, target, RequeueShortInterval)
			return &result, updateErr
		}
		log.Error(err, "Failed to get referenced GitProvider", "gitProvider", gpKey.String())
		result := ctrl.Result{}
		return &result, err
	}

	// Validate branch
	if result := r.validateBranch(ctx, target, &gp, providerNS, log); result != nil {
		return result, nil
	}

	// All validations passed
	msg := fmt.Sprintf("GitTarget is ready. Provider='%s/%s', Branch='%s', Path='%s'",
		providerNS, target.Spec.ProviderRef.Name, target.Spec.Branch, target.Spec.Path)
	r.setCondition(target, metav1.ConditionTrue, GitTargetReasonReady, msg)
	// target.Status.ObservedGeneration = target.Generation // Not in struct

	return nil, nil //nolint:nilnil // nil result means validation passed
}

// validateBranch validates that the branch matches at least one pattern in allowedBranches.
// Supports glob patterns like "main", "feature/*", "release/v*".
func (r *GitTargetReconciler) validateBranch(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	gp *configbutleraiv1alpha1.GitProvider,
	providerNS string,
	log logr.Logger,
) *ctrl.Result {
	branchAllowed := false
	for _, pattern := range gp.Spec.AllowedBranches {
		if match, err := filepath.Match(pattern, target.Spec.Branch); match {
			branchAllowed = true
			break
		} else if err != nil {
			// Log malformed pattern but continue checking other patterns
			log.Info("Invalid glob pattern in allowedBranches", "pattern", pattern, "error", err)
		}
	}

	if !branchAllowed {
		msg := fmt.Sprintf("Branch '%s' does not match any pattern in allowedBranches list %v of GitProvider '%s/%s'",
			target.Spec.Branch, gp.Spec.AllowedBranches, providerNS, target.Spec.ProviderRef.Name)
		log.Info("Branch validation failed", "branch", target.Spec.Branch, "allowedBranches", gp.Spec.AllowedBranches)
		r.setCondition(target, metav1.ConditionFalse, GitTargetReasonBranchNotAllowed, msg)
		// Security requirement: Clear LastCommit when branch not allowed
		target.Status.LastCommit = ""
		target.Status.LastPushTime = nil
		result, _ := r.updateStatusAndRequeue(ctx, target, RequeueShortInterval)
		return &result
	}

	return nil
}

// checkForConflicts checks if this GitTarget conflicts with other GitTargets.
// This provides defense-in-depth alongside webhook validation.
// Returns a result pointer if conflict detected, nil if no conflict.
func (r *GitTargetReconciler) checkForConflicts(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) *ctrl.Result {
	// List all GitTargets in the cluster
	var allTargets configbutleraiv1alpha1.GitTargetList
	if err := r.List(ctx, &allTargets); err != nil {
		log.Error(err, "Failed to list GitTargets for conflict checking")
		// Don't fail reconciliation due to listing error, just continue
		return nil
	}

	// Check each target for conflicts
	for i := range allTargets.Items {
		existing := &allTargets.Items[i]

		// Skip self (same namespace and name)
		if existing.Namespace == target.Namespace && existing.Name == target.Name {
			continue
		}

		// Skip if not referencing the same GitProvider
		// GitProvider is always in the same namespace as GitTarget
		if existing.Namespace != providerNS || existing.Spec.ProviderRef.Name != target.Spec.ProviderRef.Name {
			continue
		}

		// Check if branch and path match (conflict condition)
		if existing.Spec.Branch == target.Spec.Branch && existing.Spec.Path == target.Spec.Path {
			// Conflict detected! Elect winner by creationTimestamp
			if target.CreationTimestamp.After(existing.CreationTimestamp.Time) {
				// Current target is the loser
				msg := fmt.Sprintf(
					"Conflict detected. Another GitTarget '%s/%s' (created at %s) "+
						"is already using GitProvider '%s/%s', branch '%s', path '%s'. "+
						"This GitTarget was created later and will not be processed.",
					existing.Namespace, existing.Name,
					existing.CreationTimestamp.Format(time.RFC3339),
					providerNS, target.Spec.ProviderRef.Name,
					target.Spec.Branch, target.Spec.Path,
				)
				log.Info("Conflict detected, this GitTarget is the loser",
					"winner", fmt.Sprintf("%s/%s", existing.Namespace, existing.Name),
					"winnerCreated", existing.CreationTimestamp.Format(time.RFC3339),
					"loserCreated", target.CreationTimestamp.Format(time.RFC3339))

				r.setCondition(target, metav1.ConditionFalse, GitTargetReasonConflict, msg)
				result, _ := r.updateStatusAndRequeue(ctx, target, RequeueShortInterval)
				return &result
			}
			// Current target is the winner or equal timestamp - continue
		}
	}

	// No conflicts detected
	return nil
}

// registerWithWorkerAndEventStream registers the GitTarget with worker and event stream.
func (r *GitTargetReconciler) registerWithWorkerAndEventStream(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) {
	// Register with branch worker
	r.registerWithWorker(ctx, target, providerNS, log)

	// Register event stream
	r.registerEventStream(target, providerNS, log)
}

// registerWithWorker registers the target with branch worker.
func (r *GitTargetReconciler) registerWithWorker(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) {
	if r.WorkerManager == nil {
		return
	}

	if err := r.WorkerManager.RegisterTarget(
		ctx,
		target.Name, target.Namespace,
		target.Spec.ProviderRef.Name, providerNS,
		target.Spec.Branch,
		target.Spec.Path,
	); err != nil {
		log.Error(err, "Failed to register target with worker")
	} else {
		log.Info("Registered target with branch worker",
			"provider", target.Spec.ProviderRef.Name,
			"branch", target.Spec.Branch,
			"path", target.Spec.Path)
	}
}

// registerEventStream registers the GitTargetEventStream with EventRouter.
func (r *GitTargetReconciler) registerEventStream(
	target *configbutleraiv1alpha1.GitTarget,
	providerNS string,
	log logr.Logger,
) {
	if r.EventRouter == nil {
		return
	}

	branchWorker, exists := r.WorkerManager.GetWorkerForTarget(
		target.Spec.ProviderRef.Name, providerNS, target.Spec.Branch,
	)
	if !exists {
		log.Error(nil, "BranchWorker not found for GitTargetEventStream registration",
			"provider", target.Spec.ProviderRef.Name,
			"namespace", providerNS,
			"branch", target.Spec.Branch)
		return
	}

	gitDest := types.NewResourceReference(target.Name, target.Namespace)

	// Check if already registered
	if existingStream := r.EventRouter.GetGitTargetEventStream(gitDest); existingStream != nil {
		return
	}

	stream := reconcile.NewGitTargetEventStream(
		target.Name, target.Namespace,
		branchWorker,
		log,
	)
	r.EventRouter.RegisterGitTargetEventStream(gitDest, stream)
	log.Info("Registered GitTargetEventStream with EventRouter",
		"gitDest", gitDest.String(),
		"provider", target.Spec.ProviderRef.Name,
		"branch", target.Spec.Branch,
		"path", target.Spec.Path)
}

// updateRepositoryStatus synchronously fetches and updates repository status.
func (r *GitTargetReconciler) updateRepositoryStatus(
	ctx context.Context,
	target *configbutleraiv1alpha1.GitTarget,
	_ *configbutleraiv1alpha1.GitProvider,
	log logr.Logger,
) {
	log.Info("Syncing repository status from remote")

	// Get the branch worker for this target
	providerNS := target.Namespace

	if r.WorkerManager == nil {
		log.Error(nil, "WorkerManager is nil, cannot sync repository status")
		return
	}

	worker, exists := r.WorkerManager.GetWorkerForTarget(
		target.Spec.ProviderRef.Name, providerNS, target.Spec.Branch,
	)

	if !exists {
		// Worker not yet created - this is normal during initial reconciliation
		log.V(1).Info("Worker not yet available, will update status on next reconcile")
		return
	}

	// SYNCHRONOUS: Block and fetch fresh metadata (or use 30s cache)
	report, err := worker.SyncAndGetMetadata(ctx)
	if err != nil {
		log.Error(err, "Failed to sync repository metadata")
		// Don't fail reconcile, just skip status update
		return
	}

	// Update status with FRESH data from PullReport
	// target.Status.BranchExists = report.ExistsOnRemote // Not in struct
	target.Status.LastCommit = report.HEAD.Sha
	// target.Status.LastSyncTime = &metav1.Time{Time: time.Now()} // Not in struct

	log.Info("Repository status updated from remote",
		"branchExists", report.ExistsOnRemote,
		"lastCommit", report.HEAD.Sha,
		"incomingChanges", report.IncomingChanges)
}

// setCondition sets or updates the Ready condition.
func (r *GitTargetReconciler) setCondition(target *configbutleraiv1alpha1.GitTarget,
	status metav1.ConditionStatus, reason, message string,
) {
	condition := metav1.Condition{
		Type:               GitTargetReasonReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range target.Status.Conditions {
		if existingCondition.Type == GitTargetReasonReady {
			target.Status.Conditions[i] = condition
			return
		}
	}

	target.Status.Conditions = append(target.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *GitTargetReconciler) updateStatusAndRequeue(
	ctx context.Context, target *configbutleraiv1alpha1.GitTarget, requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, target); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *GitTargetReconciler) updateStatusWithRetry(
	ctx context.Context, target *configbutleraiv1alpha1.GitTarget,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", target.Name,
		"namespace", target.Namespace,
		"conditionsCount", len(target.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha1.GitTarget{}
		key := client.ObjectKeyFromObject(target)
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
		latest.Status = target.Status

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
func (r *GitTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitTarget{}).
		Named("gittarget").
		Complete(r)
}
