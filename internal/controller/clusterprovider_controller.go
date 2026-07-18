// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-logr/logr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
)

// LegacyClusterProviderFinalizer is the fact-purge finalizer this controller USED to take. It is
// no longer added; it is only ever REMOVED, so an object created by an older operator can still be
// deleted after an upgrade instead of stranding in Terminating.
//
// It was dropped because purge-on-delete is a promise only a living operator can keep, and the two
// cases that matter break it: `helm uninstall` removes the manager and the ClusterProvider
// together, so nothing detaches the finalizer and the object strands forever (which then blocks
// reinstalling); and an operator that is simply down blocks the delete, then loses the purge anyway
// if the object is force-removed. Nor is the purge needed: facts are keyed by
// (cluster, group/resource, uid, resourceVersion) and expire on their own, so a re-provisioned
// cluster reusing a provider name produces different object UIDs and cannot join a stale fact on
// the exact key. See docs/design/clusterprovider-fact-purge.md.
const LegacyClusterProviderFinalizer = "configbutler.ai/clusterprovider-fact-purge"

// ClusterProviderReconciler reconciles a ClusterProvider object. It is the read-side peer of the
// GitProviderReconciler: it validates the cluster's connectivity inputs (spec.kubeConfig) without
// dialing, and owns the per-cluster status the watch engine and GitTargets project from. The
// in-cluster "default" provider has no kubeConfig and is trivially Validated.
type ClusterProviderReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// OperatorNamespace is the namespace a remote provider's kubeConfig Secret is pinned to (the
	// operator's own namespace). A cluster-scoped provider has no namespace of its own, so the
	// credential for a cluster is always read from here — never from the source cluster.
	OperatorNamespace string

	// KubeConfigSafety gates exec-auth and insecure-TLS kubeconfigs (reject-not-strip), matching
	// what the watch engine's resolver enforces, so Validated agrees with what a watch would use.
	KubeConfigSafety kubeconfig.SafetyPolicy

	firsts clusterProviderLogFirsts
}

// clusterProviderLogFirsts keeps startup progress visible without turning every routine
// revalidation into default-level log noise.
type clusterProviderLogFirsts struct {
	validationSuccess sync.Once
}

// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders/status,verbs=get;update;patch

// Reconcile validates a ClusterProvider's inputs and updates its status.
func (r *ClusterProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("ClusterProviderReconciler")
	log.V(1).Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	var provider configbutleraiv1alpha3.ClusterProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ClusterProvider not found, was likely deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ClusterProvider", "name", req.Name)
		return ctrl.Result{}, err
	}

	// This controller takes NO finalizer. It only sheds the legacy one, so an object created by
	// an older operator can still be deleted after an upgrade. Deletion is otherwise ordinary:
	// nothing has to happen before a ClusterProvider goes away.
	if r.shedLegacyFinalizer(ctx, log, &provider) {
		return ctrl.Result{}, nil
	}
	if !provider.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	return r.reconcileClusterProvider(ctx, log, &provider)
}

// shedLegacyFinalizer removes the retired fact-purge finalizer if the object still carries one, and
// reports whether it did (in which case the update it wrote will re-trigger this reconcile).
//
// This is the upgrade path, and it must run BEFORE the deletion check: an object stuck in
// Terminating from an older operator is only recoverable by removing the finalizer, and a
// controller that returns early on deletionTimestamp would leave it stranded forever.
func (r *ClusterProviderReconciler) shedLegacyFinalizer(
	ctx context.Context,
	log logr.Logger,
	provider *configbutleraiv1alpha3.ClusterProvider,
) bool {
	if !controllerutil.RemoveFinalizer(provider, LegacyClusterProviderFinalizer) {
		return false
	}
	if err := r.Update(ctx, provider); err != nil {
		log.Error(err, "remove retired ClusterProvider finalizer failed; will retry", "name", provider.Name)
		return false
	}
	log.Info("removed the retired fact-purge finalizer", "name", provider.Name)
	return true
}

// reconcileClusterProvider performs the main validation logic.
func (r *ClusterProviderReconciler) reconcileClusterProvider(
	ctx context.Context,
	log logr.Logger,
	provider *configbutleraiv1alpha3.ClusterProvider,
) (ctrl.Result, error) {
	log.V(1).Info("Validating ClusterProvider",
		"name", provider.Name,
		"inCluster", provider.IsInCluster(),
		"generation", provider.Generation)

	r.setProgressingConditions(provider, ReasonChecking, "Validating cluster provider inputs...")

	valid, reason, message, err := r.validateProviderKubeConfig(ctx, provider)
	if err != nil {
		log.Error(err, "failed to read ClusterProvider kubeconfig Secret", "name", provider.Name)
		return ctrl.Result{}, err
	}
	if !valid {
		r.setCondition(provider, ClusterProviderConditionValidated, metav1.ConditionFalse, reason, message)
		r.setStalledConditions(provider, reason, message)
		result, _ := r.updateStatusAndRequeue(ctx, provider)
		return result, nil
	}

	r.setCondition(provider, ClusterProviderConditionValidated, metav1.ConditionTrue, reason, message)
	r.setReadyConditions(provider, message)

	if err := r.updateStatusWithRetry(ctx, provider); err != nil {
		log.Error(err, "failed to update ClusterProvider status", "name", provider.Name)
		return ctrl.Result{}, err
	}

	r.firsts.validationSuccess.Do(func() {
		log.Info("First ClusterProvider validation completed successfully", "name", provider.Name)
	})
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// validateProviderKubeConfig is the legibility gate for spec.kubeConfig (feeds Validated). It
// reads and parses the kubeconfig Secret FROM THE OPERATOR NAMESPACE and applies the exec/TLS
// safety policy, but deliberately does NOT dial the cluster — reachability is a runtime signal the
// watch engine records on the Reachable condition. It returns ok=false with a typed reason and a
// legible message when an input is wrong; a non-NotFound read error is returned as err so the
// reconcile requeues rather than falsely reporting a bad input. An omitted kubeConfig is an
// in-cluster provider — trivially valid, regardless of its name.
func (r *ClusterProviderReconciler) validateProviderKubeConfig(
	ctx context.Context,
	provider *configbutleraiv1alpha3.ClusterProvider,
) (bool, string, string, error) {
	if provider.IsInCluster() {
		return true, ReasonInCluster, "in-cluster provider (no kubeConfig); the operator's own cluster", nil
	}
	if provider.Spec.KubeConfig.SecretRef == nil {
		// A remote provider needs a Secret reference. configMapRef is CEL-rejected, so secretRef is
		// the only supported path.
		return false, ReasonKubeConfigInvalid,
			"spec.kubeConfig.secretRef is required for a remote ClusterProvider", nil
	}
	ref := provider.Spec.KubeConfig.SecretRef
	secretKey := k8stypes.NamespacedName{Namespace: r.OperatorNamespace, Name: ref.Name}

	var secret corev1.Secret
	if getErr := r.Get(ctx, secretKey, &secret); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, kubeconfig.ReasonSecretNotFound, fmt.Sprintf(
				"spec.kubeConfig.secretRef names Secret %s, which does not exist in the operator namespace",
				secretKey), nil
		}
		return false, "", "", fmt.Errorf("read kubeconfig Secret %s: %w", secretKey, getErr)
	}

	raw, usedKey, present := kubeconfig.ResolveKey(secret.Data, ref.Key)
	if !present {
		return false, kubeconfig.ReasonKeyNotFound, fmt.Sprintf(
			"kubeconfig Secret %s has no kubeconfig under key %q (set spec.kubeConfig.secretRef.key)",
			secretKey, describeKubeConfigKey(ref.Key)), nil
	}
	if rej := kubeconfig.Check(raw, r.KubeConfigSafety); rej != nil {
		// A RejectionError is a validation VERDICT, not a reconcile error: surfaced as the
		// Validated=False reason/message, reconcile requeues normally (err == nil).
		//nolint:nilerr
		return false, rej.Reason, fmt.Sprintf("kubeconfig Secret %s key %q: %s", secretKey, usedKey, rej.Message), nil
	}
	return true, ReasonValidated, fmt.Sprintf("kubeconfig Secret %s validated", secretKey), nil
}

func (r *ClusterProviderReconciler) setReadyConditions(
	provider *configbutleraiv1alpha3.ClusterProvider,
	message string,
) {
	r.setCondition(provider, ConditionTypeReady, metav1.ConditionTrue, ConditionTypeReady, message)
	r.setCondition(provider, ConditionTypeReconciling, metav1.ConditionFalse, ConditionTypeReady,
		"Reconciliation complete")
	r.setCondition(provider, ConditionTypeStalled, metav1.ConditionFalse, ConditionTypeReady,
		"ClusterProvider is not stalled")
}

func (r *ClusterProviderReconciler) setProgressingConditions(
	provider *configbutleraiv1alpha3.ClusterProvider,
	reason, message string,
) {
	r.setCondition(provider, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setCondition(provider, ConditionTypeReconciling, metav1.ConditionTrue, reason, message)
	r.setCondition(provider, ConditionTypeStalled, metav1.ConditionFalse, reason,
		"Reconciliation is making progress")
}

func (r *ClusterProviderReconciler) setStalledConditions(
	provider *configbutleraiv1alpha3.ClusterProvider,
	reason, message string,
) {
	r.setCondition(provider, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	r.setCondition(provider, ConditionTypeReconciling, metav1.ConditionFalse, reason,
		"Reconciliation is stalled")
	r.setCondition(provider, ConditionTypeStalled, metav1.ConditionTrue, reason, message)
}

// setCondition sets or updates one condition by type and pins observedGeneration.
func (r *ClusterProviderReconciler) setCondition(
	provider *configbutleraiv1alpha3.ClusterProvider,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.Conditions = upsertCondition(
		provider.Status.Conditions,
		conditionType,
		status,
		reason,
		message,
		provider.Generation,
	)
}

// updateStatusAndRequeue updates the status and requeues on the steady interval.
func (r *ClusterProviderReconciler) updateStatusAndRequeue(
	ctx context.Context,
	provider *configbutleraiv1alpha3.ClusterProvider,
) (ctrl.Result, error) {
	if err := r.updateStatusWithRetry(ctx, provider); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueSteadyInterval}, nil
}

// updateStatusWithRetry updates the status, re-reading the latest object on conflict.
func (r *ClusterProviderReconciler) updateStatusWithRetry(
	ctx context.Context,
	provider *configbutleraiv1alpha3.ClusterProvider,
) error {
	return wait.ExponentialBackoff(wait.Backoff{
		Duration: RetryInitialDuration,
		Factor:   RetryBackoffFactor,
		Jitter:   RetryBackoffJitter,
		Steps:    RetryMaxSteps,
	}, func() (bool, error) {
		latest := &configbutleraiv1alpha3.ClusterProvider{}
		key := client.ObjectKeyFromObject(provider)
		if err := r.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		latest.Status = provider.Status
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// clusterProviderReconcilePredicate admits spec changes and the start of deletion. A deletion
// transition is needed only for the upgrade path: an older object can still carry the retired
// fact-purge finalizer, and this controller must shed it promptly rather than waiting for periodic
// reconciliation. The controller never adds a finalizer itself.
func clusterProviderReconcilePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			oldDeleting := e.ObjectOld.GetDeletionTimestamp() != nil
			newDeleting := e.ObjectNew.GetDeletionTimestamp() != nil
			return oldDeleting != newDeleting || e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&configbutleraiv1alpha3.ClusterProvider{},
			builder.WithPredicates(clusterProviderReconcilePredicate()),
		).
		Named("clusterprovider").
		Complete(r)
}
