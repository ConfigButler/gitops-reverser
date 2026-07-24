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
	"k8s.io/client-go/tools/record"
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
// DO NOT REINTRODUCE A FINALIZER HERE. It was dropped because purge-on-delete is a promise only a
// living operator can keep, and the two cases that matter break it. `helm uninstall` removes the
// manager and the ClusterProvider together, so nothing detaches the finalizer, the object strands in
// Terminating forever, and the next `helm install --wait` then fails on it — recovering needs a
// manual kubectl patch. That was hit on a live cluster and is why this shipped. An operator that is
// simply down blocks the delete instead, and then loses the purge anyway if the object is
// force-removed, so the finalizer does not even reliably deliver the guarantee it exists for.
//
// The purge it protected is not needed either, and is now unsafe. Facts are keyed by
// (audit route, group/resource, uid, resourceVersion) and expire on their own, so a re-provisioned
// cluster produces different object UIDs and cannot join a stale fact on the exact key. And since
// spec.attribution.auditRoute is what partitions them, SEVERAL ClusterProviders may share one route
// (an API server posts audit under exactly one), so purging on a single provider's deletion would
// drop the facts of every other provider on that route, and of the operator's own cluster, which was
// never torn down. A purge would have to be re-derived against the route, not the object.
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

	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder
}

// clusterProviderLogFirsts keeps startup progress visible without turning every routine
// revalidation into default-level log noise.
type clusterProviderLogFirsts struct {
	validationSuccess sync.Once
}

// This reconciler reads providers, updates them to shed the retired finalizer, and writes status.
// It never creates or deletes one, so it takes neither verb.
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=configbutler.ai,resources=clusterproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	shed, err := r.shedLegacyFinalizer(ctx, log, &provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	if shed {
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
//
// A failed Update is returned as an error rather than swallowed: a stranded object gets no further
// events of its own, so dropping a transient conflict here would leave it in Terminating until an
// operator restart. The error is what makes the workqueue retry.
func (r *ClusterProviderReconciler) shedLegacyFinalizer(
	ctx context.Context,
	log logr.Logger,
	provider *configbutleraiv1alpha3.ClusterProvider,
) (bool, error) {
	if !controllerutil.RemoveFinalizer(provider, LegacyClusterProviderFinalizer) {
		return false, nil
	}
	if err := r.Update(ctx, provider); err != nil {
		log.Error(err, "remove retired ClusterProvider finalizer failed; will retry", "name", provider.Name)
		return false, fmt.Errorf("remove retired finalizer from ClusterProvider %s: %w", provider.Name, err)
	}
	log.Info("removed the retired fact-purge finalizer", "name", provider.Name)
	return true, nil
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

	st := beginStatus(r.Client, r.Recorder, provider, &provider.Status.Conditions)
	provider.Status.ObservedGeneration = provider.Generation

	valid, reason, message, err := r.validateProviderKubeConfig(ctx, provider)
	if err != nil {
		log.Error(err, "failed to read ClusterProvider kubeconfig Secret", "name", provider.Name)
		return ctrl.Result{}, err
	}

	validated := metav1.ConditionTrue
	rd := newReadiness(message, "ClusterProvider is not stalled")
	if !valid {
		validated = metav1.ConditionFalse
		rd.stalled(reason, message)
	}
	st.set(ClusterProviderConditionValidated, validated, reason, message)
	st.applyReadiness(rd)

	// A failed status write is a real failure, not a verdict: propagate it so the provider is
	// retried rather than left reporting a stale status for a whole steady interval.
	if err := st.commit(ctx); err != nil {
		log.Error(err, "failed to update ClusterProvider status", "name", provider.Name)
		return ctrl.Result{}, err
	}

	if valid {
		r.firsts.validationSuccess.Do(func() {
			log.Info("First ClusterProvider validation completed successfully", "name", provider.Name)
		})
	}
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
