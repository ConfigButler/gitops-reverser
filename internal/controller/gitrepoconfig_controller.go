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
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// Status condition reasons
const (
	ReasonChecking         = "Checking"
	ReasonSecretNotFound   = "SecretNotFound"
	ReasonSecretMalformed  = "SecretMalformed"
	ReasonConnectionFailed = "ConnectionFailed"
	ReasonBranchNotFound   = "BranchNotFound"
	ReasonBranchFound      = "BranchFound"
)

// GitRepoConfigReconciler reconciles a GitRepoConfig object
type GitRepoConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitrepoconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list

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

	log.Info("Starting GitRepoConfig validation",
		"name", gitRepoConfig.Name,
		"namespace", gitRepoConfig.Namespace,
		"repoUrl", gitRepoConfig.Spec.RepoURL,
		"branch", gitRepoConfig.Spec.Branch,
		"generation", gitRepoConfig.Generation,
		"resourceVersion", gitRepoConfig.ResourceVersion)

	// Set initial checking status
	log.Info("Setting initial checking status")
	r.setCondition(&gitRepoConfig, metav1.ConditionUnknown, ReasonChecking, "Validating repository connectivity...")

	// Step 1: Fetch and validate secret
	var secret *corev1.Secret
	var err error

	if gitRepoConfig.Spec.SecretRef != nil {
		log.Info("Fetching secret for authentication",
			"secretName", gitRepoConfig.Spec.SecretRef.Name,
			"namespace", gitRepoConfig.Namespace)
		secret, err = r.fetchSecret(ctx, gitRepoConfig.Spec.SecretRef.Name, gitRepoConfig.Namespace)
		if err != nil {
			log.Error(err, "Failed to fetch secret",
				"secretName", gitRepoConfig.Spec.SecretRef.Name,
				"namespace", gitRepoConfig.Namespace)
			r.setCondition(&gitRepoConfig, metav1.ConditionFalse, ReasonSecretNotFound,
				fmt.Sprintf("Secret '%s' not found in namespace '%s': %v", gitRepoConfig.Spec.SecretRef.Name, gitRepoConfig.Namespace, err))
			return r.updateStatusAndRequeue(ctx, &gitRepoConfig, time.Minute*5)
		}
		log.Info("Successfully fetched secret", "secretName", gitRepoConfig.Spec.SecretRef.Name)
	} else {
		log.Info("No secret specified, using anonymous access")
	}

	// Step 2: Extract credentials from secret
	log.Info("Extracting credentials from secret")
	auth, err := r.extractCredentials(secret)
	if err != nil {
		log.Error(err, "Failed to extract credentials from secret")
		var secretName string
		if gitRepoConfig.Spec.SecretRef != nil {
			secretName = gitRepoConfig.Spec.SecretRef.Name
		} else {
			secretName = "<none>"
		}
		r.setCondition(&gitRepoConfig, metav1.ConditionFalse, ReasonSecretMalformed,
			fmt.Sprintf("Secret '%s' malformed: %v", secretName, err))
		return r.updateStatusAndRequeue(ctx, &gitRepoConfig, time.Minute*5)
	}
	log.Info("Successfully extracted credentials", "hasAuth", auth != nil)

	// Step 3: Validate repository connectivity and branch
	log.Info("Validating repository connectivity and branch",
		"repoUrl", gitRepoConfig.Spec.RepoURL,
		"branch", gitRepoConfig.Spec.Branch)
	commitHash, err := r.validateRepository(ctx, gitRepoConfig.Spec.RepoURL, gitRepoConfig.Spec.Branch, auth)
	if err != nil {
		log.Error(err, "Repository validation failed",
			"repoUrl", gitRepoConfig.Spec.RepoURL,
			"branch", gitRepoConfig.Spec.Branch)
		if strings.Contains(err.Error(), "branch") {
			r.setCondition(&gitRepoConfig, metav1.ConditionFalse, ReasonBranchNotFound,
				fmt.Sprintf("Branch '%s' does not exist in repository: %v", gitRepoConfig.Spec.Branch, err))
		} else {
			r.setCondition(&gitRepoConfig, metav1.ConditionFalse, ReasonConnectionFailed,
				fmt.Sprintf("Failed to connect to repository: %v", err))
		}
		return r.updateStatusAndRequeue(ctx, &gitRepoConfig, time.Minute*2)
	}

	// Step 4: Success - set ready condition
	log.Info("Repository validation successful",
		"commitHash", commitHash,
		"shortCommit", commitHash[:8])
	message := fmt.Sprintf("Branch '%s' found and accessible at commit %s", gitRepoConfig.Spec.Branch, commitHash[:8])
	r.setCondition(&gitRepoConfig, metav1.ConditionTrue, ReasonBranchFound, message)

	log.Info("GitRepoConfig validation successful", "name", gitRepoConfig.Name, "commit", commitHash[:8])

	// Update status and schedule periodic revalidation
	log.Info("Updating status with success condition")
	if err := r.updateStatusWithRetry(ctx, &gitRepoConfig); err != nil {
		log.Error(err, "Failed to update GitRepoConfig status")
		return ctrl.Result{}, err
	}

	log.Info("Status update completed successfully, scheduling requeue",
		"requeueAfter", time.Minute*10)
	// Revalidate every 10 minutes
	return ctrl.Result{RequeueAfter: time.Minute * 10}, nil
}

// fetchSecret retrieves the secret containing Git credentials
func (r *GitRepoConfigReconciler) fetchSecret(ctx context.Context, secretName, secretNamespace string) (*corev1.Secret, error) {
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

// extractCredentials extracts Git authentication from secret data
func (r *GitRepoConfigReconciler) extractCredentials(secret *corev1.Secret) (transport.AuthMethod, error) {
	// If no secret is provided, return nil auth (for public repositories)
	if secret == nil {
		return nil, nil
	}

	// Try SSH key authentication first
	if privateKey, exists := secret.Data["ssh-privatekey"]; exists {
		passphrase := ""
		if passphraseData, hasPassphrase := secret.Data["ssh-passphrase"]; hasPassphrase {
			passphrase = string(passphraseData)
		}

		// Parse private key with potential passphrase
		var signer gossh.Signer
		var err error

		if passphrase != "" {
			signer, err = gossh.ParsePrivateKeyWithPassphrase(privateKey, []byte(passphrase))
		} else {
			signer, err = gossh.ParsePrivateKey(privateKey)
		}

		if err != nil {
			return nil, fmt.Errorf("failed to parse SSH private key: %w", err)
		}

		publicKeys := &ssh.PublicKeys{
			User:   "git",
			Signer: signer,
		}

		// Handle known_hosts if present
		if knownHostsData, hasKnownHosts := secret.Data["known_hosts"]; hasKnownHosts {
			// Write known_hosts to temporary file for parsing
			knownHostsFile := fmt.Sprintf("/tmp/known_hosts-%d", time.Now().Unix())
			if err := os.WriteFile(knownHostsFile, knownHostsData, 0600); err != nil {
				fmt.Printf("Warning: Failed to write known_hosts file, using insecure SSH: %v\n", err)
				publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
			} else {
				hostKeyCallback, err := knownhosts.New(knownHostsFile)
				if err != nil {
					fmt.Printf("Warning: Failed to parse known_hosts, using insecure SSH: %v\n", err)
					publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
				} else {
					publicKeys.HostKeyCallback = hostKeyCallback
				}
				// Cleanup the temporary known_hosts file
				if err := os.RemoveAll(knownHostsFile); err != nil {
					fmt.Printf("Warning: Failed to cleanup known_hosts file: %v\n", err)
				}
			}
		} else {
			// No known_hosts provided, use insecure mode for testing
			fmt.Printf("Warning: No known_hosts found in secret, using insecure SSH\n")
			publicKeys.HostKeyCallback = gossh.InsecureIgnoreHostKey()
		}

		return publicKeys, nil
	}

	// Try username/password authentication
	if username, hasUser := secret.Data["username"]; hasUser {
		if password, hasPass := secret.Data["password"]; hasPass {
			return &http.BasicAuth{
				Username: string(username),
				Password: string(password),
			}, nil
		}
		return nil, fmt.Errorf("secret contains username but missing password")
	}

	return nil, fmt.Errorf("secret must contain either 'ssh-privatekey' or both 'username' and 'password'")
}

// validateRepository checks if the repository is accessible and branch exists
func (r *GitRepoConfigReconciler) validateRepository(ctx context.Context, repoURL, branch string, auth transport.AuthMethod) (string, error) {
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
	log.Info("Starting git clone", "options", fmt.Sprintf("URL=%s, Branch=%s, SingleBranch=true, Depth=1", repoURL, branch))
	_, err := git.PlainClone(tempDir, false, &git.CloneOptions{
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
	repo, err := git.PlainOpen(tempDir)
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

// setCondition sets or updates the Ready condition
func (r *GitRepoConfigReconciler) setCondition(gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig, status metav1.ConditionStatus, reason, message string) {
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
			// Log condition update
			fmt.Printf("Updated existing Ready condition: status=%s, reason=%s, message=%s\n",
				status, reason, message)
			return
		}
	}

	gitRepoConfig.Status.Conditions = append(gitRepoConfig.Status.Conditions, condition)
	// Log condition addition
	fmt.Printf("Added new Ready condition: status=%s, reason=%s, message=%s\n",
		status, reason, message)
}

// updateStatusAndRequeue updates the status and returns requeue result
func (r *GitRepoConfigReconciler) updateStatusAndRequeue(ctx context.Context, gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig, requeueAfter time.Duration) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, gitRepoConfig); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions
func (r *GitRepoConfigReconciler) updateStatusWithRetry(ctx context.Context, gitRepoConfig *configbutleraiv1alpha1.GitRepoConfig) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.Info("Starting status update with retry",
		"name", gitRepoConfig.Name,
		"namespace", gitRepoConfig.Namespace,
		"conditionsCount", len(gitRepoConfig.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
	}, func() (bool, error) {
		log.Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha1.GitRepoConfig{}
		key := client.ObjectKeyFromObject(gitRepoConfig)
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
		latest.Status = gitRepoConfig.Status

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
func (r *GitRepoConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&configbutleraiv1alpha1.GitRepoConfig{}).
		Named("gitrepoconfig").
		Complete(r)
}
