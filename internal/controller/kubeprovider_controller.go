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
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/kubeconfig"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// KubeProviderReconciler reconciles a KubeProvider object. It is the read-side peer of the
// GitProviderReconciler: it validates the cluster's credential without dialing, reports whether the
// provider may be used at all (the installation's inCluster consent, and its audit route), and owns
// the per-cluster status the watch engine and GitTargets project from.
//
// It takes no finalizer: nothing has to happen before a KubeProvider goes away. Attribution facts
// are keyed by (audit route, group/resource, uid, resourceVersion) and expire on their own, and
// several providers may share one route, so there is nothing to purge per object.
type KubeProviderReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// KubeConfigSafety gates exec-auth and insecure-TLS kubeconfigs (reject-not-strip) and lists the
	// providers allowed to use spec.inCluster, matching what the watch engine's resolver enforces,
	// so Validated agrees with what a watch would use.
	KubeConfigSafety kubeconfig.SafetyPolicy

	// AuditFacts is the per-audit-route fact registry the AuditFactsReceived condition latches on.
	// Nil means the operator runs with author attribution disabled, and the condition is then
	// absent rather than Unknown.
	AuditFacts AuditRouteFacts

	firsts kubeProviderLogFirsts

	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder
}

// kubeProviderLogFirsts keeps startup progress visible without turning every routine
// revalidation into default-level log noise.
type kubeProviderLogFirsts struct {
	validationSuccess sync.Once
}

// This reconciler reads providers and writes their status. It never creates, updates or deletes
// one, so it takes none of those verbs.
// +kubebuilder:rbac:groups=configbutler.ai,resources=kubeproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=kubeproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates a KubeProvider's inputs and updates its status.
func (r *KubeProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("KubeProviderReconciler")
	log.V(1).Info("Starting reconciliation", "namespacedName", req.NamespacedName)

	var provider configbutleraiv1alpha3.KubeProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("KubeProvider not found, was likely deleted", "kubeProvider", req.NamespacedName)
			// The object is gone, so its condition series must stop rather than latch its last
			// value: a gauge that keeps reporting Ready=False for a deleted object is a permanent
			// false positive that trains people to ignore the panel.
			telemetry.ForgetResourceConditions(conditionKindKubeProvider, req.Namespace, req.Name)
			r.forgetAuditRoute(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch KubeProvider", "kubeProvider", req.NamespacedName)
		return ctrl.Result{}, err
	}
	if !provider.DeletionTimestamp.IsZero() {
		// A provider being deleted carries no route (v1alpha3.AuditRouteHolder).
		r.forgetAuditRoute(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	return r.reconcileKubeProvider(ctx, log, &provider)
}

// forgetAuditRoute drops a provider's route from the set the audit census labels by.
func (r *KubeProviderReconciler) forgetAuditRoute(provider k8stypes.NamespacedName) {
	if r.AuditFacts != nil {
		r.AuditFacts.ForgetRoute(provider)
	}
}

// reconcileKubeProvider performs the main validation logic.
func (r *KubeProviderReconciler) reconcileKubeProvider(
	ctx context.Context,
	log logr.Logger,
	provider *configbutleraiv1alpha3.KubeProvider,
) (ctrl.Result, error) {
	log.V(1).Info("Validating KubeProvider",
		"kubeProvider", provider.Key(),
		"inCluster", provider.IsInCluster(),
		"generation", provider.Generation)

	st := beginStatus(r.Client, r.Recorder, provider)

	valid, reason, message, err := r.validateCredential(ctx, provider)
	if err != nil {
		log.Error(err, "failed to read KubeProvider kubeconfig Secret", "kubeProvider", provider.Key())
		return ctrl.Result{}, err
	}
	route, err := authz.AuditRouteHeld(ctx, r.Client, provider)
	if err != nil {
		return ctrl.Result{}, err
	}

	validated := metav1.ConditionTrue
	rd := newReadiness(message, "KubeProvider is not stalled")
	if !valid {
		validated = metav1.ConditionFalse
		rd.stalled(reason, message)
	}
	// A route another namespace holds says nothing about the credential, so Validated stays what
	// the credential earned; the provider is still not used, which is a stall a human must fix.
	if !route.Allowed {
		rd.stalled(route.Reason, route.Message)
	}
	// Declared whether or not this provider holds it: facts posted under a route are labelled by it
	// as long as any provider names it, and the holder declares it too.
	if r.AuditFacts != nil {
		r.AuditFacts.DeclareRoute(provider.Key(), provider.AuditRoute())
	}
	st.set(KubeProviderConditionValidated, validated, reason, message)
	// Written after the readiness trio and deliberately not contributing to it: an audit route
	// nobody posts under costs the commit AUTHOR, never the mirror, so it must not make a working
	// provider look broken to a kstatus reader.
	r.applyAuditFactsCondition(st, provider)
	st.applyReadiness(rd)

	// A failed status write is a real failure, not a verdict: propagate it so the provider is
	// retried rather than left reporting a stale status for a whole steady interval.
	if err := st.commit(ctx); err != nil {
		log.Error(err, "failed to update KubeProvider status", "kubeProvider", provider.Key())
		return ctrl.Result{}, err
	}

	if valid && route.Allowed {
		r.firsts.validationSuccess.Do(func() {
			log.Info("First KubeProvider validation completed successfully", "kubeProvider", provider.Key())
		})
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(RequeueSteadyInterval)}, nil
}

// validateCredential is the legibility gate for the provider's credential (feeds Validated). For
// spec.inCluster it is the installation's consent; for spec.secretRef it reads and parses the
// kubeconfig Secret from THE PROVIDER'S NAMESPACE and applies the safety policy. It deliberately
// does NOT dial the cluster — reachability is a runtime signal the watch engine records. It returns
// ok=false with a typed reason and a legible message when an input is wrong; a non-NotFound read
// error is returned as err so the reconcile requeues rather than falsely reporting a bad input.
func (r *KubeProviderReconciler) validateCredential(
	ctx context.Context,
	provider *configbutleraiv1alpha3.KubeProvider,
) (bool, string, string, error) {
	if provider.IsInCluster() {
		if rej := kubeconfig.CheckInCluster(provider.Key(), r.KubeConfigSafety); rej != nil {
			// A verdict, not a reconcile error, as for the kubeconfig checks below.
			//nolint:nilerr
			return false, rej.Reason, rej.Message, nil
		}
		return true, ReasonInCluster,
			"spec.inCluster: the operator's own identity, allowed by --in-cluster-kube-providers", nil
	}
	if provider.Spec.SecretRef == nil {
		// The schema requires exactly one arm; this is a hand-built object in a test or a server
		// that does not enforce the schema.
		return false, ReasonKubeConfigInvalid, "set exactly one of spec.secretRef and spec.inCluster", nil
	}
	ref := provider.Spec.SecretRef
	secretKey := k8stypes.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}

	var secret corev1.Secret
	if getErr := r.Get(ctx, secretKey, &secret); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, kubeconfig.ReasonSecretNotFound, fmt.Sprintf(
				"spec.secretRef names Secret %s, which does not exist in the KubeProvider's namespace",
				secretKey), nil
		}
		return false, "", "", fmt.Errorf("read kubeconfig Secret %s: %w", secretKey, getErr)
	}

	raw, usedKey, present := kubeconfig.ResolveKey(secret.Data, ref.Key)
	if !present {
		return false, kubeconfig.ReasonKeyNotFound, fmt.Sprintf(
			"kubeconfig Secret %s has no kubeconfig under key %q (set spec.secretRef.key)",
			secretKey, describeKubeConfigKey(ref.Key)), nil
	}
	if rej := kubeconfig.Check(raw, r.KubeConfigSafety); rej != nil {
		// A RejectionError is a validation VERDICT, not a reconcile error: surfaced as the
		// Validated=False reason/message, reconcile requeues normally (err == nil).
		//nolint:nilerr
		return false, rej.Reason, fmt.Sprintf("kubeconfig Secret %s key %q: %s", secretKey, usedKey, rej.Message), nil
	}
	return true, ReasonSucceeded, fmt.Sprintf("kubeconfig Secret %s validated", secretKey), nil
}

// kubeProviderReconcilePredicate admits spec changes only. Status writes, including this
// controller's own, must not loop back into a reconcile.
func kubeProviderReconcilePredicate() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(
			&configbutleraiv1alpha3.KubeProvider{},
			builder.WithPredicates(kubeProviderReconcilePredicate()),
		).
		// The wake path for AuditRouteConflict. A provider stalled on its route clears only when
		// another provider changes: the holder is deleted, or moves to another route. Without this
		// it would wait out its steady requeue, five minutes, before it noticed.
		Watches(
			&configbutleraiv1alpha3.KubeProvider{},
			auditRouteSiblingsHandler(r.Client),
		).
		Named("kubeprovider")

	// React to the FIRST attribution fact on an audit route so AuditFactsReceived latches True
	// within seconds instead of on the provider's next periodic requeue, which for a converged
	// provider is RequeueSteadyInterval. The event names the route; the map fans it out to every
	// provider reading it.
	if r.AuditFacts != nil {
		if events := r.AuditFacts.FirstFactEvents(); events != nil {
			b = b.WatchesRawSource(source.Channel(
				events,
				handler.EnqueueRequestsFromMapFunc(r.auditRouteToKubeProviders),
			))
		}
	}

	return b.Complete(r)
}

// auditRouteSiblingsHandler enqueues, for a provider created, deleted or moved to another route,
// every OTHER provider that carries the route it held or now holds: each of them may have just won
// or lost it. An update that keeps the route enqueues nothing, so status writes cost no fan-out.
func auditRouteSiblingsHandler(reader client.Reader) handler.EventHandler {
	enqueueRoute := func(
		ctx context.Context, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request],
		self client.Object, route string,
	) {
		var providers configbutleraiv1alpha3.KubeProviderList
		if err := reader.List(ctx, &providers); err != nil {
			return
		}
		for i := range providers.Items {
			p := &providers.Items[i]
			if p.AuditRoute() != route || (p.Namespace == self.GetNamespace() && p.Name == self.GetName()) {
				continue
			}
			q.Add(ctrlreconcile.Request{NamespacedName: p.Key()})
		}
	}
	routeOf := func(obj client.Object) (string, bool) {
		p, ok := obj.(*configbutleraiv1alpha3.KubeProvider)
		if !ok {
			return "", false
		}
		return p.AuditRoute(), true
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			if route, ok := routeOf(e.Object); ok {
				enqueueRoute(ctx, q, e.Object, route)
			}
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			if route, ok := routeOf(e.Object); ok {
				enqueueRoute(ctx, q, e.Object, route)
			}
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent,
			q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			oldRoute, okOld := routeOf(e.ObjectOld)
			newRoute, okNew := routeOf(e.ObjectNew)
			if !okOld || !okNew || oldRoute == newRoute {
				return
			}
			enqueueRoute(ctx, q, e.ObjectNew, oldRoute)
			enqueueRoute(ctx, q, e.ObjectNew, newRoute)
		},
	}
}
