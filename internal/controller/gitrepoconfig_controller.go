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

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
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
)

// Connection secret status values.
const (
	ConnectionSecretValid   = "Valid"
	ConnectionSecretInvalid = "Invalid"
	ConnectionSecretMissing = "Missing"
	ConnectionSecretNotSet  = "NotSet"
)

// Connection check status values.
const (
	ConnectionCheckSuccessful = "Successful"
	ConnectionCheckFailed     = "Failed"
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
	// Initialize status fields
	gitRepoConfig.Status.ConnectionSecret = ""
	gitRepoConfig.Status.ConnectionCheck = ""
	gitRepoConfig.Status.RemoteBranchCount = 0

	// Fetch and validate secret
	secret, shouldReturn := r.fetchAndValidateSecret(ctx, log, gitRepoConfig)
	if shouldReturn {
		result, _ := r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueMediumInterval)
		return result, nil
	}

	// Extract credentials
	auth, result, shouldReturn := r.getAuthFromSecret(ctx, log, gitRepoConfig, secret)
	if shouldReturn {
		return result, nil
	}

	// Validate repository connectivity
	return r.validateAndUpdateStatus(ctx, log, gitRepoConfig, auth)
}

// fetchAndValidateSecret fetches the secret if specified.
// Returns (secret, shouldReturn). If shouldReturn is true, caller should return immediately.
func (r *GitRepoConfigReconciler) fetchAndValidateSecret(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
) (*corev1.Secret, bool) {
	if gitRepoConfig.Spec.SecretRef == nil {
		log.Info("No secret specified, using anonymous access")
		return nil, false
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
		gitRepoConfig.Status.ConnectionSecret = ConnectionSecretMissing
		return nil, true
	}

	log.Info("Successfully fetched secret", "secretName", gitRepoConfig.Spec.SecretRef.Name)
	return secret, false
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
		gitRepoConfig.Status.ConnectionSecret = ConnectionSecretInvalid
		result, _ := r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueMediumInterval)
		return nil, result, true
	}

	log.Info("Successfully extracted credentials", "hasAuth", auth != nil)
	return auth, ctrl.Result{}, false
}

// validateAndUpdateStatus validates repository connectivity and updates the status.
func (r *GitRepoConfigReconciler) validateAndUpdateStatus(
	ctx context.Context,
	log logr.Logger,
	gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig,
	auth transport.AuthMethod,
) (ctrl.Result, error) {
	log.Info("Validating repository connectivity",
		"repoUrl", gitRepoConfig.Spec.RepoURL)

	// Set connection secret status
	if auth != nil {
		gitRepoConfig.Status.ConnectionSecret = ConnectionSecretValid
	} else {
		gitRepoConfig.Status.ConnectionSecret = ConnectionSecretNotSet
	}

	// Check repository connectivity and get branch count
	branchCount, err := r.checkRemoteConnectivity(ctx, gitRepoConfig.Spec.RepoURL, auth)
	if err != nil {
		log.Error(err, "Repository connectivity check failed",
			"repoUrl", gitRepoConfig.Spec.RepoURL)
		r.setCondition(gitRepoConfig, metav1.ConditionFalse, ReasonConnectionFailed,
			fmt.Sprintf("Failed to connect to repository: %v", err))
		gitRepoConfig.Status.ConnectionCheck = ConnectionCheckFailed
		gitRepoConfig.Status.RemoteBranchCount = 0
		return r.updateStatusAndRequeue(ctx, gitRepoConfig, RequeueShortInterval)
	}

	log.Info("Repository connectivity validated successfully", "branchCount", branchCount)
	message := fmt.Sprintf("Repository connectivity validated for %s", gitRepoConfig.Spec.RepoURL)
	r.setCondition(gitRepoConfig, metav1.ConditionTrue, "Ready", message)
	gitRepoConfig.Status.ConnectionCheck = ConnectionCheckSuccessful
	gitRepoConfig.Status.RemoteBranchCount = branchCount

	// Update ObservedGeneration to mark this generation as validated
	gitRepoConfig.Status.ObservedGeneration = gitRepoConfig.Generation

	log.Info("GitRepoConfig validation successful", "name", gitRepoConfig.Name)
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

// checkRemoteConnectivity performs a lightweight check of repository connectivity and returns branch count.
func (r *GitRepoConfigReconciler) checkRemoteConnectivity(
	ctx context.Context, repoURL string, auth transport.AuthMethod,
) (int, error) {
	log := logf.FromContext(ctx).WithName("checkRemoteConnectivity")

	log.Info("Checking remote repository connectivity", "repoURL", repoURL)

	// Create a new "dummy" remote object in memory for lightweight checking
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	// Call .List() on the remote - this is the lightweight check
	refs, err := remote.List(&git.ListOptions{
		Auth: auth,
	})

	if err != nil {
		// Check if this is an empty repository (which is valid for GitOps Reverser to bootstrap)
		errStr := err.Error()
		if strings.Contains(errStr, "empty repository") ||
			strings.Contains(errStr, "repository is empty") ||
			strings.Contains(errStr, "no commits") ||
			strings.Contains(errStr, "does not have any commits yet") {
			log.Info(
				"Repository is empty but accessible - this is valid for GitOps Reverser bootstrap",
				"repoURL",
				repoURL,
			)
			return 0, nil
		}

		log.Error(err, "Remote connectivity check failed", "repoURL", repoURL)
		return 0, fmt.Errorf("failed to connect to repository: %w", err)
	}

	// Count branches (refs that look like branches)
	branchCount := 0
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name().String(), "refs/heads/") {
			branchCount++
		}
	}

	log.Info("Remote connectivity check successful", "repoURL", repoURL, "branchCount", branchCount)
	return branchCount, nil
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
