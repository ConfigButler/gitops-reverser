// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// GitTargetConditionApplyTriggerReady reports whether spec.applyTriggerRef names a trigger that is
// Ready and whose applier applies this target's repository and branch. Present only while
// spec.applyTriggerRef is set.
const GitTargetConditionApplyTriggerReady = "ApplyTriggerReady"

// ReasonApplyTriggerNotFound is ApplyTriggerReady=False for a GitTarget naming a trigger that does
// not exist in its namespace.
const ReasonApplyTriggerNotFound = "ApplyTriggerNotFound"

// applyTriggerVerdict is the ApplyTriggerReady value, and whether waiting will not clear it.
type applyTriggerVerdict struct {
	value   conditionValue
	stalled bool
}

// observeApplyTrigger publishes the ApplyTriggerReady condition and returns it for the readiness
// trio. present is false when the target names no trigger, in which case the condition is removed.
//
// It reads the trigger from the cache; the trigger's own reconciler checks its applier. What is
// left to check here is what only the GitTarget knows: the repository and branch it writes.
func (r *GitTargetReconciler) observeApplyTrigger(
	ctx context.Context,
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	dest gitDestination,
) (applyTriggerVerdict, bool) {
	ref := target.Spec.ApplyTriggerRef
	if ref == nil {
		st.remove(GitTargetConditionApplyTriggerReady)
		return applyTriggerVerdict{}, false
	}
	verdict := r.applyTriggerVerdict(ctx, target, dest)
	st.setValue(GitTargetConditionApplyTriggerReady, verdict.value)
	return verdict, true
}

func (r *GitTargetReconciler) applyTriggerVerdict(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	dest gitDestination,
) applyTriggerVerdict {
	ref := target.Spec.ApplyTriggerRef
	name := fmt.Sprintf("%s %s/%s", ref.Kind, target.Namespace, ref.Name)
	trigger, err := git.ResolveApplyTrigger(ctx, r.Client, target)
	switch {
	case err != nil:
		return applyTriggerVerdict{value: keptCondition(target.Status.Conditions, GitTargetConditionApplyTriggerReady,
			fmt.Sprintf("Cannot read %s: %v", name, err))}
	case trigger == nil:
		return applyTriggerVerdict{value: conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  ReasonApplyTriggerNotFound,
			Message: name + " not found",
		}}
	}

	ready := findCondition(*trigger.StatusConditions(), ConditionTypeReady)
	switch {
	case ready == nil || ready.ObservedGeneration != trigger.GetGeneration() || ready.Status == metav1.ConditionUnknown:
		reason, message := conditionReasonMessage(ready, ReasonProgressing, "has not been checked yet")
		return applyTriggerVerdict{value: conditionValue{
			Status: metav1.ConditionUnknown, Reason: reason, Message: name + ": " + message,
		}}
	case ready.Reason == applier.ReasonNotReferenced:
		// The trigger's reconcile has not seen this reference yet: naming a trigger does not change its
		// generation, so the generation check above cannot tell.
		return applyTriggerVerdict{value: conditionValue{
			Status: metav1.ConditionUnknown, Reason: applier.ReasonNotReferenced,
			Message: name + " has not observed that this GitTarget names it yet",
		}}
	case ready.Status == metav1.ConditionFalse:
		return applyTriggerVerdict{
			value: conditionValue{
				Status:  metav1.ConditionFalse,
				Reason:  ready.Reason,
				Message: name + ": " + ready.Message,
			},
			stalled: conditionIsTrue(*trigger.StatusConditions(), ConditionTypeStalled),
		}
	}

	if dest.provider != nil && dest.branch != nil {
		want := applier.Binding{RepositoryURL: dest.provider.Spec.URL, Branch: dest.branch.Spec.Branch}
		if mismatch := applier.BindingMismatch(trigger, want); mismatch != "" {
			return applyTriggerVerdict{
				value: conditionValue{
					Status:  metav1.ConditionFalse,
					Reason:  applier.ReasonSourceMismatch,
					Message: mismatch,
				},
				stalled: true,
			}
		}
	}
	return applyTriggerVerdict{value: conditionValue{
		Status: metav1.ConditionTrue,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s holds %s %s, which applies this repository and branch",
			name, trigger.ApplierKind(), trigger.Applier()),
	}}
}

// applyTriggerReadiness contributes ApplyTriggerReady to the trio. A trigger that applies another
// repository, or one stalled itself, needs a human; one that is missing may be on its way, and one
// that is not Ready yet is still settling.
func applyTriggerReadiness(rd *readiness, verdict applyTriggerVerdict, present bool) {
	if !present {
		return
	}
	value := verdict.value
	rd.stalledIf(value.Status == metav1.ConditionFalse && verdict.stalled, value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionFalse, metav1.ConditionFalse, value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionUnknown, metav1.ConditionUnknown, value.Reason, value.Message)
}
