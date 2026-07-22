// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// conditionValue is a condition's payload without its type: the (status, reason, message) triple
// that every gate produces and every writer consumes.
//
// It exists so a gate can RETURN its verdict instead of applying it. Passing the triple around as
// three positional strings is what made "who writes Ready?" unanswerable — every gate had to call a
// setter, so the answer was always "whoever ran last". See readiness.
type conditionValue struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// conditionReasonMessage reads a condition's reason and message, substituting the given defaults
// for whichever of the two the condition leaves empty.
func conditionReasonMessage(condition *metav1.Condition, defaultReason, defaultMessage string) (string, string) {
	if condition == nil {
		return defaultReason, defaultMessage
	}
	reason, message := condition.Reason, condition.Message
	if reason == "" {
		reason = defaultReason
	}
	if message == "" {
		message = defaultMessage
	}
	return reason, message
}

// reasonUnspecified is the placeholder for a condition written with an empty reason. The CRD schema
// requires a reason, so an empty one is rejected with a 422 that fails the whole status write and
// hides whatever the condition was trying to report. A visibly wrong reason is strictly better.
const reasonUnspecified = "Unspecified"

// reconcileStatus is the per-reconcile status session: it captures the object's status as read,
// collects every condition the reconcile writes, and commits the difference exactly once.
//
// It replaces the five near-identical `updateStatusWithRetry` implementations that each controller
// used to carry, and with them three defects those copies shared:
//
//   - They wrote unconditionally. A full-object `Status().Update` on every pass bumps
//     resourceVersion, which fires an Update watch event, which re-enqueues the object — every
//     reconcile cost roughly two. commit() sends nothing at all when nothing changed.
//   - They did read-modify-write: re-Get the object, overwrite its whole status, Update. That
//     stamps the freshly-read object with an observedGeneration computed from the generation this
//     reconcile actually saw, which may already be stale — kstatus then reads Current for a spec
//     nobody looked at. commit() patches with optimistic concurrency instead, so a spec that moved
//     under us loses the write rather than mislabelling it.
//   - They retried on conflict with an exponential backoff. A conflict means somebody else wrote
//     the object, and that write already enqueued us; re-running the reconcile on fresh data is
//     both simpler and more correct than replaying a status computed from stale data.
type reconcileStatus struct {
	client   client.Client
	recorder record.EventRecorder

	object client.Object
	// before is the object as read at the top of the reconcile; the patch is computed against it.
	before client.Object
	// beforeConditions is the persisted condition set, kept for the Event on a Ready transition.
	beforeConditions []metav1.Condition
	// conditions points at the live status condition slice of object, which set() rewrites.
	conditions *[]metav1.Condition
}

// beginStatus opens the status session. Call it once, immediately after the object is read and
// before any condition is written.
func beginStatus(
	c client.Client,
	recorder record.EventRecorder,
	object client.Object,
	conditions *[]metav1.Condition,
) *reconcileStatus {
	before, _ := object.DeepCopyObject().(client.Object)
	return &reconcileStatus{
		client:           c,
		recorder:         recorder,
		object:           object,
		before:           before,
		beforeConditions: append([]metav1.Condition(nil), *conditions...),
		conditions:       conditions,
	}
}

// set writes one condition, pinned to the generation this reconcile observed.
func (s *reconcileStatus) set(conditionType string, status metav1.ConditionStatus, reason, message string) {
	if reason == "" {
		reason = reasonUnspecified
	}
	*s.conditions = upsertCondition(
		*s.conditions, conditionType, status, reason, message, s.object.GetGeneration())
}

// setValue writes one condition from a gate's verdict.
func (s *reconcileStatus) setValue(conditionType string, value conditionValue) {
	s.set(conditionType, value.Status, value.Reason, value.Message)
}

// applyReadiness writes the kstatus trio — and is the ONLY thing that writes it. Every gate
// contributes to the readiness accumulator; the trio is derived from those contributions once,
// here, from a precedence stated in one place.
func (s *reconcileStatus) applyReadiness(r *readiness) {
	trio := r.trio()
	s.setValue(ConditionTypeReady, trio.Ready)
	s.setValue(ConditionTypeReconciling, trio.Reconciling)
	s.setValue(ConditionTypeStalled, trio.Stalled)
}

// commit writes the status subresource — or nothing, when the status is identical to what was read.
//
// Suppressing the no-op write is what breaks the self-triggering reconcile edge: a status write
// bumps resourceVersion and fires the Update watch event that `For()` turns straight back into a
// queued request. Nothing to say, nothing to write, no wake-up.
func (s *reconcileStatus) commit(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("status")

	data, err := client.MergeFrom(s.before).Data(s.object)
	if err != nil {
		return fmt.Errorf("compute status patch for %s: %w", client.ObjectKeyFromObject(s.object), err)
	}
	if string(data) == "{}" {
		return nil
	}

	// Optimistic concurrency, deliberately WITHOUT a retry loop: a conflict means the object moved
	// under this reconcile, so the status just computed describes a generation that is no longer
	// current. The write that beat us enqueued us again, so dropping this one converges on fresh
	// data instead of publishing a stale observation.
	patch := client.MergeFromWithOptions(s.before, client.MergeFromWithOptimisticLock{})
	switch err := s.client.Status().Patch(ctx, s.object, patch); {
	case err == nil:
		s.recordReadyTransition()
		return nil
	case apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		log.V(1).Info("status write skipped; object changed during reconcile",
			"object", client.ObjectKeyFromObject(s.object))
		return nil
	default:
		return fmt.Errorf("write status for %s: %w", client.ObjectKeyFromObject(s.object), err)
	}
}

// recordReadyTransition emits one Kubernetes Event per PERSISTED change of Ready.
//
// It runs after the patch rather than beside set() on purpose: a reconcile may write Ready several
// times (an early "Validating" placeholder, then the real outcome) and only the last one is ever
// stored. Announcing intermediate values would fill `kubectl describe` with states that never
// existed. Events are how a transient failure that clears before anyone looks stays visible at all,
// and they are the only thing an Event-driven alerting pipeline can route.
func (s *reconcileStatus) recordReadyTransition() {
	if s.recorder == nil {
		return
	}
	after := findCondition(*s.conditions, ConditionTypeReady)
	if after == nil {
		return
	}
	before := findCondition(s.beforeConditions, ConditionTypeReady)
	if before != nil && before.Status == after.Status && before.Reason == after.Reason {
		return
	}

	eventType := corev1.EventTypeWarning
	if after.Status == metav1.ConditionTrue {
		eventType = corev1.EventTypeNormal
	}
	s.recorder.Event(s.object, eventType, after.Reason, after.Message)
}
