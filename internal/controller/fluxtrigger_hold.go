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
//     manager, with a token; Held (and so Ready) waits until the Kustomization handled it.
//   - External resume: a held Kustomization found resumed with no operation of a branch worker in
//     flight was resumed by somebody else (`flux resume`). It is suspended again with a token, every
//     time, with a Normal ClaimedResourceReconciled Event, and recorded as an Unresolved operation
//     that completes once that token is handled. There is no step-back.
//   - Hand-back: the finalizer suspends once more, waits until that token is handled, and releases
//     spec.suspend, so Flux owns the Kustomization's applies again.
//
// All of it is the holder's alone (applier.HeldByAnother): another trigger naming the same Kustomization
// is Configured=False (HeldByAnotherTrigger), so it never suspends or records an external
// resume, and its deletion hands nothing back. It takes the Kustomization over, with an adoption,
// once the holder is gone.

// holdKustomization holds a Configured FluxTrigger's Kustomization suspended, and returns the
// trigger's Held condition. It writes nothing while a branch worker's operation is in flight
// and followed: the worker resumes the Kustomization and suspends it again itself. One nobody
// follows any more is suspended here (suspendUnfollowed), so the Kustomization is never left resumed
// with nothing guarding it.
func (r *ApplyTriggerReconciler) holdKustomization(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.FluxTrigger,
) conditionValue {
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	held, err := r.readHold(ctx, trigger)
	if err != nil {
		return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, err.Error())
	}
	if !held.inFlight() || held.operation.Token == "" {
		r.unfollowed.forget(trigger, "")
	}
	if held.inFlight() && held.operation.Token != "" {
		if r.followed(trigger, held.operation) {
			return applyingCondition(held.operation)
		}
		return r.suspendUnfollowed(ctx, trigger, held)
	}
	return r.hold(ctx, st, trigger, held)
}

// applyingCondition is Held=True (Applying) for a branch worker's operation in flight that is
// followed: the Kustomization is resumed for it, or about to be, and suspended again when it ends.
func applyingCondition(operation *configbutleraiv1alpha3.ApplyOperation) conditionValue {
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonApplying, Message: fmt.Sprintf(
		"applying %s for a %s; suspended again when it ends", applier.DescribeCommit(operation.Commit),
		operation.Reason)}
}

// OperationFollower answers whether this process carries out or follows a branch worker's
// operation: (*applier.Dispatcher).Following, which runs beside this reconciler on the leader.
type OperationFollower interface {
	Following(trigger git.TriggerRef, token string) bool
}

// DefaultUnfollowedGrace is how long a branch worker's operation in flight may go unfollowed
// before the reconciler suspends it. It covers the moments nobody follows an operation that is fine:
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

// suspendUnfollowed sees a branch worker's operation that nobody follows through, as the worker's own
// follow would have: it suspends the Kustomization with a token, waits until that is handled, and
// completes the operation Failed, since whether its apply ran cannot be told from here. The worker,
// should it still name the trigger, asks again. The suspend token is recorded on the operation, so a
// worker that recovers it meanwhile follows the same suspend.
func (r *ApplyTriggerReconciler) suspendUnfollowed(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, held heldState,
) conditionValue {
	ref := trigger.Applier()
	operation := held.operation.DeepCopy()
	switch {
	case !held.suspension.Suspended || !held.suspension.Held:
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, err.Error())
		}
		r.event(trigger, corev1.EventTypeWarning, EventReasonOperationUnfollowed,
			"the %s apply of %s is followed by nobody; suspended Kustomization %s again", operation.Reason,
			applier.DescribeCommit(operation.Commit), ref)
		operation.SuspendToken = token
		operation.Message = "followed by nobody; suspended again, and complete once nothing it started is running"
		trigger.Status.Operation = operation
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonSuspending,
			Message: fmt.Sprintf("Kustomization %s is being suspended: its apply is followed by nobody", ref)}
	case !held.suspension.Handled:
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonSuspending,
			Message: fmt.Sprintf("Kustomization %s is suspended; waiting for it to handle the suspend", ref)}
	}
	finished := metav1.NewTime(time.Now())
	operation.FinishedAt, operation.AppliedRevision = &finished, held.suspension.Applied.AppliedRevision
	operation.Phase = configbutleraiv1alpha3.ApplyOperationFailed
	operation.Message = "followed by nobody to its end; the Kustomization is suspended again, and whether the apply " +
		"ran cannot be told"
	trigger.Status.Operation = operation
	r.unfollowed.forget(trigger, operation.Token)
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSuspended,
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
func (u *unfollowedOperations) since(trigger configbutleraiv1alpha3.ApplyTrigger, token string) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := unfollowedKey{
		trigger: k8stypes.NamespacedName{Namespace: trigger.GetNamespace(), Name: trigger.GetName()}, token: token,
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
func (u *unfollowedOperations) forget(trigger configbutleraiv1alpha3.ApplyTrigger, token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	name := k8stypes.NamespacedName{Namespace: trigger.GetNamespace(), Name: trigger.GetName()}
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
	return a.Token == b.Token && a.SuspendToken == b.SuspendToken && a.Phase == b.Phase &&
		a.RequestedAt.Equal(&b.RequestedAt)
}

// hold suspends the Kustomization when it is not held with its suspend handled, and completes the operation an
// external resume was recorded as once it is.
func (r *ApplyTriggerReconciler) hold(
	ctx context.Context, st *reconcileStatus, trigger *configbutleraiv1alpha3.FluxTrigger, held heldState,
) conditionValue {
	ref := trigger.Applier()
	suspending := func(message string) conditionValue {
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonSuspending, Message: message}
	}
	unrecorded := func(err error) conditionValue {
		return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, fmt.Sprintf(
			"cannot record how Kustomization %s stood before it is held: %v", ref, err))
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
			return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: message}
		}
		if err := r.recordPreviousSuspendState(ctx, st, trigger, held.suspension.Held, false); err != nil {
			return unrecorded(err)
		}
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, err.Error())
		}
		r.suspended(trigger, token, heldBefore(trigger) || held.inFlight())
		return suspending(fmt.Sprintf("Kustomization %s is being suspended", ref))
	case !held.suspension.Held:
		// Suspended by somebody else's write (`flux suspend`): take the field over, with a token, so
		// nothing that write let run is taken for idle.
		if err := r.recordPreviousSuspendState(ctx, st, trigger, false, true); err != nil {
			return unrecorded(err)
		}
		if _, err := held.flux.Suspend(ctx, ref); err != nil {
			return keptCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld, err.Error())
		}
		return suspending(fmt.Sprintf("Kustomization %s is being suspended under GitOps Reverser", ref))
	case !held.suspension.Handled:
		return suspending(fmt.Sprintf("Kustomization %s is suspended; waiting for it to handle the suspend", ref))
	}
	if held.inFlight() {
		completeExternalOperation(trigger, held.operation, held.suspension)
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSuspended,
		Message: fmt.Sprintf("Kustomization %s is suspended; GitOps Reverser starts every apply it makes", ref)}
}

// recordPreviousSuspendState records in status.previousSuspendState, on adoption, whether the applier
// was suspended before the trigger first held it (held: whether GitOps Reverser's field manager holds
// it already), which the hand-back puts back. Only on adoption, so the value is neither GitOps
// Reverser's own nor a later resume or switch-on, and only where the adoption goes ahead. It is
// written before the write that holds the applier: after that write the applier no longer shows how
// it stood, so a status write lost after it would lose the value for good.
func (r *ApplyTriggerReconciler) recordPreviousSuspendState(
	ctx context.Context, st *reconcileStatus, trigger configbutleraiv1alpha3.ApplyTrigger, held, suspended bool,
) error {
	if held || trigger.PreviousSuspendState() != "" || heldBefore(trigger) {
		return nil
	}
	state := suspendState(suspended)
	resourceVersion, err := r.patchPreviousSuspendState(ctx, trigger, state)
	if err != nil {
		return err
	}
	trigger.SetPreviousSuspendState(state)
	trigger.SetResourceVersion(resourceVersion)
	if before, ok := st.before.(configbutleraiv1alpha3.ApplyTrigger); ok {
		before.SetPreviousSuspendState(state)
	}
	st.metadataWritten()
	return nil
}

// patchPreviousSuspendState writes state alone to trigger's status.previousSuspendState, guarded by
// its resourceVersion, and returns the resourceVersion the write left. trigger itself is not changed.
func (r *ApplyTriggerReconciler) patchPreviousSuspendState(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger, state configbutleraiv1alpha3.SuspendState,
) (string, error) {
	base, ok := trigger.DeepCopyObject().(configbutleraiv1alpha3.ApplyTrigger)
	if !ok {
		return "", fmt.Errorf("a copy of %s %s/%s is no trigger", trigger.TriggerKind(), trigger.GetNamespace(),
			trigger.GetName())
	}
	patched, _ := base.DeepCopyObject().(configbutleraiv1alpha3.ApplyTrigger)
	patched.SetPreviousSuspendState(state)
	if err := r.Status().Patch(ctx, patched,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return "", fmt.Errorf("write status.previousSuspendState of %s %s/%s: %w", trigger.TriggerKind(),
			trigger.GetNamespace(), trigger.GetName(), err)
	}
	return patched.GetResourceVersion(), nil
}

// passPreviousSuspendState gives a successor in line what trigger recorded of how the applier stood
// before it was held, before trigger lets go: the successor takes over an applier GitOps Reverser
// holds already, so it cannot read that for itself, and without it its hand-back would resume a
// Kustomization that was suspended, or switch on an Application that was manual. A successor that
// recorded a value of its own keeps it.
func (r *ApplyTriggerReconciler) passPreviousSuspendState(
	ctx context.Context, trigger, successor configbutleraiv1alpha3.ApplyTrigger,
) error {
	state := trigger.PreviousSuspendState()
	if state == "" || successor.PreviousSuspendState() != "" {
		return nil
	}
	_, err := r.patchPreviousSuspendState(ctx, successor, state)
	return err
}

// suspendState is the SuspendState of an applier that is suspended or not.
func suspendState(suspended bool) configbutleraiv1alpha3.SuspendState {
	if suspended {
		return configbutleraiv1alpha3.SuspendStateSuspended
	}
	return configbutleraiv1alpha3.SuspendStateNotSuspended
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
	r.event(trigger, corev1.EventTypeNormal, EventReasonClaimedResourceReconciled,
		"Kustomization %s was resumed outside GitOps Reverser, which suspended it again; the apply "+
			"that resume started was not checked against the branch", ref)
	trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, SuspendToken: token,
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

// heldBefore reports whether the trigger last said it held its applier.
func heldBefore(trigger configbutleraiv1alpha3.ApplyTrigger) bool {
	return apimeta.IsStatusConditionTrue(*trigger.StatusConditions(), ApplyTriggerConditionHeld)
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
	dynamicClient, err := r.ApplierClients(ctx, trigger.KubeProviderKey().String())
	if err != nil {
		return nil, err
	}
	return applier.NewFlux(dynamicClient), nil
}

// releaseDeadline is how long a retiring trigger tries to hand its applier back, counted from when
// its Held condition turned Releasing, before it lets go anyway with a Warning Event that says what
// the applier is left as. A Kustomization's suspend is handled once the apply running then ends, so
// the deadline is longer than an apply usually takes; a kustomize-controller that is down never
// handles it, and the deletion must not wait on that forever.
const releaseDeadline = 10 * time.Minute

// retire hands a trigger's applier back, and then lets go of it: a trigger being deleted (check
// nil), or one no eligible GitTarget references any more (check is what the reconcile found). A
// FluxTrigger suspends its Kustomization once more if it is resumed (a branch worker's apply may
// still be running), waits until that suspend's token is handled, and releases spec.suspend; an
// ArgoCDTrigger gives its Application's automated sync switch up (releaseStep). Neither hands back
// what a trigger in line takes over. Until that is done the trigger stays the holder, Held=False
// (Releasing), so no successor acts on the applier meanwhile. An applier that is gone has nothing
// to hand back; a cluster no client reaches any more since its KubeProvider went (releaseClient),
// and an applier the identity may not write, are let go with a Warning Event: none should hold the
// trigger forever. Nor does anything else past releaseDeadline. A trigger behind another in line
// for the applier hands nothing back: the applier is that one's. Letting go removes the finalizer;
// a live trigger then holds nothing until an eligible GitTarget references it again, which adopts
// the applier anew.
func (r *ApplyTriggerReconciler) retire(
	ctx context.Context, st *reconcileStatus, trigger configbutleraiv1alpha3.ApplyTrigger, check *triggerCheck,
) (ctrl.Result, error) {
	holder, err := applier.HeldByAnother(ctx, r.Client, trigger)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pending *pendingRelease
	if holder != nil {
		// A trigger in line never held the applier: what it holds is the holder's, under the same
		// field manager, so there is nothing of this trigger's to hand back.
		logf.FromContext(ctx).Info("the applier is held by another trigger; handing nothing back",
			"applier", trigger.ApplierKind()+" "+trigger.Applier().String(),
			"holder", holder.GetNamespace()+"/"+holder.GetName())
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
		since := releasingSince(trigger)
		if time.Since(since) < releaseDeadline {
			r.publishReleasing(st, trigger, check, since)
			if err := st.commit(ctx); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: st.requeueAfter(pending.result.RequeueAfter)}, pending.err
		}
		r.event(trigger, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"gave up handing %s %s back to %s %s after it began: %s",
			trigger.ApplierKind(), trigger.Applier(), applierTool(trigger), releaseDeadline, pending.left)
	}
	if check == nil {
		controllerutil.RemoveFinalizer(trigger, triggerFinalizer(trigger))
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, trigger))
	}
	if err := r.setFinalizer(ctx, st, trigger, false); err != nil {
		return ctrl.Result{}, err
	}
	r.letGoOfOperation(trigger)
	clearPreviousSuspendState(trigger)
	held := conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
		Message: fmt.Sprintf("nothing to hold: no GitTarget names this trigger, and its %s was handed back",
			trigger.ApplierKind())}
	if check.refs.Named() {
		held = conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
			Message: fmt.Sprintf("the %s was handed back, and is held again once an eligible GitTarget "+
				"references the trigger", trigger.ApplierKind())}
	}
	st.setValue(ApplyTriggerConditionHeld, held)
	st.applyReadiness(r.triggerReadiness(trigger, check.configured, &held))
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(heldTriggerSteadyInterval)}, nil
}

// clearPreviousSuspendState drops what a trigger recorded of its applier before it held it, once it
// has handed it back (or on to a successor): a later adoption records anew.
func clearPreviousSuspendState(trigger configbutleraiv1alpha3.ApplyTrigger) {
	trigger.SetPreviousSuspendState("")
}

// triggerFinalizer is the finalizer of trigger's kind: the one it carries while it holds its
// applier.
func triggerFinalizer(trigger configbutleraiv1alpha3.ApplyTrigger) string {
	if trigger.TriggerKind() == configbutleraiv1alpha3.ApplyTriggerKindArgoCD {
		return configbutleraiv1alpha3.ArgoCDTriggerFinalizer
	}
	return configbutleraiv1alpha3.FluxTriggerFinalizer
}

// applierTool is the tool a trigger's applier belongs to, which a hand-back hands it back to.
func applierTool(trigger configbutleraiv1alpha3.ApplyTrigger) string {
	if trigger.TriggerKind() == configbutleraiv1alpha3.ApplyTriggerKindArgoCD {
		return "Argo CD"
	}
	return "Flux"
}

// publishReleasing sets Held=False (Releasing) and the trio on a trigger whose hand-back is not
// done, its deadline counted from since. The condition is written anew when it was not Releasing
// before, so its lastTransitionTime says when the release began.
func (r *ApplyTriggerReconciler) publishReleasing(
	st *reconcileStatus, trigger configbutleraiv1alpha3.ApplyTrigger, check *triggerCheck, since time.Time,
) {
	why := "the trigger is being deleted"
	if check != nil && check.refs.Named() {
		why = "no GitTarget naming the trigger is eligible any more"
	} else if check != nil {
		why = "no GitTarget names the trigger any more"
	}
	held := conditionValue{Status: metav1.ConditionFalse, Reason: ReasonReleasing, Message: fmt.Sprintf(
		"%s: handing %s %s back to %s, by %s at the latest", why, trigger.ApplierKind(), trigger.Applier(),
		applierTool(trigger), since.Add(releaseDeadline).UTC().Format(time.RFC3339))}
	if previous := findCondition(*trigger.StatusConditions(), ApplyTriggerConditionHeld); previous == nil ||
		previous.Reason != ReasonReleasing {
		st.remove(ApplyTriggerConditionHeld)
	}
	st.setValue(ApplyTriggerConditionHeld, held)
	if check == nil {
		rd := newReadiness("", trigger.TriggerKind()+" is not stalled")
		rd.progressing(held.Status, held.Reason, held.Message)
		st.applyReadiness(rd)
		return
	}
	st.applyReadiness(r.triggerReadiness(trigger, check.configured, &held))
}

// releasingSince is when the trigger's Held condition turned Releasing, or now when it has not.
func releasingSince(trigger configbutleraiv1alpha3.ApplyTrigger) time.Time {
	held := findCondition(*trigger.StatusConditions(), ApplyTriggerConditionHeld)
	if held == nil || held.Reason != ReasonReleasing || held.LastTransitionTime.IsZero() {
		return time.Now()
	}
	return held.LastTransitionTime.Time
}

// letGoOfOperation completes an operation a retired trigger still records in flight, when this
// process does not follow it: nobody would complete it, and its applier is handed back.
func (r *ApplyTriggerReconciler) letGoOfOperation(trigger configbutleraiv1alpha3.ApplyTrigger) {
	operation := trigger.Operation()
	if operation == nil || operation.Phase.Completed() {
		return
	}
	ref := git.TriggerRef{Kind: trigger.TriggerKind(), Namespace: trigger.GetNamespace(), Name: trigger.GetName()}
	if operation.Token != "" && r.Follower != nil && r.Follower.Following(ref, operation.Token) {
		return
	}
	completed := operation.DeepCopy()
	finished := metav1.NewTime(time.Now())
	completed.Phase, completed.FinishedAt = configbutleraiv1alpha3.ApplyOperationFailed, &finished
	completed.Message = fmt.Sprintf("the trigger handed its %s back before the apply was seen to end",
		trigger.ApplierKind())
	trigger.SetOperation(completed, trigger.RepairPending())
	r.unfollowed.forget(trigger, "")
}

// setFinalizer puts the trigger's finalizer on it, or takes it off, and carries the metadata that
// write left into the status session, so the status patch that follows is made against it. The
// finalizer is what hands the applier back, so it is in place before the first write that holds it,
// and off once the trigger has handed the applier back or over.
func (r *ApplyTriggerReconciler) setFinalizer(
	ctx context.Context, st *reconcileStatus, trigger configbutleraiv1alpha3.ApplyTrigger, present bool,
) error {
	update, ok := trigger.DeepCopyObject().(configbutleraiv1alpha3.ApplyTrigger)
	if !ok {
		return fmt.Errorf("a copy of %s %s/%s is no trigger", trigger.TriggerKind(), trigger.GetNamespace(),
			trigger.GetName())
	}
	changed := controllerutil.RemoveFinalizer(update, triggerFinalizer(trigger))
	if present {
		changed = controllerutil.AddFinalizer(update, triggerFinalizer(trigger))
	}
	if !changed {
		return nil
	}
	if err := r.Update(ctx, update); err != nil {
		return err
	}
	trigger.SetFinalizers(update.GetFinalizers())
	trigger.SetResourceVersion(update.GetResourceVersion())
	st.metadataWritten()
	return nil
}

// nextInLine is the trigger that takes the applier over once trigger is gone, or nil when no
// trigger will: one in line that is not being deleted, and whose Configured is True or False only
// because trigger holds the applier (HeldByAnotherTrigger is checked last, so such a trigger
// passed every other check).
func (r *ApplyTriggerReconciler) nextInLine(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger,
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

// pendingRelease is a hand-back that is not done: what to retry it on, and what the applier is
// left as should the trigger go now.
type pendingRelease struct {
	result ctrl.Result
	err    error
	left   string
}

// releaseStep takes the hand-back of trigger's applier one step, and returns nil once the trigger
// may go.
func (r *ApplyTriggerReconciler) releaseStep(
	ctx context.Context, trigger, successor configbutleraiv1alpha3.ApplyTrigger,
) *pendingRelease {
	if successor != nil {
		if err := r.passPreviousSuspendState(ctx, trigger, successor); err != nil {
			return &pendingRelease{err: err, left: fmt.Sprintf("it is left held, and %s %s/%s, which holds it "+
				"next, does not know how it stood before it was held (%v)", successor.TriggerKind(),
				successor.GetNamespace(), successor.GetName(), err)}
		}
	}
	switch t := trigger.(type) {
	case *configbutleraiv1alpha3.FluxTrigger:
		return r.releaseKustomization(ctx, t, successor)
	case *configbutleraiv1alpha3.ArgoCDTrigger:
		return r.releaseAutomatedSync(ctx, t, successor)
	}
	return nil
}

// releaseKustomization takes a FluxTrigger's hand-back one step. With a successor (nextInLine), the
// Kustomization is not released but left suspended, its suspend handled, for it: released, Flux
// would apply it at once, unchecked against the branch, before the successor's adoption could
// suspend it again.
func (r *ApplyTriggerReconciler) releaseKustomization(
	ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger, successor configbutleraiv1alpha3.ApplyTrigger,
) *pendingRelease {
	dynamicClient, err := r.releaseClient(ctx, trigger)
	if err != nil {
		return &pendingRelease{
			err: err, left: fmt.Sprintf("its cluster cannot be reached (%v), so it is left as it was", err),
		}
	}
	if dynamicClient == nil {
		return nil
	}
	flux := applier.NewFlux(dynamicClient)
	ref := trigger.Applier()
	suspending := &pendingRelease{
		result: ctrl.Result{RequeueAfter: applyTriggerSuspendingInterval},
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
		return suspending
	case !suspension.Handled:
		return suspending
	}
	if successor != nil {
		r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
			"left Kustomization %s suspended for %s %s/%s, which holds it next", ref, successor.TriggerKind(),
			successor.GetNamespace(), successor.GetName())
		return nil
	}
	return r.handBackKustomization(ctx, flux, trigger)
}

// handBackKustomization releases a FluxTrigger's Kustomization, suspended and with its suspend
// handled, as status.previousSuspendState recorded it, with a Released Event that says how it is left.
func (r *ApplyTriggerReconciler) handBackKustomization(
	ctx context.Context, flux *applier.Flux, trigger *configbutleraiv1alpha3.FluxTrigger,
) *pendingRelease {
	ref := trigger.Applier()
	wasSuspended := trigger.Status.PreviousSuspendState == configbutleraiv1alpha3.SuspendStateSuspended
	keeper, err := flux.Release(ctx, ref, wasSuspended)
	if err != nil {
		return r.releaseFailed(trigger, err, fmt.Sprintf("it is suspended, and releasing spec.suspend failed (%v); "+
			"`flux resume` resumes it", err))
	}
	if wasSuspended {
		r.event(trigger, corev1.EventTypeNormal, EventReasonReleased,
			"handed Kustomization %s back to Flux: it stays suspended, as it was before GitOps Reverser held it; "+
				"`flux resume` resumes it", ref)
		return nil
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
	trigger configbutleraiv1alpha3.ApplyTrigger, err error, left string,
) *pendingRelease {
	if apierrors.IsForbidden(err) {
		r.event(trigger, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"could not hand %s %s back to %s: %v", trigger.ApplierKind(), trigger.Applier(), applierTool(trigger), err)
		return nil
	}
	return &pendingRelease{err: err, left: left}
}

// releaseClient is the client of the cluster a retiring trigger hands its applier back in, or nil
// when there is nothing to reach: no cluster clients are wired, or the KubeProvider is gone and no
// client reaches its cluster any more, which lets the trigger go with a Warning Event. A
// KubeProvider that is gone does not end the hand-back by itself: an inCluster one named the
// operator's own cluster, which may still answer, and a remote one may still be reached through the
// client it left.
func (r *ApplyTriggerReconciler) releaseClient(
	ctx context.Context, trigger configbutleraiv1alpha3.ApplyTrigger,
) (dynamic.Interface, error) {
	if r.ApplierClients == nil {
		return nil, nil //nolint:nilnil // nothing to reach is no error
	}
	provider := &configbutleraiv1alpha3.KubeProvider{}
	err := r.Get(ctx, trigger.KubeProviderKey(), provider)
	gone := apierrors.IsNotFound(err)
	if err != nil && !gone {
		return nil, err
	}
	dynamicClient, err := r.ApplierClients(ctx, trigger.KubeProviderKey().String())
	switch {
	case err != nil && gone:
		r.event(trigger, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"could not hand %s %s back to %s: its KubeProvider %s is gone; %s", trigger.ApplierKind(),
			trigger.Applier(), applierTool(trigger), trigger.KubeProviderKey(), leftHeld(trigger))
		return nil, nil //nolint:nilnil // nothing to reach, and the Event says so
	case err != nil:
		return nil, fmt.Errorf("reach the cluster of %s %s to hand it back: %w", trigger.ApplierKind(),
			trigger.Applier(), err)
	}
	return dynamicClient, nil
}

// leftHeld says what an applier a trigger could not hand back is left as, and how a human hands it
// back.
func leftHeld(trigger configbutleraiv1alpha3.ApplyTrigger) string {
	if trigger.TriggerKind() == configbutleraiv1alpha3.ApplyTriggerKindArgoCD {
		return "its automated sync is left off, and switching it on in Argo CD has it sync on its own again"
	}
	return "it is left suspended, and `flux resume` resumes it"
}

// event records an Event on the trigger, when there is a recorder.
func (r *ApplyTriggerReconciler) event(object client.Object, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, messageFmt, args...)
	}
}

// publishConsumers sets the trigger's status.source.otherConsumers: the consumers of its GitRepository
// in the applier's cluster, other than its own Kustomization, that no FluxTrigger on the same
// KubeProvider holds. A listing that fails publishes none. Information only (decision 2): it never
// affects Ready.
func (r *ApplyTriggerReconciler) publishConsumers(ctx context.Context, trigger *configbutleraiv1alpha3.FluxTrigger) {
	source := trigger.Status.Source
	if source == nil || r.ApplierClients == nil {
		return
	}
	source.OtherConsumers = nil
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
		logf.FromContext(ctx).V(1).Info("cannot list the GitRepository's consumers",
			"gitRepository", source.GitRepository.Namespace+"/"+source.GitRepository.Name, "error", err.Error())
		return
	}
	others, err := r.heldKustomizations(ctx, trigger.KubeProviderKey())
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
}

// heldKustomizations is every Kustomization a FluxTrigger on the KubeProvider holds: one whose
// Held condition is True. A trigger that is not Configured (SuspendOwnedElsewhere), or whose
// Kustomization has not handled its suspend yet, does not hold it: Flux may still apply a new artifact on its own.
func (r *ApplyTriggerReconciler) heldKustomizations(
	ctx context.Context,
	cluster k8stypes.NamespacedName,
) (map[k8stypes.NamespacedName]bool, error) {
	var triggers configbutleraiv1alpha3.FluxTriggerList
	if err := r.List(ctx, &triggers); err != nil {
		return nil, err
	}
	held := map[k8stypes.NamespacedName]bool{}
	for i := range triggers.Items {
		trigger := &triggers.Items[i]
		if trigger.DeletionTimestamp.IsZero() && trigger.KubeProviderKey() == cluster && heldBefore(trigger) {
			held[trigger.Applier()] = true
		}
	}
	return held, nil
}
