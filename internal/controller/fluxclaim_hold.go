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

// A FluxClaim holds its Kustomization suspended while idle; the branch workers resume it for each
// apply and suspend it again (internal/applier/flux.go). The reconciler does the rest of the
// lifecycle, from docs/design/apply-trigger.md ("Who owns spec.suspend"):
//
//   - Adoption: a Configured applier claim suspends its Kustomization, under GitOps Reverser's field
//     manager, with a token; InControl (and so Ready) waits until the Kustomization handled it.
//   - External resume: a held Kustomization found resumed with no operation of a branch worker in
//     flight was resumed by somebody else (`flux resume`). It is suspended again with a token, every
//     time, with a Normal ClaimedResourceReconciled Event, and recorded as an Unresolved operation
//     that completes once that token is handled. There is no step-back.
//   - Hand-back: the finalizer suspends once more, waits until that token is handled, and releases
//     spec.suspend, so Flux owns the Kustomization's applies again.
//
// All of it is the holder's alone (applier.HeldByAnother): another applier claim naming the same Kustomization
// is Configured=False (HeldByAnotherClaim), so it never suspends or records an external
// resume, and its deletion hands nothing back. It takes the Kustomization over, with an adoption,
// once the holder is gone.

// holdKustomization holds a Configured FluxClaim's Kustomization suspended, and returns the
// applier claim's InControl condition. It writes nothing while a branch worker's operation is in flight
// and followed: the worker resumes the Kustomization and suspends it again itself. One nobody
// follows any more is suspended here (suspendUnfollowed), so the Kustomization is never left resumed
// with nothing guarding it.
func (r *ApplierClaimReconciler) holdKustomization(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.FluxClaim,
) conditionValue {
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	held, err := r.readHold(ctx, claim)
	if err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, err.Error())
	}
	if !held.inFlight() || held.operation.Token == "" {
		r.unfollowed.forget(claim, "")
	}
	if held.inFlight() && held.operation.Token != "" {
		if r.followed(claim, held.operation) {
			return applyingCondition(held.operation)
		}
		return r.suspendUnfollowed(ctx, claim, held)
	}
	return r.hold(ctx, st, claim, held)
}

// applyingCondition is InControl=True (Applying) for a branch worker's operation in flight that is
// followed: the Kustomization is resumed for it, or about to be, and suspended again when it ends.
func applyingCondition(operation *configbutleraiv1alpha3.ApplyOperation) conditionValue {
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonApplying, Message: fmt.Sprintf(
		"applying %s for a %s; suspended again when it ends", applier.DescribeCommit(operation.Commit),
		operation.Reason)}
}

// OperationFollower answers whether this process carries out or follows a branch worker's
// operation: (*applier.Dispatcher).Following, which runs beside this reconciler on the leader.
type OperationFollower interface {
	Following(claim git.ApplierClaimRef, token string) bool
}

// DefaultUnfollowedGrace is how long a branch worker's operation in flight may go unfollowed
// before the reconciler suspends it. It covers the moments nobody follows an operation that is fine:
// a process that just started, whose worker has not recovered the operation yet, and an operation
// whose completion is not written yet.
const DefaultUnfollowedGrace = time.Minute

// EventReasonOperationUnfollowed is a branch worker's operation nobody followed any more, whose
// Kustomization the reconciler suspended and whose operation it completed.
const EventReasonOperationUnfollowed = "OperationUnfollowed"

// followed reports whether the reconciler leaves the applier claim's in-flight operation to its worker:
// this process follows it, or has seen it unfollowed for less than the grace.
func (r *ApplierClaimReconciler) followed(
	claim *configbutleraiv1alpha3.FluxClaim, operation *configbutleraiv1alpha3.ApplyOperation,
) bool {
	ref := git.ApplierClaimRef{Kind: claim.ClaimKind(), Namespace: claim.Namespace, Name: claim.Name}
	if r.Follower != nil && r.Follower.Following(ref, operation.Token) {
		r.unfollowed.forget(claim, operation.Token)
		return true
	}
	grace := r.UnfollowedGrace
	if grace <= 0 {
		grace = DefaultUnfollowedGrace
	}
	return time.Since(r.unfollowed.since(claim, operation.Token)) < grace
}

// suspendUnfollowed sees a branch worker's operation that nobody follows through, as the worker's own
// follow would have: it suspends the Kustomization with a token, waits until that is handled, and
// completes the operation Failed, since whether its apply ran cannot be told from here. The worker,
// should it still name the applier claim, asks again. The suspend token is recorded on the operation, so a
// worker that recovers it meanwhile follows the same suspend.
func (r *ApplierClaimReconciler) suspendUnfollowed(
	ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim, held heldState,
) conditionValue {
	ref := claim.Applier()
	operation := held.operation.DeepCopy()
	switch {
	case !held.suspension.Suspended || !held.suspension.Held:
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, err.Error())
		}
		r.event(claim, corev1.EventTypeWarning, EventReasonOperationUnfollowed,
			"the %s apply of %s is followed by nobody; suspended Kustomization %s again", operation.Reason,
			applier.DescribeCommit(operation.Commit), ref)
		operation.SuspendToken = token
		operation.Message = "followed by nobody; suspended again, and complete once nothing it started is running"
		claim.Status.Operation = operation
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
	claim.Status.Operation = operation
	r.unfollowed.forget(claim, operation.Token)
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSuspended,
		Message: fmt.Sprintf("Kustomization %s is suspended; GitOps Reverser starts every apply it makes", ref)}
}

// unfollowedOperations remembers when the reconciler first saw each applier claim's in-flight operation
// followed by nobody, by claim and token. It is in memory: a process that starts gives every
// operation the whole grace, which is when restart recovery picks it up.
type unfollowedOperations struct {
	mu    sync.Mutex
	first map[unfollowedKey]time.Time
}

type unfollowedKey struct {
	claim k8stypes.NamespacedName
	token string
}

// since is when the operation was first seen unfollowed, now if this is the first time.
func (u *unfollowedOperations) since(claim configbutleraiv1alpha3.ApplierClaim, token string) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := unfollowedKey{
		claim: k8stypes.NamespacedName{Namespace: claim.GetNamespace(), Name: claim.GetName()}, token: token,
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

// forget drops what is remembered of the applier claim's operation with token, or of all its operations
// when token is empty.
func (u *unfollowedOperations) forget(claim configbutleraiv1alpha3.ApplierClaim, token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	name := k8stypes.NamespacedName{Namespace: claim.GetNamespace(), Name: claim.GetName()}
	for key := range u.first {
		if key.claim == name && (token == "" || key.token == token) {
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

// inFlight reports whether the applier claim records an operation that has not completed.
func (h heldState) inFlight() bool { return h.operation != nil && !h.operation.Phase.Completed() }

// readHold reads the Kustomization's suspension and the applier claim's operation, uncached. The
// operation is read before and after the Kustomization, and the two must be the same operation in
// the same phase, or the read is an error and the next reconcile reads again. A worker writes its
// operation before it resumes and completes it after it suspended again, so with both reads equal,
// a Kustomization read resumed with no operation in flight was resumed by somebody else. With one
// read, an operation that completed between the two GETs would be taken for an external resume.
func (r *ApplierClaimReconciler) readHold(
	ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim,
) (heldState, error) {
	ref := claim.Applier()
	flux, err := r.fluxClient(ctx, claim)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot reach the cluster of Kustomization %s: %w", ref, err)
	}
	before, err := r.liveOperation(ctx, claim)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read the operation of %s: %w", claim.Name, err)
	}
	suspension, err := flux.Suspension(ctx, ref)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read Kustomization %s: %w", ref, err)
	}
	operation, err := r.liveOperation(ctx, claim)
	if err != nil {
		return heldState{}, fmt.Errorf("cannot read the operation of %s: %w", claim.Name, err)
	}
	if !sameOperationPhase(before, operation) {
		return heldState{}, fmt.Errorf("the operation of %s moved while Kustomization %s was read; reading again",
			claim.Name, ref)
	}
	return heldState{flux: flux, suspension: suspension, operation: operation}, nil
}

// sameOperationPhase reports whether two reads of an applier claim's operation show the same operation, by
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
func (r *ApplierClaimReconciler) hold(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.FluxClaim, held heldState,
) conditionValue {
	ref := claim.Applier()
	suspending := func(message string) conditionValue {
		return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonSuspending, Message: message}
	}
	unrecorded := func(err error) conditionValue {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, fmt.Sprintf(
			"cannot record how Kustomization %s stood before it is held: %v", ref, err))
	}
	adopt := adopting(claim, held.operation)
	switch {
	case !held.suspension.Suspended:
		// Read resumed: the resume can be another applier claim's, one that came first in line since the
		// cached read this reconcile decided on. That applier claim recorded its operation before it
		// resumed, so a read made now sees it.
		if other, ok := r.heldElsewhere(ctx, claim); !ok {
			return other
		}
		if err := r.recordPreviousSuspendState(ctx, st, claim, held.suspension.Held, false); err != nil {
			return unrecorded(err)
		}
		token, err := held.flux.Suspend(ctx, ref)
		if err != nil {
			return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, err.Error())
		}
		r.suspended(claim, token, wasInControl(claim) || held.inFlight())
		return suspending(fmt.Sprintf("Kustomization %s is being suspended", ref))
	case !held.suspension.Held || adopt:
		return r.takeOverSuspend(ctx, st, claim, held)
	case !held.suspension.Handled:
		return suspending(fmt.Sprintf("Kustomization %s is suspended; waiting for it to handle the suspend", ref))
	}
	if held.inFlight() {
		completeExternalOperation(claim, held.operation, held.suspension)
	}
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSuspended,
		Message: fmt.Sprintf("Kustomization %s is suspended; GitOps Reverser starts every apply it makes", ref)}
}

// takeOverSuspend takes over a Kustomization suspended by somebody else's write (`flux suspend`), or
// kept suspended under GitOps Reverser's field manager by a hand-back with no applier claim holding it
// now: it suspends it with a token, so nothing that write let run is taken for idle. A kept suspend
// needs the token most: the hand-back gave its requestedAt up, and Flux still reports the last one
// handled, so without a new one the suspend would never read as handled.
func (r *ApplierClaimReconciler) takeOverSuspend(
	ctx context.Context, st *reconcileStatus, claim *configbutleraiv1alpha3.FluxClaim, held heldState,
) conditionValue {
	ref := claim.Applier()
	if held.suspension.Held {
		if other, ok := r.heldElsewhere(ctx, claim); !ok {
			return other
		}
	}
	if err := r.recordPreviousSuspendState(ctx, st, claim, false, true); err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, fmt.Sprintf(
			"cannot record how Kustomization %s stood before it is held: %v", ref, err))
	}
	if _, err := held.flux.Suspend(ctx, ref); err != nil {
		return keptCondition(claim.Status.Conditions, ApplierClaimConditionInControl, err.Error())
	}
	return conditionValue{Status: metav1.ConditionFalse, Reason: ReasonSuspending,
		Message: fmt.Sprintf("Kustomization %s is being suspended under GitOps Reverser", ref)}
}

// heldElsewhere reports, from an uncached read, whether the applier claim's applier is free of any
// other applier claim's hold, and when it is not (or cannot be told) the InControl condition that says so.
// The cached read Configured was judged on can predate another applier claim coming first in line, and
// that one's hold looks like a hand-back's or an apply's from the applier alone.
func (r *ApplierClaimReconciler) heldElsewhere(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim,
) (conditionValue, bool) {
	other, err := applier.HeldByAnother(ctx, r.uncachedReader(), claim)
	if err == nil && other == nil {
		return conditionValue{}, true
	}
	message := fmt.Sprintf("cannot check which applier claim holds %s %s: %v", claim.ApplierKind(),
		claim.Applier(), err)
	if other != nil {
		message = applier.HeldByAnotherMessage(claim, other)
	}
	return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured, Message: message}, false
}

// adopting reports whether the applier claim takes its applier anew: it records nothing of how the
// applier stood, did not last say it was in control, and has no operation in flight (operation, read
// uncached where it matters). A hand-back forgets the record and ends the operation, and a successor
// is passed its predecessor's record. So a field GitOps Reverser's field manager holds when an
// applier claim adopts is no hold in place but one a hand-back kept, set to how the applier stood:
// it is recorded as such, and held anew.
func adopting(claim configbutleraiv1alpha3.ApplierClaim, operation *configbutleraiv1alpha3.ApplyOperation) bool {
	return claim.PreviousSuspendState() == "" && !wasInControl(claim) &&
		(operation == nil || operation.Phase.Completed())
}

// recordPreviousSuspendState records in status.previousSuspendState, on adoption, whether the applier
// was suspended before the applier claim first held it (held: whether GitOps Reverser's field manager holds
// it already), which the hand-back puts back. Only on adoption, so the value is neither GitOps
// Reverser's own nor a later resume or switch-on, and only where the adoption goes ahead. It is
// written before the write that holds the applier: after that write the applier no longer shows how
// it stood, so a status write lost after it would lose the value for good.
func (r *ApplierClaimReconciler) recordPreviousSuspendState(
	ctx context.Context, st *reconcileStatus, claim configbutleraiv1alpha3.ApplierClaim, held, suspended bool,
) error {
	if held || claim.PreviousSuspendState() != "" || wasInControl(claim) {
		return nil
	}
	state := suspendState(suspended)
	resourceVersion, err := r.patchPreviousSuspendState(ctx, claim, state)
	if err != nil {
		return err
	}
	claim.SetPreviousSuspendState(state)
	claim.SetResourceVersion(resourceVersion)
	if before, ok := st.before.(configbutleraiv1alpha3.ApplierClaim); ok {
		before.SetPreviousSuspendState(state)
	}
	st.metadataWritten()
	return nil
}

// patchPreviousSuspendState writes state alone to claim's status.previousSuspendState, guarded by
// its resourceVersion, and returns the resourceVersion the write left. claim itself is not changed.
func (r *ApplierClaimReconciler) patchPreviousSuspendState(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim, state configbutleraiv1alpha3.SuspendState,
) (string, error) {
	base, ok := claim.DeepCopyObject().(configbutleraiv1alpha3.ApplierClaim)
	if !ok {
		return "", fmt.Errorf("a copy of %s %s/%s is no applier claim", claim.ClaimKind(), claim.GetNamespace(),
			claim.GetName())
	}
	patched, _ := base.DeepCopyObject().(configbutleraiv1alpha3.ApplierClaim)
	patched.SetPreviousSuspendState(state)
	if err := r.Status().Patch(ctx, patched,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return "", fmt.Errorf("write status.previousSuspendState of %s %s/%s: %w", claim.ClaimKind(),
			claim.GetNamespace(), claim.GetName(), err)
	}
	return patched.GetResourceVersion(), nil
}

// passPreviousSuspendState gives a successor in line what claim recorded of how the applier stood
// before it was held, before claim lets go: the successor takes over an applier GitOps Reverser
// holds already, so it cannot read that for itself, and without it its hand-back would resume a
// Kustomization that was suspended, or switch on an Application that was manual. A successor that
// recorded a value of its own keeps it.
func (r *ApplierClaimReconciler) passPreviousSuspendState(
	ctx context.Context, claim, successor configbutleraiv1alpha3.ApplierClaim,
) error {
	state := claim.PreviousSuspendState()
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
func (r *ApplierClaimReconciler) suspended(claim *configbutleraiv1alpha3.FluxClaim, token string, resumed bool) {
	ref := claim.Applier()
	if !resumed {
		r.event(claim, corev1.EventTypeNormal, EventReasonAdopted,
			"suspended Kustomization %s: from now on GitOps Reverser starts every apply it makes", ref)
		return
	}
	r.event(claim, corev1.EventTypeNormal, EventReasonClaimedResourceReconciled,
		"Kustomization %s was resumed outside GitOps Reverser, which suspended it again; the apply "+
			"that resume started was not checked against the branch", ref)
	claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, SuspendToken: token,
		RequestedAt: metav1.NewTime(time.Now()),
		Message: "the Kustomization was resumed outside GitOps Reverser; suspended again, and " +
			"complete once nothing that resume started is running",
	}
}

// completeExternalOperation completes recorded, the operation an external resume was recorded as,
// once the suspend after it was handled, with what the Kustomization's last apply reported.
// recorded is the operation read uncached, which the hold decided on: the cached applier claim can still
// lack it.
func completeExternalOperation(
	claim *configbutleraiv1alpha3.FluxClaim, recorded *configbutleraiv1alpha3.ApplyOperation,
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
	claim.Status.Operation = operation
}

// wasInControl reports whether the applier claim last said it was in control of its applier.
func wasInControl(claim configbutleraiv1alpha3.ApplierClaim) bool {
	return apimeta.IsStatusConditionTrue(*claim.StatusConditions(), ApplierClaimConditionInControl)
}

// liveOperation reads the applier claim's status.operation uncached: a branch worker writes it just before
// it resumes the Kustomization, and a cached read could still show the operation before it.
func (r *ApplierClaimReconciler) liveOperation(
	ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim,
) (*configbutleraiv1alpha3.ApplyOperation, error) {
	live := &configbutleraiv1alpha3.FluxClaim{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(claim), live); err != nil {
		return nil, err
	}
	return live.Status.Operation, nil
}

// uncachedReader is APIReader, or the client when none is set.
func (r *ApplierClaimReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// fluxClient is the Flux client of the applier claim's cluster.
func (r *ApplierClaimReconciler) fluxClient(
	ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim,
) (*applier.Flux, error) {
	dynamicClient, err := r.ApplierClients(ctx, claim.KubeProviderKey().String())
	if err != nil {
		return nil, err
	}
	return applier.NewFlux(dynamicClient), nil
}

// releaseDeadline is how long a retiring applier claim tries to hand its applier back, counted from when
// its InControl condition turned Releasing, before it lets go anyway with a Warning Event that says what
// the applier is left as. A Kustomization's suspend is handled once the apply running then ends, so
// the deadline is longer than an apply usually takes; a kustomize-controller that is down never
// handles it, and the deletion must not wait on that forever.
const releaseDeadline = 10 * time.Minute

// retire hands an applier claim's applier back, and then lets go of it: an applier claim being deleted (check
// nil), or one no eligible GitTarget references any more (check is what the reconcile found). A
// FluxClaim suspends its Kustomization once more if it is resumed (a branch worker's apply may
// still be running), waits until that suspend's token is handled, and releases spec.suspend; an
// ArgoCDClaim gives its Application's automated sync switch up (releaseStep). Neither hands back
// what an applier claim in line takes over. Until that is done the applier claim stays the holder,
// InControl=False (Releasing), so no successor acts on the applier meanwhile. An applier that is gone has nothing
// to hand back; a cluster no client reaches any more since its KubeProvider went (releaseClient),
// and an applier the identity may not write, are let go with a Warning Event: none should hold the
// applier claim forever. Nor does anything else past releaseDeadline. An applier claim behind another in line
// for the applier hands nothing back: the applier is that one's. Letting go removes the finalizer;
// a live applier claim then holds nothing until an eligible GitTarget references it again, which adopts
// the applier anew.
func (r *ApplierClaimReconciler) retire(
	ctx context.Context, st *reconcileStatus, claim configbutleraiv1alpha3.ApplierClaim, check *claimCheck,
) (ctrl.Result, error) {
	holder, err := applier.HeldByAnother(ctx, r.Client, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	var pending *pendingRelease
	if holder != nil {
		// An applier claim in line never held the applier: what it holds is the holder's, under the same
		// field manager, so there is nothing of this applier claim's to hand back.
		logf.FromContext(ctx).Info("the applier is held by another applier claim; handing nothing back",
			"applier", claim.ApplierKind()+" "+claim.Applier().String(),
			"holder", holder.GetNamespace()+"/"+holder.GetName())
	} else {
		successor, err := r.nextInLine(ctx, claim)
		if err != nil {
			return ctrl.Result{}, err
		}
		step, cancel := context.WithTimeout(ctx, applierCheckTimeout)
		pending = r.releaseStep(step, claim, successor)
		cancel()
	}
	if pending != nil {
		since := releasingSince(claim)
		if time.Since(since) < releaseDeadline {
			r.publishReleasing(st, claim, check, since)
			if err := st.commit(ctx); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: st.requeueAfter(pending.result.RequeueAfter)}, pending.err
		}
		r.event(claim, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"gave up handing %s %s back to %s %s after it began: %s",
			claim.ApplierKind(), claim.Applier(), applierTool(claim), releaseDeadline, pending.left)
	}
	if check == nil {
		controllerutil.RemoveFinalizer(claim, claimFinalizer(claim))
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, claim))
	}
	if err := r.setFinalizer(ctx, st, claim, false); err != nil {
		return ctrl.Result{}, err
	}
	r.letGoOfOperation(claim)
	clearPreviousSuspendState(claim)
	inControl := handedBackCondition(claim, check)
	st.setValue(ApplierClaimConditionInControl, inControl)
	st.applyReadiness(r.claimReadiness(claim, check.configured, &inControl))
	if err := st.commit(ctx); err != nil {
		return ctrl.Result{}, err
	}
	// A hand-back that a denial began and a regrant outlived adopts the applier anew at once.
	if !check.denied && check.configured.Status == metav1.ConditionTrue {
		return ctrl.Result{RequeueAfter: st.requeueAfter(applierClaimSuspendingInterval)}, nil
	}
	return ctrl.Result{RequeueAfter: st.requeueAfter(heldClaimSteadyInterval)}, nil
}

// handedBackCondition is the InControl condition of a live applier claim that has handed its applier
// back, saying what makes it hold the applier again.
func handedBackCondition(claim configbutleraiv1alpha3.ApplierClaim, check *claimCheck) conditionValue {
	switch {
	case check.denied:
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
			Message: fmt.Sprintf("the %s was handed back, and is held again once the KubeProvider's "+
				"spec.allowedApplierClaims admit the applier claim", claim.ApplierKind())}
	case check.refs.Named():
		return conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured,
			Message: fmt.Sprintf("the %s was handed back, and is held again once an eligible GitTarget "+
				"references the applier claim", claim.ApplierKind())}
	}
	return conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonNotReferenced,
		Message: fmt.Sprintf("nothing to hold: no GitTarget names this applier claim, and its %s was handed back",
			claim.ApplierKind())}
}

// clearPreviousSuspendState drops what an applier claim recorded of its applier before it held it, once it
// has handed it back (or on to a successor): a later adoption records anew.
func clearPreviousSuspendState(claim configbutleraiv1alpha3.ApplierClaim) {
	claim.SetPreviousSuspendState("")
}

// claimFinalizer is the finalizer of applier claim's kind: the one it carries while it holds its
// applier.
func claimFinalizer(claim configbutleraiv1alpha3.ApplierClaim) string {
	if claim.ClaimKind() == configbutleraiv1alpha3.ApplierClaimKindArgoCD {
		return configbutleraiv1alpha3.ArgoCDClaimFinalizer
	}
	return configbutleraiv1alpha3.FluxClaimFinalizer
}

// applierTool is the tool an applier claim's applier belongs to, which a hand-back hands it back to.
func applierTool(claim configbutleraiv1alpha3.ApplierClaim) string {
	if claim.ClaimKind() == configbutleraiv1alpha3.ApplierClaimKindArgoCD {
		return "Argo CD"
	}
	return "Flux"
}

// publishReleasing sets InControl=False (Releasing, or Revoking for a hand-back a denial began) and the
// trio on an applier claim whose hand-back is not done, its deadline counted from since. The condition
// is written anew when no hand-back was under way before, so its lastTransitionTime says when the
// release began.
func (r *ApplierClaimReconciler) publishReleasing(
	st *reconcileStatus, claim configbutleraiv1alpha3.ApplierClaim, check *claimCheck, since time.Time,
) {
	why, reason := "the applier claim is being deleted", ReasonReleasing
	switch {
	case check != nil && (check.denied || revoking(claim)):
		why, reason = "the KubeProvider's spec.allowedApplierClaims no longer admit the applier claim", ReasonRevoking
	case check != nil && check.refs.Named():
		why = "no GitTarget naming the applier claim is eligible any more"
	case check != nil:
		why = "no GitTarget names the applier claim any more"
	}
	inControl := conditionValue{Status: metav1.ConditionFalse, Reason: reason, Message: fmt.Sprintf(
		"%s: handing %s %s back to %s, by %s at the latest", why, claim.ApplierKind(), claim.Applier(),
		applierTool(claim), since.Add(releaseDeadline).UTC().Format(time.RFC3339))}
	if !handingBack(findCondition(*claim.StatusConditions(), ApplierClaimConditionInControl)) {
		st.remove(ApplierClaimConditionInControl)
	}
	st.setValue(ApplierClaimConditionInControl, inControl)
	if check == nil {
		rd := newReadiness("", claim.ClaimKind()+" is not stalled")
		rd.progressing(inControl.Status, inControl.Reason, inControl.Message)
		st.applyReadiness(rd)
		return
	}
	st.applyReadiness(r.claimReadiness(claim, check.configured, &inControl))
}

// revoking reports whether the applier claim is handing its applier back because its
// KubeProvider's grants refused it (InControl=False, Revoking): such a hand-back finishes even after a
// regrant, and the applier is adopted anew once it has.
func revoking(claim configbutleraiv1alpha3.ApplierClaim) bool {
	inControl := findCondition(*claim.StatusConditions(), ApplierClaimConditionInControl)
	return inControl != nil && inControl.Reason == ReasonRevoking
}

// handingBack reports whether an InControl condition says a hand-back is under way: Releasing, or
// Revoking. A denial arriving during a release continues the same hand-back, with the same deadline.
func handingBack(inControl *metav1.Condition) bool {
	return inControl != nil && (inControl.Reason == ReasonReleasing || inControl.Reason == ReasonRevoking)
}

// releasingSince is when the applier claim's InControl condition turned Releasing or Revoking, or now
// when it has not.
func releasingSince(claim configbutleraiv1alpha3.ApplierClaim) time.Time {
	inControl := findCondition(*claim.StatusConditions(), ApplierClaimConditionInControl)
	if !handingBack(inControl) || inControl.LastTransitionTime.IsZero() {
		return time.Now()
	}
	return inControl.LastTransitionTime.Time
}

// letGoOfOperation completes an operation a retired applier claim still records in flight, when this
// process does not follow it: nobody would complete it, and its applier is handed back.
func (r *ApplierClaimReconciler) letGoOfOperation(claim configbutleraiv1alpha3.ApplierClaim) {
	operation := claim.Operation()
	if operation == nil || operation.Phase.Completed() {
		return
	}
	ref := git.ApplierClaimRef{Kind: claim.ClaimKind(), Namespace: claim.GetNamespace(), Name: claim.GetName()}
	if operation.Token != "" && r.Follower != nil && r.Follower.Following(ref, operation.Token) {
		return
	}
	completed := operation.DeepCopy()
	finished := metav1.NewTime(time.Now())
	completed.Phase, completed.FinishedAt = configbutleraiv1alpha3.ApplyOperationFailed, &finished
	completed.Message = fmt.Sprintf("the applier claim handed its %s back before the apply was seen to end",
		claim.ApplierKind())
	claim.SetOperation(completed, claim.RepairPending())
	r.unfollowed.forget(claim, "")
}

// setFinalizer puts the applier claim's finalizer on it, or takes it off, and carries the metadata that
// write left into the status session, so the status patch that follows is made against it. The
// finalizer is what hands the applier back, so it is in place before the first write that holds it,
// and off once the applier claim has handed the applier back or over.
func (r *ApplierClaimReconciler) setFinalizer(
	ctx context.Context, st *reconcileStatus, claim configbutleraiv1alpha3.ApplierClaim, present bool,
) error {
	update, ok := claim.DeepCopyObject().(configbutleraiv1alpha3.ApplierClaim)
	if !ok {
		return fmt.Errorf("a copy of %s %s/%s is no applier claim", claim.ClaimKind(), claim.GetNamespace(),
			claim.GetName())
	}
	changed := controllerutil.RemoveFinalizer(update, claimFinalizer(claim))
	if present {
		changed = controllerutil.AddFinalizer(update, claimFinalizer(claim))
	}
	if !changed {
		return nil
	}
	if err := r.Update(ctx, update); err != nil {
		return err
	}
	claim.SetFinalizers(update.GetFinalizers())
	claim.SetResourceVersion(update.GetResourceVersion())
	st.metadataWritten()
	return nil
}

// nextInLine is the applier claim that takes the applier over once claim is gone, or nil when no
// applier claim will: one in line that is not being deleted, and whose Configured is True or False only
// because claim holds the applier (HeldByAnotherClaim is checked last, so such an applier claim
// passed every other check).
func (r *ApplierClaimReconciler) nextInLine(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim,
) (configbutleraiv1alpha3.ApplierClaim, error) {
	others, err := applier.ClaimsOfApplier(ctx, r.Client, claim)
	if err != nil {
		return nil, err
	}
	var next configbutleraiv1alpha3.ApplierClaim
	var nextCreated metav1.Time
	for _, other := range others {
		configured := findCondition(*other.StatusConditions(), ApplierClaimConditionConfigured)
		takesOver := configured != nil && (configured.Status == metav1.ConditionTrue ||
			configured.Reason == applier.ReasonHeldByAnotherClaim)
		created := other.GetCreationTimestamp()
		if takesOver && other.GetDeletionTimestamp().IsZero() && (next == nil || created.Before(&nextCreated)) {
			next, nextCreated = other, created
		}
	}
	return next, nil
}

// pendingRelease is a hand-back that is not done: what to retry it on, and what the applier is
// left as should the applier claim go now.
type pendingRelease struct {
	result ctrl.Result
	err    error
	left   string
}

// releaseStep takes the hand-back of applier claim's applier one step, and returns nil once the applier claim
// may go.
func (r *ApplierClaimReconciler) releaseStep(
	ctx context.Context, claim, successor configbutleraiv1alpha3.ApplierClaim,
) *pendingRelease {
	if successor != nil {
		if err := r.passPreviousSuspendState(ctx, claim, successor); err != nil {
			return &pendingRelease{err: err, left: fmt.Sprintf("it is left held, and %s %s/%s, which holds it "+
				"next, does not know how it stood before it was held (%v)", successor.ClaimKind(),
				successor.GetNamespace(), successor.GetName(), err)}
		}
	}
	switch t := claim.(type) {
	case *configbutleraiv1alpha3.FluxClaim:
		return r.releaseKustomization(ctx, t, successor)
	case *configbutleraiv1alpha3.ArgoCDClaim:
		return r.releaseAutomatedSync(ctx, t, successor)
	}
	return nil
}

// releaseKustomization takes a FluxClaim's hand-back one step. With a successor (nextInLine), the
// Kustomization is not released but left suspended, its suspend handled, for it: released, Flux
// would apply it at once, unchecked against the branch, before the successor's adoption could
// suspend it again.
func (r *ApplierClaimReconciler) releaseKustomization(
	ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim, successor configbutleraiv1alpha3.ApplierClaim,
) *pendingRelease {
	dynamicClient, err := r.releaseClient(ctx, claim)
	if err != nil {
		return &pendingRelease{
			err: err, left: fmt.Sprintf("its cluster cannot be reached (%v), so it is left as it was", err),
		}
	}
	if dynamicClient == nil {
		return nil
	}
	flux := applier.NewFlux(dynamicClient)
	ref := claim.Applier()
	suspending := &pendingRelease{
		result: ctrl.Result{RequeueAfter: applierClaimSuspendingInterval},
		left: "it is suspended, and has not handled the suspend yet (is kustomize-controller running?); " +
			"`flux resume` resumes it",
	}
	suspension, err := flux.Suspension(ctx, ref)
	switch {
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return nil
	case err != nil:
		return r.releaseFailed(claim, err, fmt.Sprintf("it cannot be read (%v), so it is left as it was", err))
	case !suspension.Held:
		return nil
	case !suspension.Suspended:
		if _, err := flux.Suspend(ctx, ref); err != nil {
			return r.releaseFailed(claim, err, fmt.Sprintf("it is resumed, and suspending it failed (%v), so "+
				"GitOps Reverser's field manager still holds spec.suspend", err))
		}
		return suspending
	case !suspension.Handled:
		return suspending
	}
	if successor != nil {
		r.event(claim, corev1.EventTypeNormal, EventReasonReleased,
			"left Kustomization %s suspended for %s %s/%s, which holds it next", ref, successor.ClaimKind(),
			successor.GetNamespace(), successor.GetName())
		return nil
	}
	return r.handBackKustomization(ctx, flux, claim)
}

// handBackKustomization releases a FluxClaim's Kustomization, suspended and with its suspend
// handled, as status.previousSuspendState recorded it, with a Released Event that says how it is left.
func (r *ApplierClaimReconciler) handBackKustomization(
	ctx context.Context, flux *applier.Flux, claim *configbutleraiv1alpha3.FluxClaim,
) *pendingRelease {
	ref := claim.Applier()
	wasSuspended := claim.Status.PreviousSuspendState == configbutleraiv1alpha3.SuspendStateSuspended
	keeper, err := flux.Release(ctx, ref, wasSuspended)
	if err != nil {
		return r.releaseFailed(claim, err, fmt.Sprintf("it is suspended, and releasing spec.suspend failed (%v); "+
			"`flux resume` resumes it", err))
	}
	if wasSuspended {
		r.event(claim, corev1.EventTypeNormal, EventReasonReleased,
			"handed Kustomization %s back to Flux: it stays suspended, as it was before GitOps Reverser held it; "+
				"`flux resume` resumes it", ref)
		return nil
	}
	if keeper != "" {
		r.event(claim, corev1.EventTypeNormal, EventReasonReleased,
			"handed Kustomization %s back to Flux: it stays suspended, because field manager %q also sets "+
				"spec.suspend", ref, keeper)
		return nil
	}
	r.event(claim, corev1.EventTypeNormal, EventReasonReleased,
		"handed Kustomization %s back to Flux: it is resumed, and applies on its own again", ref)
	return nil
}

// releaseFailed is a hand-back step that failed with err: one the identity may not make lets the
// applier claim go with a Warning Event, since a retry cannot change it, and any other is retried.
func (r *ApplierClaimReconciler) releaseFailed(
	claim configbutleraiv1alpha3.ApplierClaim, err error, left string,
) *pendingRelease {
	if apierrors.IsForbidden(err) {
		r.event(claim, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"could not hand %s %s back to %s: %v", claim.ApplierKind(), claim.Applier(), applierTool(claim), err)
		return nil
	}
	return &pendingRelease{err: err, left: left}
}

// releaseClient is the client of the cluster a retiring applier claim hands its applier back in, or nil
// when there is nothing to reach: no cluster clients are wired, or the KubeProvider is gone and no
// client reaches its cluster any more, which lets the applier claim go with a Warning Event. A
// KubeProvider that is gone does not end the hand-back by itself: an inCluster one named the
// operator's own cluster, which may still answer, and a remote one may still be reached through the
// client it left.
func (r *ApplierClaimReconciler) releaseClient(
	ctx context.Context, claim configbutleraiv1alpha3.ApplierClaim,
) (dynamic.Interface, error) {
	if r.ApplierClients == nil {
		return nil, nil //nolint:nilnil // nothing to reach is no error
	}
	provider := &configbutleraiv1alpha3.KubeProvider{}
	err := r.Get(ctx, claim.KubeProviderKey(), provider)
	gone := apierrors.IsNotFound(err)
	if err != nil && !gone {
		return nil, err
	}
	dynamicClient, err := r.ApplierClients(ctx, claim.KubeProviderKey().String())
	switch {
	case err != nil && gone:
		r.event(claim, corev1.EventTypeWarning, EventReasonReleaseFailed,
			"could not hand %s %s back to %s: its KubeProvider %s is gone; %s", claim.ApplierKind(),
			claim.Applier(), applierTool(claim), claim.KubeProviderKey(), leftHeld(claim))
		return nil, nil //nolint:nilnil // nothing to reach, and the Event says so
	case err != nil:
		return nil, fmt.Errorf("reach the cluster of %s %s to hand it back: %w", claim.ApplierKind(),
			claim.Applier(), err)
	}
	return dynamicClient, nil
}

// leftHeld says what an applier an applier claim could not hand back is left as, and how a human hands it
// back.
func leftHeld(claim configbutleraiv1alpha3.ApplierClaim) string {
	if claim.ClaimKind() == configbutleraiv1alpha3.ApplierClaimKindArgoCD {
		return "its automated sync is left off, and switching it on in Argo CD has it sync on its own again"
	}
	return "it is left suspended, and `flux resume` resumes it"
}

// event records an Event on the applier claim, when there is a recorder.
func (r *ApplierClaimReconciler) event(object client.Object, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, messageFmt, args...)
	}
}

// publishAlsoAppliedBy sets the applier claim's status.source.alsoAppliedBy: the objects applying its
// GitRepository in the applier's cluster, other than its own Kustomization, that no FluxClaim on the
// same KubeProvider holds. A listing that fails publishes none. Information only (decision 2): it never
// affects Ready.
func (r *ApplierClaimReconciler) publishAlsoAppliedBy(ctx context.Context, claim *configbutleraiv1alpha3.FluxClaim) {
	source := claim.Status.Source
	if source == nil || r.ApplierClients == nil {
		return
	}
	source.AlsoAppliedBy = nil
	ctx, cancel := context.WithTimeout(ctx, applierCheckTimeout)
	defer cancel()
	flux, err := r.fluxClient(ctx, claim)
	if err != nil {
		return
	}
	appliedBy, err := flux.AppliedBy(ctx, k8stypes.NamespacedName{
		Namespace: source.GitRepository.Namespace, Name: source.GitRepository.Name,
	})
	if err != nil {
		logf.FromContext(ctx).V(1).Info("cannot list what applies the GitRepository",
			"gitRepository", source.GitRepository.Namespace+"/"+source.GitRepository.Name, "error", err.Error())
		return
	}
	others, err := r.kustomizationsInControl(ctx, claim.KubeProviderKey())
	if err != nil {
		return
	}
	for _, applying := range appliedBy {
		key := k8stypes.NamespacedName{Namespace: applying.Namespace, Name: applying.Name}
		itself := key == claim.Applier()
		if applying.Kind == configbutleraiv1alpha3.ApplierKindKustomization && (itself || others[key]) {
			continue
		}
		source.AlsoAppliedBy = append(source.AlsoAppliedBy, applying)
	}
}

// kustomizationsInControl is every Kustomization a FluxClaim on the KubeProvider is in control of: one
// whose InControl condition is True. An applier claim that is not Configured (SuspendOwnedElsewhere),
// or whose Kustomization has not handled its suspend yet, is not in control of it: Flux may still
// apply a new artifact on its own.
func (r *ApplierClaimReconciler) kustomizationsInControl(
	ctx context.Context,
	cluster k8stypes.NamespacedName,
) (map[k8stypes.NamespacedName]bool, error) {
	var claims configbutleraiv1alpha3.FluxClaimList
	if err := r.List(ctx, &claims); err != nil {
		return nil, err
	}
	inControl := map[k8stypes.NamespacedName]bool{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.DeletionTimestamp.IsZero() && claim.KubeProviderKey() == cluster && wasInControl(claim) {
			inControl[claim.Applier()] = true
		}
	}
	return inControl, nil
}
