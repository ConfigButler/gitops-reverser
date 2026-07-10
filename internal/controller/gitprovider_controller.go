// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
)

// GitProviderReconciler reconciles a GitProvider object.
type GitProviderReconciler struct {
	client.Client

	Scheme *runtime.Scheme
	firsts gitProviderLogFirsts

	// SSHHostKeys configures SSH host-key resolution (install-level default ConfigMap and the
	// dev-only missing-key opt-out) for the connectivity check's credential read, so it matches
	// what the write path uses.
	SSHHostKeys gitpkg.SSHHostKeyConfig
}

// gitProviderLogFirsts keeps startup progress visible without turning every
// routine connectivity recheck into default-level log noise. Each sync.Once
// fires a single Info line on the first occurrence across all GitProviders, so
// an operator sees how far setup progressed even when a later step gets stuck.
type gitProviderLogFirsts struct {
	anonymousAccess   sync.Once
	credentialsLoaded sync.Once
	validationSuccess sync.Once
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitProviderReconciler")
	log.V(1).Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	// Fetch the GitProvider instance
	var gitProvider configbutleraiv1alpha3.GitProvider
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
	gitProvider *configbutleraiv1alpha3.GitProvider,
) (ctrl.Result, error) {
	log.V(1).Info("Starting GitProvider validation",
		"name", gitProvider.Name,
		"namespace", gitProvider.Namespace,
		"url", gitProvider.Spec.URL,
		"allowedBranches", gitProvider.Spec.AllowedBranches,
		"generation", gitProvider.Generation,
		"resourceVersion", gitProvider.ResourceVersion)

	r.setProgressingConditions(gitProvider, ReasonChecking, "Validating repository connectivity...")
	if err := r.validateCommitConfiguration(gitProvider); err != nil {
		r.setStalledConditions(gitProvider, ReasonCommitConfigInvalid, err.Error())
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider)
		return result, nil
	}

	if err := r.ensureSigningKey(ctx, gitProvider); err != nil {
		reason := ReasonSecretMalformed
		if strings.Contains(err.Error(), "secretRef.name") {
			reason = ReasonCommitConfigInvalid
		}
		if strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "generateWhenMissing is disabled") {
			reason = ReasonSecretNotFound
		}

		r.setStalledConditions(gitProvider, reason, err.Error())
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider)
		return result, nil
	}

	// Fetch and validate secret
	secret, shouldReturn := r.fetchAndValidateSecret(ctx, log, gitProvider)
	if shouldReturn {
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider)
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
	gitProvider *configbutleraiv1alpha3.GitProvider,
) (*corev1.Secret, bool) {
	if gitProvider.Spec.SecretRef == nil {
		log.V(1).Info("No secret specified, using anonymous access")
		r.firsts.anonymousAccess.Do(func() {
			log.Info("GitProvider configured for anonymous access (no secretRef) - "+
				"only public repositories will be reachable; set spec.secretRef to use a private repository",
				"name", gitProvider.Name,
				"namespace", gitProvider.Namespace,
				"url", gitProvider.Spec.URL)
		})
		return nil, false
	}

	log.V(1).Info("Fetching secret for authentication",
		"secretName", gitProvider.Spec.SecretRef.Name,
		"namespace", gitProvider.Namespace)

	secret, err := r.fetchSecret(ctx, gitProvider.Spec.SecretRef.Name, gitProvider.Namespace)
	if err != nil {
		log.Error(err, "Failed to fetch secret",
			"secretName", gitProvider.Spec.SecretRef.Name,
			"namespace", gitProvider.Namespace)
		r.setStalledConditions(
			gitProvider,
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

	log.V(1).Info("Successfully fetched secret", "secretName", gitProvider.Spec.SecretRef.Name)
	return secret, false
}

// getAuthFromSecret extracts authentication from the secret.
// Returns (auth, result, shouldReturn). If shouldReturn is true, caller should return the result immediately.
func (r *GitProviderReconciler) getAuthFromSecret(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha3.GitProvider,
	secret *corev1.Secret,
) (transport.AuthMethod, ctrl.Result, bool) {
	log.V(1).Info("Extracting credentials from secret")
	auth, err := r.extractCredentials(ctx, gitProvider, secret)
	if err != nil {
		log.Error(err, "Failed to extract credentials from secret")
		secretName := gitProvider.Spec.SecretRef.Name
		r.setStalledConditions(gitProvider, ReasonSecretMalformed,
			fmt.Sprintf("Secret '%s' malformed: %v", secretName, err))
		result, _ := r.updateStatusAndRequeue(ctx, gitProvider)
		return nil, result, true
	}

	log.V(1).Info("Successfully extracted credentials", "hasAuth", auth != nil)
	// secret is nil on the anonymous-access path (no secretRef); in that case
	// there is no secret to report and gitProvider.Spec.SecretRef is nil, so
	// guard the dereference below.
	if secret != nil {
		r.firsts.credentialsLoaded.Do(func() {
			log.Info("GitProvider credentials loaded from secret",
				"secretName", gitProvider.Spec.SecretRef.Name,
				"namespace", gitProvider.Namespace)
		})
	}
	return auth, ctrl.Result{}, false
}

// validateAndUpdateStatus validates repository connectivity and updates the status.
func (r *GitProviderReconciler) validateAndUpdateStatus(
	ctx context.Context,
	log logr.Logger,
	gitProvider *configbutleraiv1alpha3.GitProvider,
	auth transport.AuthMethod,
) (ctrl.Result, error) {
	log.V(1).Info("Validating repository connectivity",
		"url", gitProvider.Spec.URL)

	// Check repository connectivity and get branch count
	branchCount, err := r.checkRemoteConnectivity(ctx, gitProvider.Spec.URL, auth)
	if err != nil {
		log.Error(err, "Repository connectivity check failed",
			"url", gitProvider.Spec.URL)
		r.setStalledConditions(gitProvider, ReasonConnectionFailed,
			fmt.Sprintf("Failed to connect to repository: %v", err))
		return r.updateStatusAndRequeue(ctx, gitProvider)
	}

	log.V(1).Info("Repository connectivity validated successfully", "branchCount", branchCount)
	message := fmt.Sprintf("Repository connectivity validated for %s", gitProvider.Spec.URL)
	r.setReadyConditions(gitProvider, message)

	log.V(1).Info("GitProvider validation successful", "name", gitProvider.Name)
	log.V(1).Info("Updating status with success condition")

	if err := r.updateStatusWithRetry(ctx, gitProvider); err != nil {
		log.Error(err, "Failed to update GitProvider status")
		return ctrl.Result{}, err
	}

	r.firsts.validationSuccess.Do(func() {
		log.Info("First GitProvider validation completed successfully",
			"name", gitProvider.Name,
			"namespace", gitProvider.Namespace,
			"branchCount", branchCount)
	})
	log.V(1).Info("Status update completed successfully, scheduling requeue", "requeueAfter", RequeueSteadyInterval)
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
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

// extractCredentials resolves Git authentication from the credentials Secret, accepting the
// Kubernetes-native, Flux, and Argo CD key dialects. It delegates to the same reader the write
// path uses (internal/git), so the connectivity check validates exactly what a worker would use —
// including SSH host-key resolution via the GitProvider's knownHostsRef and the install-level
// default ConfigMap.
func (r *GitProviderReconciler) extractCredentials(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha3.GitProvider,
	secret *corev1.Secret,
) (transport.AuthMethod, error) {
	return gitpkg.AuthFromSecretData(ctx, r.Client, gitProvider, secret, r.SSHHostKeys)
}

// checkRemoteConnectivity performs a lightweight check of repository connectivity and returns branch count.
func (r *GitProviderReconciler) checkRemoteConnectivity(
	ctx context.Context, repoURL string, auth transport.AuthMethod,
) (int, error) {
	log := logf.FromContext(ctx).WithName("checkRemoteConnectivity")

	log.V(1).Info("Checking remote repository connectivity", "repoURL", repoURL)

	// Use new CheckRepo abstraction from git package
	repoInfo, err := gitpkg.CheckRepo(ctx, repoURL, auth)
	if err != nil {
		log.Error(err, "Remote connectivity check failed", "repoURL", repoURL)
		return 0, fmt.Errorf("failed to connect to repository: %w", err)
	}

	log.V(1).Info("Remote connectivity check successful", "repoURL", repoURL, "branchCount", repoInfo.RemoteBranchCount)
	return repoInfo.RemoteBranchCount, nil
}

func (r *GitProviderReconciler) validateCommitConfiguration(
	gitProvider *configbutleraiv1alpha3.GitProvider,
) error {
	gitProvider.Status.SigningPublicKey = ""

	if gitProvider.Spec.Commit == nil {
		return nil
	}

	if err := gitpkg.ValidateCommitConfig(gitpkg.ResolveCommitConfig(gitProvider.Spec.Commit)); err != nil {
		return fmt.Errorf("invalid commit configuration: %w", err)
	}

	if gitProvider.Spec.Commit.Signing != nil &&
		strings.TrimSpace(gitProvider.Spec.Commit.Signing.SecretRef.Name) == "" {
		return errors.New("commit.signing.secretRef.name must be set when signing is enabled")
	}

	return nil
}

func (r *GitProviderReconciler) setReadyConditions(
	gitProvider *configbutleraiv1alpha3.GitProvider,
	message string,
) {
	r.setCondition(gitProvider, ConditionTypeReady, metav1.ConditionTrue, ConditionTypeReady, message)
	r.setCondition(
		gitProvider,
		ConditionTypeReconciling,
		metav1.ConditionFalse,
		ConditionTypeReady,
		"Reconciliation complete",
	)
	r.setCondition(
		gitProvider,
		ConditionTypeStalled,
		metav1.ConditionFalse,
		ConditionTypeReady,
		"GitProvider is not stalled",
	)
}

func (r *GitProviderReconciler) setProgressingConditions(
	gitProvider *configbutleraiv1alpha3.GitProvider,
	reason string,
	message string,
) {
	r.setCondition(gitProvider, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setCondition(gitProvider, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	r.setCondition(
		gitProvider,
		ConditionTypeStalled,
		metav1.ConditionFalse,
		reason,
		"Reconciliation is making progress",
	)
}

func (r *GitProviderReconciler) setStalledConditions(
	gitProvider *configbutleraiv1alpha3.GitProvider,
	reason string,
	message string,
) {
	r.setCondition(gitProvider, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setCondition(gitProvider, ConditionTypeReconciling, metav1.ConditionFalse, reason, "Reconciliation is stalled")
	r.setCondition(gitProvider, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
}

// setCondition sets or updates one condition by type.
func (r *GitProviderReconciler) setCondition(
	gitProvider *configbutleraiv1alpha3.GitProvider,
	conditionType string,
	status metav1.ConditionStatus,
	reason,
	message string,
) {
	gitProvider.Status.ObservedGeneration = gitProvider.Generation
	gitProvider.Status.Conditions = upsertCondition(
		gitProvider.Status.Conditions,
		conditionType,
		status,
		reason,
		message,
		gitProvider.Generation,
	)
}

// updateStatusAndRequeue updates the status and requeues on the unified control-plane steady
// interval. The control plane no longer watches Secrets, so every status outcome falls back to
// this single cadence; see docs/future/secret-value-retention-plan.md.
func (r *GitProviderReconciler) updateStatusAndRequeue(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha3.GitProvider,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, gitProvider); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// updateStatusWithRetry updates the status with retry logic to handle race conditions.
//

func (r *GitProviderReconciler) updateStatusWithRetry(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha3.GitProvider,
) error {
	log := logf.FromContext(ctx).WithName("updateStatusWithRetry")

	log.V(1).Info("Starting status update with retry",
		"name", gitProvider.Name,
		"namespace", gitProvider.Namespace,
		"conditionsCount", len(gitProvider.Status.Conditions))

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		log.V(1).Info("Attempting status update")

		// Get the latest version of the resource
		latest := &configbutleraiv1alpha3.GitProvider{}
		key := client.ObjectKeyFromObject(gitProvider)
		if err := r.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("Resource was deleted, nothing to update")
				return true, nil
			}
			log.Error(err, "Failed to get latest resource version")
			return false, err
		}

		log.V(1).Info("Got latest resource version",
			"generation", latest.Generation,
			"resourceVersion", latest.ResourceVersion)

		// Copy our status to the latest version
		latest.Status = gitProvider.Status

		log.V(1).Info("Attempting to update status",
			"conditionsCount", len(latest.Status.Conditions))

		// Attempt to update
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				log.V(1).Info("Resource version conflict, retrying")
				return false, nil
			}
			log.Error(err, "Failed to update status")
			return false, err
		}

		log.V(1).Info("Status update successful")
		return true, nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&configbutleraiv1alpha3.GitProvider{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gitprovider").
		Complete(r)
}
