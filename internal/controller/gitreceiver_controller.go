// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-logr/logr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// GitReceiverReasonListenerDisabled is the stall for a GitReceiver the operator does not serve: it
// runs with --receiver-bind-address=0, so nothing listens for the path it publishes.
const GitReceiverReasonListenerDisabled = "ListenerDisabled"

// GitReceiverReconciler reconciles a GitReceiver: it reads the token Secret, publishes the path the
// receiver is served on, and reports whether anything serves it.
//
// It takes no finalizer: a delivery reads the GitReceiver and its token afresh, so nothing has to
// happen before one goes away.
type GitReceiverReconciler struct {
	client.Client

	Scheme *runtime.Scheme

	// ListenerEnabled reports whether this operator serves the push receiver listener
	// (--receiver-bind-address). Without it a GitReceiver publishes its path and reports
	// ListenerDisabled, because no delivery can reach it.
	ListenerEnabled bool

	// Recorder emits a Kubernetes Event on every persisted Ready transition; nil disables Events.
	Recorder record.EventRecorder
}

// This reconciler reads receivers and writes their status. It never creates, updates or deletes
// one, so it takes none of those verbs. Secrets are read by name (get); the token is never watched.
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitreceivers,verbs=get;list;watch
// +kubebuilder:rbac:groups=configbutler.ai,resources=gitreceivers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile reads a GitReceiver's token and publishes its path and readiness.
func (r *GitReceiverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("GitReceiverReconciler")

	var receiver configbutleraiv1alpha3.GitReceiver
	if err := r.Get(ctx, req.NamespacedName, &receiver); err != nil {
		if apierrors.IsNotFound(err) {
			// The object is gone, so its condition series must stop rather than latch its last value.
			telemetry.ForgetResourceConditions(conditionKindGitReceiver, req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !receiver.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	st := beginStatus(r.Client, r.Recorder, &receiver)
	rd, err := r.evaluate(ctx, log, &receiver)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.applyReadiness(rd)
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	cadence := RequeueSteadyInterval
	if receiver.Status.WebhookPath == "" {
		// Secrets are not watched, so a token Secret created after its GitReceiver is noticed only
		// by looking again, and until it is, no delivery can reach the receiver.
		cadence = RequeueReceiverTokenInterval
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(cadence)}, nil
}

// evaluate publishes status.webhookPath and returns the verdict. The path is published ahead of
// every other gate: an operator wiring a Git host's webhook needs it while the receiver is still
// red, for instance before the listener is switched on.
//
// The token is read on every reconcile, so a rotated token moves the path at the next one. The
// receiver recomputes the path from the token it reads per delivery, so the old path stops serving
// at once, whatever this status still says.
func (r *GitReceiverReconciler) evaluate(
	ctx context.Context,
	log logr.Logger,
	receiver *configbutleraiv1alpha3.GitReceiver,
) (*readiness, error) {
	secretKey := client.ObjectKey{Namespace: receiver.Namespace, Name: receiver.Spec.SecretRef.Name}
	var secret corev1.Secret
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			// A read that failed says nothing about the token: keep the published path and retry.
			return nil, fmt.Errorf("read GitReceiver token Secret %s: %w", secretKey, err)
		}
		log.V(1).Info("GitReceiver token Secret not found", "secret", secretKey)
		receiver.Status.WebhookPath = ""
		rd := newReadiness("", "GitReceiver is not stalled")
		rd.stalled(ReasonSecretNotFound, fmt.Sprintf(
			"spec.secretRef names Secret %s, which does not exist in the GitReceiver's namespace; "+
				"status.webhookPath is published once it does", secretKey))
		return rd, nil
	}
	token := secret.Data[configbutleraiv1alpha3.ReceiverTokenKey]
	if len(token) == 0 {
		receiver.Status.WebhookPath = ""
		rd := newReadiness("", "GitReceiver is not stalled")
		rd.stalled(ReasonSecretMalformed, fmt.Sprintf(
			"Secret %s has no %q key; status.webhookPath is published once it does",
			secretKey, configbutleraiv1alpha3.ReceiverTokenKey))
		return rd, nil
	}
	receiver.Status.WebhookPath = configbutleraiv1alpha3.ReceiverPath(token, receiver.Namespace, receiver.Name)

	rd := newReadiness(fmt.Sprintf("Serving %s push deliveries on %s", receiver.Spec.Type,
		receiver.Status.WebhookPath), "GitReceiver is not stalled")
	rd.stalledIf(!r.ListenerEnabled, GitReceiverReasonListenerDisabled,
		"The operator serves no push receiver (--receiver-bind-address=0; chart value "+
			"servers.receiver.enabled=false), so no delivery reaches status.webhookPath. Foreign pushes are "+
			"still found by the next rejected push or the periodic refresh")
	return rd, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitReceiverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&configbutleraiv1alpha3.GitReceiver{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named("gitreceiver").
		Complete(r)
}
