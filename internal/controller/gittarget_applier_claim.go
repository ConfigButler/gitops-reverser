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

// GitTargetConditionApplierClaimReady reports whether spec.applierClaimRef names an applier claim that is
// Ready and whose applier applies this target's repository and branch. Present only while
// spec.applierClaimRef is set.
const GitTargetConditionApplierClaimReady = "ApplierClaimReady"

// ReasonApplierClaimNotFound is ApplierClaimReady=False for a GitTarget naming an applier claim that does
// not exist in its namespace.
const ReasonApplierClaimNotFound = "ApplierClaimNotFound"

// applierClaimVerdict is the ApplierClaimReady value, and whether waiting will not clear it.
type applierClaimVerdict struct {
	value   conditionValue
	stalled bool
}

// observeApplierClaim publishes the ApplierClaimReady condition and returns it for the readiness
// trio. present is false when the target names no applier claim, in which case the condition is removed.
//
// It reads the applier claim from the cache; the applier claim's own reconciler checks its applier. What is
// left to check here is what only the GitTarget knows: the repository and branch it writes.
func (r *GitTargetReconciler) observeApplierClaim(
	ctx context.Context,
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	dest gitDestination,
) (applierClaimVerdict, bool) {
	ref := target.Spec.ApplierClaimRef
	if ref == nil {
		st.remove(GitTargetConditionApplierClaimReady)
		return applierClaimVerdict{}, false
	}
	verdict := r.applierClaimVerdict(ctx, target, dest)
	st.setValue(GitTargetConditionApplierClaimReady, verdict.value)
	return verdict, true
}

func (r *GitTargetReconciler) applierClaimVerdict(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	dest gitDestination,
) applierClaimVerdict {
	ref := target.Spec.ApplierClaimRef
	name := fmt.Sprintf("%s %s/%s", ref.Kind, target.Namespace, ref.Name)
	applierClaim, err := git.ResolveApplierClaim(ctx, r.Client, target)
	switch {
	case err != nil:
		return applierClaimVerdict{value: keptCondition(target.Status.Conditions, GitTargetConditionApplierClaimReady,
			fmt.Sprintf("Cannot read %s: %v", name, err))}
	case applierClaim == nil:
		return applierClaimVerdict{value: conditionValue{
			Status:  metav1.ConditionFalse,
			Reason:  ReasonApplierClaimNotFound,
			Message: name + " not found",
		}}
	}

	ready := findCondition(*applierClaim.StatusConditions(), ConditionTypeReady)
	switch {
	case ready == nil || ready.ObservedGeneration != applierClaim.GetGeneration() || ready.Status == metav1.ConditionUnknown:
		reason, message := conditionReasonMessage(ready, ReasonProgressing, "has not been checked yet")
		return applierClaimVerdict{value: conditionValue{
			Status: metav1.ConditionUnknown, Reason: reason, Message: name + ": " + message,
		}}
	case ready.Reason == applier.ReasonNotReferenced:
		// The applier claim's reconcile has not seen this reference yet: naming an applier claim does not change its
		// generation, so the generation check above cannot tell.
		return applierClaimVerdict{value: conditionValue{
			Status: metav1.ConditionUnknown, Reason: applier.ReasonNotReferenced,
			Message: name + " has not observed that this GitTarget names it yet",
		}}
	case ready.Status == metav1.ConditionFalse:
		return applierClaimVerdict{
			value: conditionValue{
				Status:  metav1.ConditionFalse,
				Reason:  ready.Reason,
				Message: name + ": " + ready.Message,
			},
			stalled: conditionIsTrue(*applierClaim.StatusConditions(), ConditionTypeStalled),
		}
	}

	if dest.provider != nil && dest.branch != nil {
		want := applier.Binding{RepositoryURL: dest.provider.Spec.URL, Branch: dest.branch.Spec.Branch}
		if mismatch := applier.BindingMismatch(applierClaim, want); mismatch != "" {
			return applierClaimVerdict{
				value: conditionValue{
					Status:  metav1.ConditionFalse,
					Reason:  applier.ReasonSourceMismatch,
					Message: mismatch,
				},
				stalled: true,
			}
		}
	}
	return applierClaimVerdict{value: conditionValue{
		Status: metav1.ConditionTrue,
		Reason: ReasonSucceeded,
		Message: fmt.Sprintf("%s holds %s %s, which applies this repository and branch",
			name, applierClaim.ApplierKind(), applierClaim.Applier()),
	}}
}

// applierClaimReadiness contributes ApplierClaimReady to the trio. An applier claim that applies another
// repository, or one stalled itself, needs a human; one that is missing may be on its way, and one
// that is not Ready yet is still settling.
func applierClaimReadiness(rd *readiness, verdict applierClaimVerdict, present bool) {
	if !present {
		return
	}
	value := verdict.value
	rd.stalledIf(value.Status == metav1.ConditionFalse && verdict.stalled, value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionFalse, metav1.ConditionFalse, value.Reason, value.Message)
	rd.progressingIf(value.Status == metav1.ConditionUnknown, metav1.ConditionUnknown, value.Reason, value.Message)
}
