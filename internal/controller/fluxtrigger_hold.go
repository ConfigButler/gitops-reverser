// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// A FluxTrigger holds its Kustomization suspended while idle; the branch workers resume it for each
// apply and suspend it again (internal/applier/flux.go). The reconciler does the rest of the
// lifecycle, from docs/design/apply-trigger.md ("Who owns spec.suspend"):
//
//   - Adoption: a Configured trigger suspends its Kustomization, under GitOps Reverser's field
//     manager, with a token; Suspended (and so Ready) waits until the Kustomization handled it.
//   - External resume: a held Kustomization found resumed with no operation of a branch worker in
//     flight was resumed by somebody else (`flux resume`). It is suspended again with a token, every
//     time, with a Normal Event, and recorded as an Unresolved operation that completes once that
//     token is handled. There is no step-back.
//   - Hand-back: the finalizer suspends once more, waits until that token is handled, and releases
//     spec.suspend, so Flux owns the Kustomization's applies again.

// holdKustomization holds a Configured FluxTrigger's Kustomization suspended, and returns the
// trigger's Suspended condition. It writes nothing while a branch worker's operation is in flight:
// the worker resumes the Kustomization and suspends it again itself.
func (r *ApplyTriggerReconciler) holdKustomization(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, configured conditionValue,
) conditionValue {
	if configured.Status != metav1.ConditionTrue {
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotHeld,
			Message: "the Kustomization is held once the trigger is Configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	held, err := r.readHold(ctx, trigger)
	if err != nil {
		return keptSuspended(trigger, err.Error())
	}
	if held.inFlight() && held.operation.Token != "" {
		return keptSuspended(trigger, fmt.Sprintf("Kustomization %s is resumed for an apply", trigger.Applier()))
	}
	return r.hold(ctx, trigger, held)
}

// heldState is what holdKustomization reads before it decides.
type heldState struct {
	flux       *applier.Flux
	suspension applier.Suspension
	operation  *configbutleraiv1alpha3.ApplyOperation
}

// inFlight reports whether the trigger records an operation that has not completed.
func (h heldState) inFlight() bool { return h.operation != nil && !h.operation.Phase.Completed() }

// readHold reads the Kustomization's suspension and the trigger's operation, uncached.
func (r *ApplyTriggerReconciler) readHold(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (heldState, error) {
	ref := trigger.Applier()
	flux, err := r.fluxClient(ctx, trigger)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot reach the cluster of Kustomization %s: %w", ref, err)
	}
	suspension, err := flux.Suspension(ctx, ref)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read Kustomization %s: %w", ref, err)
	}
	operation, err := r.liveOperation(ctx, trigger)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read the operation of %s: %w", trigger.Name, err)
	}
	return heldState{flux: flux, suspension: suspension, operation: operation}, nil
}

// hold suspends the Kustomization when it is not held and fenced, and completes the operation an
// external resume was recorded as once it is.
func (r *ApplyTriggerReconciler) hold(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, held heldState,
) conditionValue {
	ref := trigger.Applier()
	fencing := func(message string) conditionValue {
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonFencing, Message: message}
	}
	switch {
	case !held.suspension.Suspended:
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptSuspended(trigger, err.Error())
		}
		r.suspended(trigger, token, heldBefore(trigger) || held.inFlight())
		return fencing(fmt.Sprintf("Kustomization %s is being suspended", ref))
	case !held.suspension.Held:
		// Suspended by somebody else's write (`flux suspend`): take the field over, with a token, so
		// nothing that write let run is taken for idle.
		if _, err := held.flux.Suspend(ctx, ref); err != nil {
			return keptSuspended(trigger, err.Error())
		}
		return fencing(fmt.Sprintf("Kustomization %s is being suspended under GitOps Reverser", ref))
	case !held.suspension.Fenced:
		return fencing(fmt.Sprintf("Kustomization %s is suspended; waiting for it to handle the suspend", ref))
	}
	if held.inFlight() {
		completeExternalOperation(trigger, held.operation, held.suspension)
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonHeld,
		Message: fmt.Sprintf("Kustomization %s is suspended; GitOps Reverser starts every apply it makes", ref)}
}

// suspended records a Kustomization just suspended: an adoption, or, for one held before, an
// external resume, recorded as an Unresolved operation that completes once token is handled.
func (r *ApplyTriggerReconciler) suspended(trigger *configbutleraiv1alpha3.FluxTrigger, token string, resumed bool) {
	ref := trigger.Applier()
	if !resumed {
		r.event(trigger, corev1.EventTypeNormal, EventReasonAdopted,
			"suspended Kustomization %s: from now on GitOps Reverser starts every apply it makes", ref)
		return
	}
	r.event(trigger, corev1.EventTypeNormal, EventReasonResumedElsewhere,
		"Kustomization %s was resumed outside GitOps Reverser, which suspended it again; the apply "+
			"that resume started was not checked against the branch", ref)
	trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, FenceToken: token,
		RequestedAt: metav1.NewTime(time.Now()),
		Message: "the Kustomization was resumed outside GitOps Reverser; suspended again, and " +
			"complete once nothing that resume started is running",
	}
}

// keptSuspended is the Suspended value a check that could not be made publishes: the last one, or
// Unknown.
func keptSuspended(trigger *configbutleraiv1alpha3.FluxTrigger, message string) conditionValue {
	if cond := findCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended); cond != nil {
		return conditionValue{Status: cond.Status, Reason: cond.Reason, Message: cond.Message}
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonProgressing, Message: message}
}

// completeExternalOperation completes recorded, the operation an external resume was recorded as,
// once the suspend after it was handled, with what the Kustomization's last apply reported.
// recorded is the operation read uncached, which the hold decided on: the cached trigger can still
// lack it.
func completeExternalOperation(
	trigger *configbutleraiv1alpha3.FluxTrigger, recorded *configbutleraiv1alpha3.ApplyOperation,
	suspension applier.Suspension,
) {
	if recorded == nil {
		return
	}
	operation := recorded.DeepCopy()
	finished := metav1.NewTime(time.Now())
	operation.FinishedAt, operation.AppliedRevision = &finished, suspension.Applied.AppliedRevision
	operation.Phase = configbutleraiv1alpha3.ApplyOperationSucceeded
	operation.Message = "the Kustomization was resumed outside GitOps Reverser, and is suspended again"
	if suspension.Applied.Outcome != git.ApplySucceeded {
		operation.Phase = configbutleraiv1alpha3.ApplyOperationFailed
		operation.Message += ": " + suspension.Applied.Message
	}
	trigger.Status.Operation = operation
}

// heldBefore reports whether the trigger last said it held its Kustomization.
func heldBefore(trigger *configbutleraiv1alpha3.FluxTrigger) bool {
	return apimeta.IsStatusConditionTrue(trigger.Status.Conditions, ApplyTriggerConditionSuspended)
}

// liveOperation reads the trigger's status.operation uncached: a branch worker writes it just before
// it resumes the Kustomization, and a cached read could still show the operation before it.
func (r *ApplyTriggerReconciler) liveOperation(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (*configbutleraiv1alpha3.ApplyOperation, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	live := &configbutleraiv1alpha3.FluxTrigger{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(trigger), live); err != nil {
		return nil, err
	}
	return live.Status.Operation, nil
}

// fluxClient is the Flux client of the trigger's cluster.
func (r *ApplyTriggerReconciler) fluxClient(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (*applier.Flux, error) {
	dynamicClient, err := r.ApplierClients(ctx, trigger.ClusterProvider())
	if err != nil {
		return nil, err
	}
	newFlux := r.newFlux
	if newFlux == nil {
		newFlux = applier.NewFlux
	}
	return newFlux(dynamicClient), nil
}

// releaseDeadline is how long a deleted FluxTrigger tries to hand its Kustomization back, counted
// from its deletionTimestamp, before it lets go anyway with a Warning Event that says what the
// Kustomization is left as. The fence it waits for is handled once the apply running then ends, so
// the deadline is longer than an apply usually takes; a kustomize-controller that is down never
// handles it, and the deletion must not wait on that forever.
const releaseDeadline = 10 * time.Minute

// release hands a deleted FluxTrigger's Kustomization back to Flux, and then lets the trigger go:
// it suspends it once more if it is resumed (a branch worker's apply may still be running), waits
// until that suspend's token is handled, and releases spec.suspend. A Kustomization or a cluster
// that is gone has nothing to hand back, and one the identity may not write is let go with a
// Warning Event: neither should hold the trigger forever. Nor does anything else past
// releaseDeadline.
func (r *ApplyTriggerReconciler) release(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(trigger, configbutleraiv1alpha3.FluxTriggerFinalizer) {
		return ctrl.Result{}, nil
	}
	step, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	pending := r.releaseStep(step, trigger)
	cancel()
	if pending != nil {
		if time.Since(trigger.DeletionTimestamp.Time) < releaseDeadline {
			return pending.result, pending.err
		}
		r.event(trigger, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"gave up handing Kustomization %s back to Flux %s after the trigger was deleted: %s",
			trigger.Applier(), releaseDeadline, pending.left)
	}
	controllerutil.RemoveFinalizer(trigger, configbutleraiv1alpha3.FluxTriggerFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, trigger))
}

// pendingRelease is a hand-back that is not done: what to retry it on, and what the Kustomization is
// left as should the trigger go now.
type pendingRelease struct {
	result ctrl.Result
	err    error
	left   string
}

// releaseStep takes the hand-back one step, and returns nil once the trigger may go.
func (r *ApplyTriggerReconciler) releaseStep(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) *pendingRelease {
	flux, err := r.releaseClient(ctx, trigger)
	if err != nil {
		return &pendingRelease{
			err: err, left: fmt.Sprintf("its cluster cannot be reached (%v), so it is left as it was", err),
		}
	}
	if flux == nil {
		return nil
	}
	ref := trigger.Applier()
	fencing := &pendingRelease{
		result: ctrl.Result{RequeueAfter: applyTriggerFenceInterval},
		left: "it is suspended, and has not handled the suspend yet (is kustomize-controller running?); " +
			"`flux resume` resumes it",
	}
	suspension, err := flux.Suspension(ctx, ref)
	switch {
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return nil
	case err != nil:
		return r.releaseFailed(trigger, err, fmt.Sprintf("it cannot be read (%v), so it is left as it was", err))
	case !suspension.Held:
		return nil
	case !suspension.Suspended:
		if _, err := flux.Suspend(ctx, ref); err != nil {
			return r.releaseFailed(trigger, err, fmt.Sprintf("it is resumed, and suspending it failed (%v), so "+
				"GitOps Reverser's field manager still holds spec.suspend", err))
		}
		return fencing
	case !suspension.Fenced:
		return fencing
	}
	keeper, err := flux.Release(ctx, ref)
	if err != nil {
		return r.releaseFailed(trigger, err, fmt.Sprintf("it is suspended, and releasing spec.suspend failed (%v); "+
			"`flux resume` resumes it", err))
	}
	if keeper != "" {
		r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
			"handed Kustomization %s back to Flux: it stays suspended, because field manager %q also sets "+
				"spec.suspend", ref, keeper)
		return nil
	}
	r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
		"handed Kustomization %s back to Flux: it is resumed, and applies on its own again", ref)
	return nil
}

// releaseFailed is a hand-back step that failed with err: one the identity may not make lets the
// trigger go with a Warning Event, since a retry cannot change it, and any other is retried.
func (r *ApplyTriggerReconciler) releaseFailed(
	trigger *configbutleraiv1alpha3.FluxTrigger, err error, left string,
) *pendingRelease {
	if apierrors.IsForbidden(err) {
		r.event(trigger, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"could not hand Kustomization %s back to Flux: %v", trigger.Applier(), err)
		return nil
	}
	return &pendingRelease{err: err, left: left}
}

// releaseClient is the Flux client a deleted trigger hands its Kustomization back through, or nil
// when there is nothing to reach: no cluster clients are wired, or the ClusterProvider is gone.
func (r *ApplyTriggerReconciler) releaseClient(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (*applier.Flux, error) {
	if r.ApplierClients == nil {
		return nil, nil //nolint:nilnil // nothing to reach is no error
	}
	provider := &configbutleraiv1alpha3.ClusterProvider{}
	err := r.Get(ctx, client.ObjectKey{Name: trigger.ClusterProvider()}, provider)
	if apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Info("the trigger's ClusterProvider is gone; nothing to hand back",
			"clusterProvider", trigger.ClusterProvider())
		return nil, nil //nolint:nilnil // as above
	}
	if err != nil {
		return nil, err
	}
	flux, err := r.fluxClient(ctx, trigger)
	if err != nil {
		return nil, fmt.Errorf("reach the cluster of Kustomization %s to hand it back: %w", trigger.Applier(), err)
	}
	return flux, nil
}

// event records an Event on the trigger, when there is a recorder.
func (r *ApplyTriggerReconciler) event(object client.Object, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, messageFmt, args...)
	}
}

// fluxClientFactory builds the Flux client of a dynamic client; a test sets one.
type fluxClientFactory func(client dynamic.Interface) *applier.Flux

// publishConsumers sets the trigger's status.source.otherConsumers and status.source.prefetch: the
// consumers of its GitRepository in the applier's cluster, other than its own Kustomization, that no
// FluxTrigger on the same ClusterProvider holds. Prefetch also needs its own Kustomization held,
// which held, this pass's Suspended, says. A listing that fails publishes none and no prefetch,
// because the precondition cannot be checked. Information only (decision 2): it never affects Ready.
func (r *ApplyTriggerReconciler) publishConsumers(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, held conditionValue,
) {
	source := trigger.Status.Source
	if source == nil || r.ApplierClients == nil {
		return
	}
	source.OtherConsumers, source.Prefetch = nil, false
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	flux, err := r.fluxClient(ctx, trigger)
	if err != nil {
		return
	}
	consumers, err := flux.Consumers(ctx, k8stypes.NamespacedName{
		Namespace: source.GitRepository.Namespace, Name: source.GitRepository.Name,
	})
	if err != nil {
		logf.FromContext(ctx).V(1).Info("cannot list the GitRepository's consumers; not prefetching it",
			"gitRepository", source.GitRepository.Namespace+"/"+source.GitRepository.Name, "error", err.Error())
		return
	}
	others, err := r.heldKustomizations(ctx, trigger.ClusterProvider())
	if err != nil {
		return
	}
	for _, consumer := range consumers {
		key := k8stypes.NamespacedName{Namespace: consumer.Namespace, Name: consumer.Name}
		itself := key == trigger.Applier()
		if consumer.Kind == configbutleraiv1alpha3.ApplierKindKustomization && (itself || others[key]) {
			continue
		}
		source.OtherConsumers = append(source.OtherConsumers, consumer)
	}
	source.Prefetch = len(source.OtherConsumers) == 0 && held.Status == metav1.ConditionTrue
}

// heldKustomizations is every Kustomization a FluxTrigger on the ClusterProvider holds: one whose
// Suspended condition is True. A trigger that is not Configured (SuspendOwnedElsewhere), or has not
// fenced its Kustomization yet, does not hold it: Flux may still apply a new artifact on its own.
func (r *ApplyTriggerReconciler) heldKustomizations(
	ctx context.Context,
	cluster string,
) (map[k8stypes.NamespacedName]bool, error) {
	var triggers configbutleraiv1alpha3.FluxTriggerList
	if err := r.List(ctx, &triggers); err != nil {
		return nil, err
	}
	held := map[k8stypes.NamespacedName]bool{}
	for i := range triggers.Items {
		trigger := &triggers.Items[i]
		if trigger.DeletionTimestamp.IsZero() && trigger.ClusterProvider() == cluster && heldBefore(trigger) {
			held[trigger.Applier()] = true
		}
	}
	return held, nil
}
