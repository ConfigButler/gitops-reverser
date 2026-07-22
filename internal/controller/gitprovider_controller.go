// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder

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
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
//
// Every gate below contributes to ONE readiness accumulator and every exit goes through
// commitProvider, so the Ready/Reconciling/Stalled trio is written exactly once per reconcile.
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

	st := beginStatus(r.Client, r.Recorder, gitProvider, &gitProvider.Status.Conditions)
	gitProvider.Status.ObservedGeneration = gitProvider.Generation
	rd := newReadiness(
		fmt.Sprintf("Repository connectivity validated for %s", gitProvider.Spec.URL),
		"GitProvider is not stalled",
	)

	if err := r.validateCommitConfiguration(gitProvider); err != nil {
		rd.stalled(ReasonCommitConfigInvalid, err.Error())
		return r.commitProvider(ctx, st, rd)
	}

	if err := r.ensureSigningKey(ctx, gitProvider); err != nil {
		rd.stalled(signingKeyFailureReason(err), err.Error())
		return r.commitProvider(ctx, st, rd)
	}

	secret, shouldReturn := r.fetchAndValidateSecret(ctx, log, rd, gitProvider)
	if shouldReturn {
		return r.commitProvider(ctx, st, rd)
	}

	auth, err := r.extractCredentials(ctx, gitProvider, secret)
	if err != nil {
		log.Error(err, "Failed to extract credentials from secret")
		rd.stalled(ReasonSecretMalformed,
			fmt.Sprintf("Secret '%s' malformed: %v", gitProvider.Spec.SecretRef.Name, err))
		return r.commitProvider(ctx, st, rd)
	}
	// secret is nil on the anonymous-access path (no secretRef); in that case there is no secret to
	// report and gitProvider.Spec.SecretRef is nil, so guard the dereference below.
	if secret != nil {
		r.firsts.credentialsLoaded.Do(func() {
			log.Info("GitProvider credentials loaded from secret",
				"secretName", gitProvider.Spec.SecretRef.Name,
				"namespace", gitProvider.Namespace)
		})
	}

	log.V(1).Info("Validating repository connectivity", "url", gitProvider.Spec.URL)
	branchCount, err := r.checkRemoteConnectivity(ctx, gitProvider.Spec.URL, auth)
	if err != nil {
		log.Error(err, "Repository connectivity check failed", "url", gitProvider.Spec.URL)
		rd.stalled(ReasonConnectionFailed, fmt.Sprintf("Failed to connect to repository: %v", err))
		return r.commitProvider(ctx, st, rd)
	}

	r.firsts.validationSuccess.Do(func() {
		log.Info("First GitProvider validation completed successfully",
			"name", gitProvider.Name,
			"namespace", gitProvider.Namespace,
			"branchCount", branchCount)
	})
	return r.commitProvider(ctx, st, rd)
}

// commitProvider writes the trio and persists the status. Every outcome requeues on the steady
// interval: a GitProvider's failures are all "the remote or the credential changed", which nothing
// this controller does will fix sooner.
func (r *GitProviderReconciler) commitProvider(
	ctx context.Context,
	st *reconcileStatus,
	rd *readiness,
) (ctrl.Result, error) {
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// signingKeyFailureReason classifies an ensureSigningKey failure for the Stalled reason.
func signingKeyFailureReason(err error) string {
	switch {
	case strings.Contains(err.Error(), "not found"),
		strings.Contains(err.Error(), "generateWhenMissing is disabled"):
		return ReasonSecretNotFound
	case strings.Contains(err.Error(), "secretRef.name"):
		return ReasonCommitConfigInvalid
	default:
		return ReasonSecretMalformed
	}
}

// fetchAndValidateSecret fetches the secret if specified.
// Returns (secret, shouldReturn). If shouldReturn is true, caller should return immediately.
func (r *GitProviderReconciler) fetchAndValidateSecret(
	ctx context.Context,
	log logr.Logger,
	rd *readiness,
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
		rd.stalled(ReasonSecretNotFound, fmt.Sprintf(
			"Secret '%s' not found in namespace '%s': %v",
			gitProvider.Spec.SecretRef.Name, gitProvider.Namespace, err))
		return nil, true
	}

	log.V(1).Info("Successfully fetched secret", "secretName", gitProvider.Spec.SecretRef.Name)
	return secret, false
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
