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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
)

// GitProviderReconciler reconciles a GitProvider object.
type GitProviderReconciler struct {
	client.Client

	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitProviderReconciler")
	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the GitProvider instance
	var gitProvider configbutleraiv1alpha1.GitProvider
	if err := r.Get(ctx, req.NamespacedName, &gitProvider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("GitProvider not found, was likely deleted", "namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch GitProvider", "namespacedName", req.NamespacedName)
		return ctrl.Result{}, err
	}

	return r.reconcileGitProvider(ctx, log, &gitProvider)
}

// reconcileGitProvider performs the main reconciliation logic.
func (r *GitProviderReconciler) reconcileGitProvider(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha1.GitProvider,
) (ctrl.Result, error) {
	log.Info("Starting GitProvider validation",
		"name", gitProvider.Name,
		"namespace", gitProvider.Namespace,
		"url", gitProvider.Spec.URL,
		"allowedBranches", gitProvider.Spec.AllowedBranches,
		"generation", gitProvider.Generation,
		"resourceVersion", gitProvider.ResourceVersion)

	r.setCondition(gitProvider, metav1.ConditionUnknown, ReasonChecking, "Validating repository connectivity...")

	// Fetch and validate secret
	secret, shouldReturn := r.fetchAndValidateSecret(ctx, log, gitProvider)
	if shouldReturn {
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider, RequeueMediumInterval)
		return result, nil
	}

	// Extract credentials
	auth, result, shouldReturn := r.getAuthFromSecret(ctx, log, gitProvider, secret)
	if shouldReturn {
		return result, nil
	}

	// Validate repository connectivity
	return r.validateAndUpdateStatus(ctx, log, gitProvider, auth)
}

// fetchAndValidateSecret fetches the secret if specified.
// Returns (secret, shouldReturn). If shouldReturn is true, caller should return immediately.
func (r *GitProviderReconciler) fetchAndValidateSecret(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha1.GitProvider,
) (*corev1.Secret, bool) {
	if gitProvider.Spec.SecretRef == nil {
		log.Info("No secret specified, using anonymous access")
		return nil, false
	}

	log.Info("Fetching secret for authentication",
		"secretName", gitProvider.Spec.SecretRef.Name,
		"namespace", gitProvider.Namespace)

	secret, err := r.fetchSecret(ctx, gitProvider.Spec.SecretRef.Name, gitProvider.Namespace)
	if err != nil {
		log.Error(err, "Failed to fetch secret",
			"secretName", gitProvider.Spec.SecretRef.Name,
			"namespace", gitProvider.Namespace)
		r.setCondition(
			gitProvider,
			metav1.ConditionFalse,
			ReasonSecretNotFound,
			fmt.Sprintf(
				"Secret '%s' not found in namespace '%s': %v",
				gitProvider.Spec.SecretRef.Name,
				gitProvider.Namespace,
				err,
			),
		)
		return nil, true
	}

	log.Info("Successfully fetched secret", "secretName", gitProvider.Spec.SecretRef.Name)
	return secret, false
}

// getAuthFromSecret extracts authentication from the secret.
// Returns (auth, result, shouldReturn). If shouldReturn is true, caller should return the result immediately.
func (r *GitProviderReconciler) getAuthFromSecret(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha1.GitProvider,
	secret *corev1.Secret,
) (transport.AuthMethod, ctrl.Result, bool) {
	log.Info("Extracting credentials from secret")
	auth, err := r.extractCredentials(secret)
	if err != nil {
		log.Error(err, "Failed to extract credentials from secret")
		secretName := gitProvider.Spec.SecretRef.Name
		r.setCondition(gitProvider, metav1.ConditionFalse, ReasonSecretMalformed,
			fmt.Sprintf("Secret '%s' malformed: %v", secretName, err))
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider, RequeueMediumInterval)
		return nil, result, true
	}

	log.Info("Successfully extracted credentials", "hasAuth", auth != nil)
	return auth, ctrl.Result{}, false
}

// validateAndUpdateStatus validates repository connectivity and updates the status.
func (r *GitProviderReconciler) validateAndUpdateStatus(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha1.GitProvider,
	auth transport.AuthMethod,
) (ctrl.Result, error) {
	log.Info("Validating repository connectivity",
		"url", gitProvider.Spec.URL)

	// Check repository connectivity and get branch count
	branchCount, err := r.checkRemoteConnectivity(ctx, gitProvider.Spec.URL, auth)
	if err != nil {
		log.Error(err, "Repository connectivity check failed",
			"url", gitProvider.Spec.URL)
		r.setCondition(gitProvider, metav1.ConditionFalse, ReasonConnectionFailed,
			fmt.Sprintf("Failed to connect to repository: %v", err))
		return r.updateStatusAndRequeue(ctx, gitProvider, RequeueShortInterval)
	}

	log.Info("Repository connectivity validated successfully", "branchCount", branchCount)
	message := fmt.Sprintf("Repository connectivity validated for %s", gitProvider.Spec.URL)
	r.setCondition(gitProvider, metav1.ConditionTrue, "Ready", message)

	log.Info("GitProvider validation successful", "name", gitProvider.Name)
	log.Info("Updating status with success condition")

	if err := r.updateStatusWithRetry(ctx, gitProvider); err != nil {
		log.Error(err, "Failed to update GitProvider status")
		return ctrl.Result{}, err
	}

	log.Info("Status update completed successfully, scheduling requeue", "requeueAfter", RequeueLongInterval)
	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

// fetchSecret retrieves the secret containing Git credentials.
func (r *GitProviderReconciler) fetchSecret(
	ctx context.Context, secretName, secretNamespace string) (*corev1.Secret, error) {
	var secret corev1.Secret
	secretKey := types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}

	if err := r.Get(ctx, secretKey, &secret); err != nil {
		return nil, err
	}

	return &secret, nil
}

// extractCredentials extracts Git authentication from secret data.
func (r *GitProviderReconciler) extractCredentials(secret *corev1.Secret) (transport.AuthMethod, error) {
	// If no secret is provided, return nil auth (for public repositories)
	if secret == nil {
		return nil, nil //nolint:nilnil // nil auth means public repository
	}

	// Try SSH key authentication first
	if privateKey, exists := secret.Data["ssh-privatekey"]; exists {
		keyPassword := ""
		if passData, hasPass := secret.Data["ssh-password"]; hasPass {
			keyPassword = string(passData)
		}
		// Get known_hosts if available
		knownHosts := ""
		if knownHostsData, hasKnownHosts := secret.Data["known_hosts"]; hasKnownHosts {
			knownHosts = string(knownHostsData)
		}
		return ssh.GetAuthMethod(string(privateKey), keyPassword, knownHosts)
	}

	// Try username/password authentication
	if username, hasUser := secret.Data["username"]; hasUser {
		if password, hasPass := secret.Data["password"]; hasPass {
			return &http.BasicAuth{
				Username: string(username),
				Password: string(password),
			}, nil
		}
		return nil, ErrMissingPassword
	}

	return nil, ErrInvalidSecretFormat
}

// checkRemoteConnectivity performs a lightweight check of repository connectivity and returns branch count.
func (r *GitProviderReconciler) checkRemoteConnectivity(
	ctx context.Context, repoURL string, auth transport.AuthMethod,
) (int, error) {
	log := logf.FromContext(ctx).WithName("checkRemoteConnectivity")

	log.Info("Checking remote repository connectivity", "repoURL", repoURL)

	// Use new CheckRepo abstraction from git package
	repoInfo, err := gitpkg.CheckRepo(ctx, repoURL, auth)
	if err != nil {
		log.Error(err, "Remote connectivity check failed", "repoURL", repoURL)
		return 0, fmt.Errorf("failed to connect to repository: %w", err)
	}

	log.Info("Remote connectivity check successful", "repoURL", repoURL, "branchCount", repoInfo.RemoteBranchCount)
	return repoInfo.RemoteBranchCount, nil
}

// setCondition sets or updates the Ready condition.
func (r *GitProviderReconciler) setCondition(
	gitProvider *configbutleraiv1alpha1.GitProvider, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range gitProvider.Status.Conditions {
		if existingCondition.Type == ConditionTypeReady {
			gitProvider.Status.Conditions[i] = condition
			return
		}
	}

	gitProvider.Status.Conditions = append(gitProvider.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *GitProviderReconciler) updateStatusAndRequeue(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha1.GitProvider,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, gitProvider); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *GitProviderReconciler) updateStatusWithRetry(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha1.GitProvider,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", gitProvider.Name,
		"namespace", gitProvider.Namespace,
		"conditionsCount", len(gitProvider.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha1.GitProvider{}
		key := client.ObjectKeyFromObject(gitProvider)
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
		latest.Status = gitProvider.Status

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
func (r *GitProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitProvider{}).
		Named("gitprovider").
		Complete(r)
}
