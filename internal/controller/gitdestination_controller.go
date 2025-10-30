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

// GitDestination status condition reasons.
const (
	GitDestinationReasonValidating            = "Validating"
	GitDestinationReasonGitRepoConfigNotFound = "GitRepoConfigNotFound"
	GitDestinationReasonBranchNotAllowed      = "BranchNotAllowed"
	GitDestinationReasonReady                 = "Ready"
)

// GitDestinationReconciler reconciles a GitDestination object.
type GitDestinationReconciler struct {
	client.Client

	Scheme        *runtime.Scheme
	WorkerManager *git.WorkerManager
	EventRouter   *watch.EventRouter
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitdestinations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitdestinations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitdestinations/finalizers,verbs=update
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch

// Reconcile validates GitDestination references and updates status conditions.
func (r *GitDestinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitDestinationReconciler")
	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the GitDestination instance
	var dest configbutleraiv1alpha1.GitDestination
	if err := r.Get(ctx, req.NamespacedName, &dest); err != nil {
		return r.handleFetchError(err, log, req.NamespacedName)
	}

	log.Info("Validating GitDestination",
		"name", dest.Name,
		"namespace", dest.Namespace,
		"repoRef", dest.Spec.RepoRef,
		"branch", dest.Spec.Branch,
		"baseFolder", dest.Spec.BaseFolder,
		"generation", dest.Generation,
		"resourceVersion", dest.ResourceVersion)

	// Set initial validating status
	r.setCondition(&dest, metav1.ConditionUnknown,
		GitDestinationReasonValidating, "Validating GitDestination configuration...")

	// Validate GitRepoConfig and branch
	repoNS := dest.Spec.RepoRef.Namespace
	if repoNS == "" {
		repoNS = dest.Namespace
	}

	validationResult, validationErr := r.validateGitRepoConfig(ctx, &dest, repoNS, log)
	if validationErr != nil {
		return ctrl.Result{}, validationErr
	}
	if validationResult != nil {
		return *validationResult, nil
	}

	// Register with worker and event stream
	r.registerWithWorkerAndEventStream(ctx, &dest, repoNS, log)

	log.Info("Updating status with success condition")
	if err := r.updateStatusWithRetry(ctx, &dest); err != nil {
		log.Error(err, "Failed to update GitDestination status")
		return ctrl.Result{}, err
	}

	log.Info("Reconciliation successful", "name", dest.Name)
	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

// handleFetchError handles errors from fetching GitDestination.
func (r *GitDestinationReconciler) handleFetchError(
	err error,
	log logr.Logger,
	namespacedName k8stypes.NamespacedName,
) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		log.Info("GitDestination not found, was likely deleted", "namespacedName", namespacedName)
		return ctrl.Result{}, nil
	}
	log.Error(err, "unable to fetch GitDestination", "namespacedName", namespacedName)
	return ctrl.Result{}, err
}

// validateGitRepoConfig validates the GitRepoConfig reference and branch.
// Returns a result pointer if validation failed (caller should return it), nil if validation passed.
func (r *GitDestinationReconciler) validateGitRepoConfig(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
	repoNS string,
	log logr.Logger,
) (*ctrl.Result, error) {
	var grc configbutleraiv1alpha1.GitRepoConfig
	grcKey := k8stypes.NamespacedName{Name: dest.Spec.RepoRef.Name, Namespace: repoNS}
	if err := r.Get(ctx, grcKey, &grc); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Referenced GitRepoConfig '%s/%s' not found", repoNS, dest.Spec.RepoRef.Name)
			log.Info("GitRepoConfig not found", "message", msg)
			r.setCondition(dest, metav1.ConditionFalse, GitDestinationReasonGitRepoConfigNotFound, msg)
			result, updateErr := r.updateStatusAndRequeue(ctx, dest, RequeueShortInterval)
			return &result, updateErr
		}
		log.Error(err, "Failed to get referenced GitRepoConfig", "gitRepoConfig", grcKey.String())
		result := ctrl.Result{}
		return &result, err
	}

	// Validate branch
	if result := r.validateBranch(ctx, dest, &grc, repoNS, log); result != nil {
		return result, nil
	}

	// All validations passed
	msg := fmt.Sprintf("GitDestination is ready. Repo='%s/%s', Branch='%s', BaseFolder='%s'",
		repoNS, dest.Spec.RepoRef.Name, dest.Spec.Branch, dest.Spec.BaseFolder)
	r.setCondition(dest, metav1.ConditionTrue, GitDestinationReasonReady, msg)
	dest.Status.ObservedGeneration = dest.Generation

	return nil, nil //nolint:nilnil // Valid case: no result needed, validation passed
}

// validateBranch validates that the branch is in the allowedBranches list.
func (r *GitDestinationReconciler) validateBranch(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
	grc *configbutleraiv1alpha1.GitRepoConfig,
	repoNS string,
	log logr.Logger,
) *ctrl.Result {
	branchAllowed := false
	for _, allowedBranch := range grc.Spec.AllowedBranches {
		if dest.Spec.Branch == allowedBranch {
			branchAllowed = true
			break
		}
	}

	if !branchAllowed {
		msg := fmt.Sprintf("Branch '%s' is not in allowedBranches list %v of GitRepoConfig '%s/%s'",
			dest.Spec.Branch, grc.Spec.AllowedBranches, repoNS, dest.Spec.RepoRef.Name)
		log.Info("Branch validation failed", "branch", dest.Spec.Branch, "allowedBranches", grc.Spec.AllowedBranches)
		r.setCondition(dest, metav1.ConditionFalse, GitDestinationReasonBranchNotAllowed, msg)
		result, _ := r.updateStatusAndRequeue(ctx, dest, RequeueShortInterval)
		return &result
	}

	return nil
}

// registerWithWorkerAndEventStream registers the GitDestination with worker and event stream.
func (r *GitDestinationReconciler) registerWithWorkerAndEventStream(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
	repoNS string,
	log logr.Logger,
) {
	// Register with branch worker
	r.registerWithWorker(ctx, dest, repoNS, log)

	// Register event stream
	r.registerEventStream(dest, repoNS, log)
}

// registerWithWorker registers the destination with branch worker.
func (r *GitDestinationReconciler) registerWithWorker(
	ctx context.Context,
	dest *configbutleraiv1alpha1.GitDestination,
	repoNS string,
	log logr.Logger,
) {
	if r.WorkerManager == nil {
		return
	}

	if err := r.WorkerManager.RegisterDestination(
		ctx,
		dest.Name, dest.Namespace,
		dest.Spec.RepoRef.Name, repoNS,
		dest.Spec.Branch,
		dest.Spec.BaseFolder,
	); err != nil {
		log.Error(err, "Failed to register destination with worker")
	} else {
		log.Info("Registered destination with branch worker",
			"repo", dest.Spec.RepoRef.Name,
			"branch", dest.Spec.Branch,
			"baseFolder", dest.Spec.BaseFolder)
	}
}

// registerEventStream registers the GitDestinationEventStream with EventRouter.
func (r *GitDestinationReconciler) registerEventStream(
	dest *configbutleraiv1alpha1.GitDestination,
	repoNS string,
	log logr.Logger,
) {
	if r.EventRouter == nil {
		return
	}

	branchWorker, exists := r.WorkerManager.GetWorkerForDestination(
		dest.Spec.RepoRef.Name, repoNS, dest.Spec.Branch,
	)
	if !exists {
		log.Error(nil, "BranchWorker not found for GitDestinationEventStream registration",
			"repo", dest.Spec.RepoRef.Name,
			"namespace", repoNS,
			"branch", dest.Spec.Branch)
		return
	}

	gitDest := types.NewResourceReference(dest.Name, dest.Namespace)
	stream := reconcile.NewGitDestinationEventStream(
		dest.Name, dest.Namespace,
		branchWorker,
		log,
	)
	r.EventRouter.RegisterGitDestinationEventStream(gitDest, stream)
	log.Info("Registered GitDestinationEventStream with EventRouter",
		"gitDest", gitDest.String(),
		"repo", dest.Spec.RepoRef.Name,
		"branch", dest.Spec.Branch,
		"baseFolder", dest.Spec.BaseFolder)
}

// Note: Worker registration is idempotent - calling RegisterDestination multiple times
// for the same destination is safe. Workers are cleaned up on pod restart.
// Since we have no active users, this simplified lifecycle approach is acceptable.

// setCondition sets or updates the Ready condition.
func (r *GitDestinationReconciler) setCondition(dest *configbutleraiv1alpha1.GitDestination,
	status metav1.ConditionStatus, reason, message string,
) {
	condition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range dest.Status.Conditions {
		if existingCondition.Type == ConditionTypeReady {
			dest.Status.Conditions[i] = condition
			return
		}
	}

	dest.Status.Conditions = append(dest.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *GitDestinationReconciler) updateStatusAndRequeue( //nolint:lll // Function signature
	ctx context.Context, dest *configbutleraiv1alpha1.GitDestination, requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, dest); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *GitDestinationReconciler) updateStatusWithRetry(
	ctx context.Context, dest *configbutleraiv1alpha1.GitDestination,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", dest.Name,
		"namespace", dest.Namespace,
		"conditionsCount", len(dest.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha1.GitDestination{}
		key := client.ObjectKeyFromObject(dest)
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
		latest.Status = dest.Status

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
func (r *GitDestinationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitDestination{}).
		Named("gitdestination").
		Complete(r)
}
