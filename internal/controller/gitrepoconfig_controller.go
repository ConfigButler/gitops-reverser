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
	"errors"
	"fmt"
	"os"
	"strings"
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

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/ssh"
)

// Status condition reasons.
const (
	ReasonChecking         = "Checking"
	ReasonSecretNotFound   = "SecretNotFound"
	ReasonSecretMalformed  = "SecretMalformed"
	ReasonConnectionFailed = "ConnectionFailed"
	ReasonBranchNotFound   = "BranchNotFound"
	ReasonBranchFound      = "BranchFound"
)

// Sentinel errors for credential extraction.
var (
	ErrInvalidSecretFormat = errors.New("secret must contain either 'ssh-privatekey' or both 'username' and 'password'")
	ErrMissingPassword     = errors.New("secret contains username but missing password")
)

// GitRepoConfigReconciler reconciles a GitRepoConfig object.
type GitRepoConfigReconciler struct {
	client.Client

	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitRepoConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitRepoConfigReconciler")
	log.Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the GitRepoConfig instance
	var gitRepoConfig configbutleraiv1alpha1.GitRepoConfig
	if err := r.Get(ctx, req.NamespacedName, &gitRepoConfig); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("GitRepoConfig not found, was likely deleted", "namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch GitRepoConfig", "namespacedName", req.NamespacedName)
		return ctrl.Result{}, err
	}

	return r.reconcileGitRepoConfig(ctx, log, &gitRepoConfig)
}

// reconcileGitRepoConfig performs the main reconciliation logic.
func (r *GitRepoConfigReconciler) reconcileGitRepoConfig(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
) (ctrl.Result, error) {
	log.Info("Starting GitRepoConfig validation",
		"name", gitRepoConfig.Name,
		"namespace", gitRepoConfig.Namespace,
		"repoUrl", gitRepoConfig.Spec.RepoURL,
		"allowedBranches", gitRepoConfig.Spec.AllowedBranches,
		"generation", gitRepoConfig.Generation,
		"observedGeneration", gitRepoConfig.Status.ObservedGeneration,
		"resourceVersion", gitRepoConfig.ResourceVersion)

	// Skip validation if we've already validated this generation
	if gitRepoConfig.Status.ObservedGeneration == gitRepoConfig.Generation {
		log.Info("Skipping validation - already validated this generation",
			"generation", gitRepoConfig.Generation)
		return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
	}

	r.setCondition(gitRepoConfig, metav1.ConditionUnknown, ReasonChecking, "Validating repository connectivity...")

	// Fetch and validate secret
	secret, result, shouldReturn := r.fetchAndValidateSecret(ctx, log, gitRepoConfig)
	if shouldReturn {
		return result, nil
	}

	// Extract credentials
	auth, result, shouldReturn := r.getAuthFromSecret(ctx, log, gitRepoConfig, secret)
	if shouldReturn {
		return result, nil
	}

	// Validate repository
	return r.validateAndUpdateStatus(ctx, log, gitRepoConfig, auth)
}

// fetchAndValidateSecret fetches the secret if specified.
// Returns (secret, result, shouldReturn). If shouldReturn is true, caller should return the result immediately.
func (r *GitRepoConfigReconciler) fetchAndValidateSecret(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
) (*corev1.Secret, ctrl.Result, bool) {
	if gitRepoConfig.Spec.SecretRef == nil {
		log.Info("No secret specified, using anonymous access")
		return nil, ctrl.Result{}, false
	}

	log.Info("Fetching secret for authentication",
		"secretName", gitRepoConfig.Spec.SecretRef.Name,
		"namespace", gitRepoConfig.Namespace)

	secret, err := r.fetchSecret(ctx, gitRepoConfig.Spec.SecretRef.Name, gitRepoConfig.Namespace)
	if err != nil {
		log.Error(err, "Failed to fetch secret",
			"secretName", gitRepoConfig.Spec.SecretRef.Name,
			"namespace", gitRepoConfig.Namespace)
		r.setCondition(
			gitRepoConfig,
			metav1.ConditionFalse,
			ReasonSecretNotFound, //nolint:lll // Error message
			fmt.Sprintf(
				"Secret '%s' not found in namespace '%s': %v",
				gitRepoConfig.Spec.SecretRef.Name,
				gitRepoConfig.Namespace,
				err,
			),
		)
		result, _ := r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueMediumInterval)
		return nil, result, true
	}

	log.Info("Successfully fetched secret", "secretName", gitRepoConfig.Spec.SecretRef.Name)
	return secret, ctrl.Result{}, false
}

// getAuthFromSecret extracts authentication from the secret.
// Returns (auth, result, shouldReturn). If shouldReturn is true, caller should return the result immediately.
func (r *GitRepoConfigReconciler) getAuthFromSecret(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
	secret *corev1.Secret,
) (transport.AuthMethod, ctrl.Result, bool) {
	log.Info("Extracting credentials from secret")
	auth, err := r.extractCredentials(secret)
	if err != nil {
		log.Error(err, "Failed to extract credentials from secret")
		secretName := "<none>"
		if gitRepoConfig.Spec.SecretRef != nil {
			secretName = gitRepoConfig.Spec.SecretRef.Name
		}
		r.setCondition(gitRepoConfig, metav1.ConditionFalse, ReasonSecretMalformed,
			fmt.Sprintf("Secret '%s' malformed: %v", secretName, err))
		result, _ := r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueMediumInterval)
		return nil, result, true
	}

	log.Info("Successfully extracted credentials", "hasAuth", auth != nil)
	return auth, ctrl.Result{}, false
}

// validateAndUpdateStatus validates the repository and updates the status.
func (r *GitRepoConfigReconciler) validateAndUpdateStatus(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
	auth transport.AuthMethod,
) (ctrl.Result, error) {
	log.Info("Validating repository connectivity and branches",
		"repoUrl", gitRepoConfig.Spec.RepoURL,
		"allowedBranches", gitRepoConfig.Spec.AllowedBranches)

	// Validate all allowed branches
	validatedBranches := make(map[string]string) // branch -> commit hash
	for _, branch := range gitRepoConfig.Spec.AllowedBranches {
		commitHash, err := r.validateRepository(ctx, gitRepoConfig.Spec.RepoURL, branch, auth)
		if err != nil {
			log.Error(err, "Repository validation failed",
				"repoUrl", gitRepoConfig.Spec.RepoURL,
				"branch", branch)
			if strings.Contains(err.Error(), "branch") {
				r.setCondition(gitRepoConfig, metav1.ConditionFalse, ReasonBranchNotFound,
					fmt.Sprintf("Branch '%s' does not exist in repository: %v", branch, err))
			} else {
				r.setCondition(gitRepoConfig, metav1.ConditionFalse, ReasonConnectionFailed,
					fmt.Sprintf("Failed to connect to repository: %v", err))
			}
			return r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueShortInterval)
		}
		validatedBranches[branch] = commitHash
		log.Info("Branch validated successfully", "branch", branch, "commitHash", commitHash[:8])
	}

	log.Info("All branches validated successfully", "branchCount", len(validatedBranches))
	var branchSummaries []string
	for branch, hash := range validatedBranches {
		branchSummaries = append(branchSummaries, fmt.Sprintf("%s@%s", branch, hash[:8]))
	}
	message := fmt.Sprintf("All %d branches validated: %s", len(validatedBranches), strings.Join(branchSummaries, ", "))
	r.setCondition(gitRepoConfig, metav1.ConditionTrue, ReasonBranchFound, message)

	// Update ObservedGeneration to mark this generation as validated
	gitRepoConfig.Status.ObservedGeneration = gitRepoConfig.Generation

	log.Info("GitRepoConfig validation successful", "name", gitRepoConfig.Name, "branchCount", len(validatedBranches))
	log.Info("Updating status with success condition")

	if err := r.updateStatusWithRetry(ctx, gitRepoConfig); err != nil {
		log.Error(err, "Failed to update GitRepoConfig status")
		return ctrl.Result{}, err
	}

	log.Info("Status update completed successfully, scheduling requeue", "requeueAfter", RequeueLongInterval)
	return ctrl.Result{RequeueAfter: RequeueLongInterval}, nil
}

// fetchSecret retrieves the secret containing Git credentials.
func (r *GitRepoConfigReconciler) fetchSecret( //nolint:lll // Function signature
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
func (r *GitRepoConfigReconciler) extractCredentials(secret *corev1.Secret) (transport.AuthMethod, error) {
	// If no secret is provided, return nil auth (for public repositories)
	if secret == nil {
		return nil, nil //nolint:nilnil // Returning nil auth for public repos is semantically correct
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

// validateRepository checks if the repository is accessible and branch exists.
func (r *GitRepoConfigReconciler) validateRepository( //nolint:lll // Function signature
	ctx context.Context, repoURL, branch string, auth transport.AuthMethod) (string, error) {
	log := logf.FromContext(ctx).WithName("validateRepository")

	log.Info("Starting repository validation",
		"repoURL", repoURL,
		"branch", branch,
		"hasAuth", auth != nil)

	// Create a temporary directory for a minimal clone test
	tempDir := fmt.Sprintf("/tmp/git-validation-%d", time.Now().Unix())
	log.Info("Created temporary directory", "tempDir", tempDir)

	defer func() {
		// Clean up temp directory
		log.Info("Cleaning up temporary directory", "tempDir", tempDir)
		_ = os.RemoveAll(tempDir) // Ignore cleanup errors
	}()

	// Try to clone just the specified branch with depth 1 for validation
	log.Info("Starting git clone", //nolint:lll // Structured log with many fields
		"options", fmt.Sprintf("URL=%s, Branch=%s, SingleBranch=true, Depth=1", repoURL, branch))
	_, err := gogit.PlainClone(tempDir, false, &gogit.CloneOptions{
		URL:           repoURL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
		NoCheckout:    true, // Don't checkout files, just verify connectivity
	})

	if err != nil {
		log.Error(err, "Git clone failed", "repoURL", repoURL, "branch", branch)
		if strings.Contains(err.Error(), "couldn't find remote ref") ||
			strings.Contains(err.Error(), "reference not found") {
			return "", fmt.Errorf("branch '%s' not found in repository", branch)
		}
		return "", fmt.Errorf("failed to access repository: %w", err)
	}

	log.Info("Git clone successful, opening repository to get commit hash")

	// If we got here, the repository and branch are accessible
	// Open the temporary repo to get the commit hash
	repo, err := gogit.PlainOpen(tempDir)
	if err != nil {
		log.Error(err, "Failed to open cloned repository", "tempDir", tempDir)
		return "", fmt.Errorf("failed to open cloned repository: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		log.Error(err, "Failed to get HEAD reference")
		return "", fmt.Errorf("failed to get HEAD reference: %w", err)
	}

	commitHash := head.Hash().String()
	log.Info("Repository validation completed successfully", "commitHash", commitHash)
	return commitHash, nil
}

// setCondition sets or updates the Ready condition.
func (r *GitRepoConfigReconciler) setCondition( //nolint:lll // Function signature
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Update existing condition or add new one
	for i, existingCondition := range gitRepoConfig.Status.Conditions {
		if existingCondition.Type == ConditionTypeReady {
			gitRepoConfig.Status.Conditions[i] = condition
			return
		}
	}

	gitRepoConfig.Status.Conditions = append(gitRepoConfig.Status.Conditions, condition)
}

// updateStatusAndRequeue updates the status and returns requeue result.
func (r *GitRepoConfigReconciler) updateStatusAndRequeue( //nolint:lll // Function signature
	ctx context.Context,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, gitRepoConfig); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions
//
//nolint:dupl // Similar retry logic pattern used across controllers
func (r *GitRepoConfigReconciler) updateStatusWithRetry(
	ctx context.Context,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", gitRepoConfig.Name,
		"namespace", gitRepoConfig.Namespace,
		"conditionsCount", len(gitRepoConfig.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha1.GitRepoConfig{}
		key := client.ObjectKeyFromObject(gitRepoConfig)
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
		latest.Status = gitRepoConfig.Status

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
func (r *GitRepoConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitRepoConfig{}).
		Named("gitrepoconfig").
		Complete(r)
}
