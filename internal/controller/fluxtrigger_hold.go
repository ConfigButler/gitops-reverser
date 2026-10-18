// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
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
//
// All of it is the holder's alone (applier.HeldByAnother): another trigger naming the same Kustomization
// is Configured=False (HeldByAnotherTrigger), so it never suspends, fences or records an external
// resume, and its deletion hands nothing back. It takes the Kustomization over, with an adoption,
// once the holder is gone.

// holdKustomization holds a Configured FluxTrigger's Kustomization suspended, and returns the
// trigger's Suspended condition. It writes nothing while a branch worker's operation is in flight
// and followed: the worker resumes the Kustomization and suspends it again itself. One nobody
// follows any more is fenced here (fenceUnfollowed), so the Kustomization is never left resumed
// with nothing guarding it.
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
		return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended, err.Error())
	}
	if !held.inFlight() || held.operation.Token == "" {
		r.unfollowed.forget(trigger, "")
	}
	if held.inFlight() && held.operation.Token != "" {
		if r.followed(trigger, held.operation) {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended,
				fmt.Sprintf("Kustomization %s is resumed for an apply", trigger.Applier()))
		}
		return r.fenceUnfollowed(ctx, trigger, held)
	}
	return r.hold(ctx, trigger, held)
}

// OperationFollower answers whether this process carries out or follows a branch worker's
// operation: (*applier.Dispatcher).Following, which runs beside this reconciler on the leader.
type OperationFollower interface {
	Following(trigger git.TriggerRef, token string) bool
}

// DefaultUnfollowedGrace is how long a branch worker's operation in flight may go unfollowed
// before the reconciler fences it. It covers the moments nobody follows an operation that is fine:
// a process that just started, whose worker has not recovered the operation yet, and an operation
// whose completion is not written yet.
const DefaultUnfollowedGrace = time.Minute

// EventReasonOperationUnfollowed is a branch worker's operation nobody followed any more, whose
// Kustomization the reconciler suspended and whose operation it completed.
const EventReasonOperationUnfollowed = "OperationUnfollowed"

// followed reports whether the reconciler leaves the trigger's in-flight operation to its worker:
// this process follows it, or has seen it unfollowed for less than the grace.
func (r *ApplyTriggerReconciler) followed(
	trigger *configbutleraiv1alpha3.FluxTrigger, operation *configbutleraiv1alpha3.ApplyOperation,
) bool {
	ref := git.TriggerRef{Kind: trigger.TriggerKind(), Namespace: trigger.Namespace, Name: trigger.Name}
	if r.Follower != nil && r.Follower.Following(ref, operation.Token) {
		r.unfollowed.forget(trigger, operation.Token)
		return true
	}
	grace := r.UnfollowedGrace
	if grace <= 0 {
		grace = DefaultUnfollowedGrace
	}
	return time.Since(r.unfollowed.since(trigger, operation.Token)) < grace
}

// fenceUnfollowed sees a branch worker's operation that nobody follows through, as the worker's own
// follow would have: it suspends the Kustomization with a token, waits until that is handled, and
// completes the operation Failed, since whether its apply ran cannot be told from here. The worker,
// should it still name the trigger, asks again. The fence token is recorded on the operation, so a
// worker that recovers it meanwhile follows the same fence.
func (r *ApplyTriggerReconciler) fenceUnfollowed(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, held heldState,
) conditionValue {
	ref := trigger.Applier()
	operation := held.operation.DeepCopy()
	switch {
	case !held.suspension.Suspended || !held.suspension.Held:
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended, err.Error())
		}
		r.event(trigger, corev1.EventTypeWarning, EventReasonOperationUnfollowed,
			"the %s apply of %s is followed by nobody; suspended Kustomization %s again", operation.Reason,
			applier.DescribeCommit(operation.Commit), ref)
		operation.FenceToken = token
		operation.Message = "followed by nobody; suspended again, and complete once nothing it started is running"
		trigger.Status.Operation = operation
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonFencing,
			Message: fmt.Sprintf("Kustomization %s is being suspended: its apply is followed by nobody", ref)}
	case !held.suspension.Fenced:
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonFencing,
			Message: fmt.Sprintf("Kustomization %s is suspended; waiting for it to handle the suspend", ref)}
	}
	finished := metav1.NewTime(time.Now())
	operation.FinishedAt, operation.AppliedRevision = &finished, held.suspension.Applied.AppliedRevision
	operation.Phase = configbutleraiv1alpha3.ApplyOperationFailed
	operation.Message = "followed by nobody to its end; the Kustomization is suspended again, and whether the apply " +
		"ran cannot be told"
	trigger.Status.Operation = operation
	r.unfollowed.forget(trigger, operation.Token)
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonHeld,
		Message: fmt.Sprintf("Kustomization %s is suspended; GitOps Reverser starts every apply it makes", ref)}
}

// unfollowedOperations remembers when the reconciler first saw each trigger's in-flight operation
// followed by nobody, by trigger and token. It is in memory: a process that starts gives every
// operation the whole grace, which is when restart recovery picks it up.
type unfollowedOperations struct {
	mu    sync.Mutex
	first map[unfollowedKey]time.Time
}

type unfollowedKey struct {
	trigger k8stypes.NamespacedName
	token   string
}

// since is when the operation was first seen unfollowed, now if this is the first time.
func (u *unfollowedOperations) since(trigger *configbutleraiv1alpha3.FluxTrigger, token string) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := unfollowedKey{
		trigger: k8stypes.NamespacedName{Namespace: trigger.Namespace, Name: trigger.Name}, token: token,
	}
	if u.first == nil {
		u.first = map[unfollowedKey]time.Time{}
	}
	first, seen := u.first[key]
	if !seen {
		first = time.Now()
		u.first[key] = first
	}
	return first
}

// forget drops what is remembered of the trigger's operation with token, or of all its operations
// when token is empty.
func (u *unfollowedOperations) forget(trigger *configbutleraiv1alpha3.FluxTrigger, token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	name := k8stypes.NamespacedName{Namespace: trigger.Namespace, Name: trigger.Name}
	for key := range u.first {
		if key.trigger == name && (token == "" || key.token == token) {
			delete(u.first, key)
		}
	}
}

// heldState is what holdKustomization reads before it decides.
type heldState struct {
	flux       *applier.Flux
	suspension applier.Suspension
	operation  *configbutleraiv1alpha3.ApplyOperation
}

// inFlight reports whether the trigger records an operation that has not completed.
func (h heldState) inFlight() bool { return h.operation != nil && !h.operation.Phase.Completed() }

// readHold reads the Kustomization's suspension and the trigger's operation, uncached. The
// operation is read before and after the Kustomization, and the two must be the same operation in
// the same phase, or the read is an error and the next reconcile reads again. A worker writes its
// operation before it resumes and completes it after it suspended again, so with both reads equal,
// a Kustomization read resumed with no operation in flight was resumed by somebody else. With one
// read, an operation that completed between the two GETs would be taken for an external resume.
func (r *ApplyTriggerReconciler) readHold(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (heldState, error) {
	ref := trigger.Applier()
	flux, err := r.fluxClient(ctx, trigger)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot reach the cluster of Kustomization %s: %w", ref, err)
	}
	before, err := r.liveOperation(ctx, trigger)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read the operation of %s: %w", trigger.Name, err)
	}
	suspension, err := flux.Suspension(ctx, ref)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read Kustomization %s: %w", ref, err)
	}
	operation, err := r.liveOperation(ctx, trigger)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read the operation of %s: %w", trigger.Name, err)
	}
	if !sameOperationPhase(before, operation) {
		return heldState{}, fmt.Errorf("the operation of %s moved while Kustomization %s was read; reading again",
			trigger.Name, ref)
	}
	return heldState{flux: flux, suspension: suspension, operation: operation}, nil
}

// sameOperationPhase reports whether two reads of a trigger's operation show the same operation, by
// its tokens, in the same phase.
func sameOperationPhase(a, b *configbutleraiv1alpha3.ApplyOperation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Token == b.Token && a.FenceToken == b.FenceToken && a.Phase == b.Phase &&
		a.RequestedAt.Equal(&b.RequestedAt)
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
		// Read resumed: the resume can be another trigger's, one that came first in line since the
		// cached read this reconcile decided on. That trigger recorded its operation before it
		// resumed, so a read made now sees it.
		if other, err := applier.HeldByAnother(ctx, r.uncachedReader(), trigger); err != nil || other != nil {
			message := fmt.Sprintf("cannot check which trigger holds Kustomization %s: %v", ref, err)
			if other != nil {
				message = applier.HeldByAnotherMessage(trigger, other)
			}
			return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotHeld, Message: message}
		}
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended, err.Error())
		}
		r.suspended(trigger, token, heldBefore(trigger) || held.inFlight())
		return fencing(fmt.Sprintf("Kustomization %s is being suspended", ref))
	case !held.suspension.Held:
		// Suspended by somebody else's write (`flux suspend`): take the field over, with a token, so
		// nothing that write let run is taken for idle.
		if _, err := held.flux.Suspend(ctx, ref); err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended, err.Error())
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

// completeExternalOperation completes recorded, the operation an external resume was recorded as,
// once the suspend after it was handled, with what the Kustomization's last apply reported.
// recorded is the operation read uncached, which the hold decided on: the cached trigger can still
// lack it.
func completeExternalOperation(
	trigger *configbutleraiv1alpha3.FluxTrigger, recorded *configbutleraiv1alpha3.ApplyOperation,
	suspension applier.Suspension,
) {
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
	live := &configbutleraiv1alpha3.FluxTrigger{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(trigger), live); err != nil {
		return nil, err
	}
	return live.Status.Operation, nil
}

// uncachedReader is APIReader, or the client when none is set.
func (r *ApplyTriggerReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// fluxClient is the Flux client of the trigger's cluster.
func (r *ApplyTriggerReconciler) fluxClient(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (*applier.Flux, error) {
	dynamicClient, err := r.ApplierClients(ctx, trigger.ClusterProvider())
	if err != nil {
		return nil, err
	}
	return applier.NewFlux(dynamicClient), nil
}

// releaseDeadline is how long a deleted FluxTrigger tries to hand its Kustomization back, counted
// from its deletionTimestamp, before it lets go anyway with a Warning Event that says what the
// Kustomization is left as. The fence it waits for is handled once the apply running then ends, so
// the deadline is longer than an apply usually takes; a kustomize-controller that is down never
// handles it, and the deletion must not wait on that forever.
const releaseDeadline = 10 * time.Minute

// release hands a deleted FluxTrigger's Kustomization back to Flux, and then lets the trigger go:
// it suspends it once more if it is resumed (a branch worker's apply may still be running), waits
// until that suspend's token is handled, and releases spec.suspend, unless a trigger in line takes
// the Kustomization over (releaseStep). A Kustomization or a cluster
// that is gone has nothing to hand back, and one the identity may not write is let go with a
// Warning Event: neither should hold the trigger forever. Nor does anything else past
// releaseDeadline. A trigger behind another in line for the Kustomization hands nothing back: the
// Kustomization is that one's.
func (r *ApplyTriggerReconciler) release(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(trigger, configbutleraiv1alpha3.FluxTriggerFinalizer) {
		return ctrl.Result{}, nil
	}
	holder, err := applier.HeldByAnother(ctx, r.Client, trigger)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pending *pendingRelease
	if holder != nil {
		// A trigger in line never held the Kustomization: its spec.suspend is the holder's, under the
		// same field manager, so there is nothing of this trigger's to hand back.
		logf.FromContext(ctx).Info("the Kustomization is held by another trigger; handing nothing back",
			"kustomization", trigger.Applier().String(), "holder", holder.GetNamespace()+"/"+holder.GetName())
	} else {
		successor, err := r.nextInLine(ctx, trigger)
		if err != nil {
			return ctrl.Result{}, err
		}
		step, cancel := context.WithTimeout(ctx, applierCheckTimeout)
		pending = r.releaseStep(step, trigger, successor)
		cancel()
	}
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

// nextInLine is the trigger that takes the Kustomization over once trigger is gone, or nil when no
// trigger will: one in line that is not being deleted, and whose Configured is True or False only
// because trigger holds the Kustomization (HeldByAnotherTrigger is checked last, so such a trigger
// passed every other check).
func (r *ApplyTriggerReconciler) nextInLine(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger,
) (configbutleraiv1alpha3.ApplyTrigger, error) {
	others, err := applier.TriggersOfApplier(ctx, r.Client, trigger)
	if err != nil {
		return nil, err
	}
	var next configbutleraiv1alpha3.ApplyTrigger
	var nextCreated metav1.Time
	for _, other := range others {
		configured := findCondition(*other.StatusConditions(), ApplyTriggerConditionConfigured)
		takesOver := configured != nil && (configured.Status == metav1.ConditionTrue ||
			configured.Reason == applier.ReasonHeldByAnotherTrigger)
		created := other.GetCreationTimestamp()
		if takesOver && other.GetDeletionTimestamp().IsZero() && (next == nil || created.Before(&nextCreated)) {
			next, nextCreated = other, created
		}
	}
	return next, nil
}

// pendingRelease is a hand-back that is not done: what to retry it on, and what the Kustomization is
// left as should the trigger go now.
type pendingRelease struct {
	result ctrl.Result
	err    error
	left   string
}

// releaseStep takes the hand-back one step, and returns nil once the trigger may go. With a
// successor (nextInLine), the Kustomization is not released but left suspended and fenced for it:
// released, Flux would apply it at once, unchecked against the branch, before the successor's
// adoption could suspend it again.
func (r *ApplyTriggerReconciler) releaseStep(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, successor configbutleraiv1alpha3.ApplyTrigger,
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
	if successor != nil {
		r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
			"left Kustomization %s suspended for %s %s/%s, which holds it next", ref, successor.TriggerKind(),
			successor.GetNamespace(), successor.GetName())
		return nil
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
