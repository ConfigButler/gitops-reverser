// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// GitTargetConditionRetargeting is True while spec's destination and
	// status.observedDestination disagree: the folder named by observedDestination is
	// being abandoned and the one named by spec is being built.
	GitTargetConditionRetargeting = "Retargeting"

	// GitTargetReasonDestinationChanged marks the teardown of the old materialization.
	GitTargetReasonDestinationChanged = "DestinationChanged"
	// GitTargetReasonDestinationSettled marks a completed retarget, or a first
	// materialization.
	GitTargetReasonDestinationSettled = "DestinationSettled"
)

// specDestination is the destination the spec asks for, in the shape status records: the
// path as the user wrote it, minus surrounding whitespace and a trailing slash. It is NOT
// normalizeGitTargetPath's form, which prefixes a "/" for prefix comparison and would
// leak an internal comparison key into a user-facing status field.
func specDestination(target *configbutleraiv1alpha3.GitTarget) configbutleraiv1alpha3.GitTargetDestination {
	return configbutleraiv1alpha3.GitTargetDestination{
		Branch: target.Spec.Branch,
		Path:   displayGitTargetPath(target.Spec.Path),
	}
}

// displayGitTargetPath trims a path to the form status records. "." — the deliberate
// repository-root choice — survives, because path.Clean keeps it.
func displayGitTargetPath(p string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(p), "/")
	if trimmed == "" {
		return "."
	}
	return trimmed
}

// sameDestination compares two destinations the way the filesystem does: "apps" and
// "apps/" are the same folder, so a trailing slash is never a move.
func sameDestination(a, b configbutleraiv1alpha3.GitTargetDestination) bool {
	return a.Branch == b.Branch && normalizeGitTargetPath(a.Path) == normalizeGitTargetPath(b.Path)
}

// destinationMoved reports whether the GitTarget has a materialization at a destination
// other than the one its spec now asks for. A target that has never materialized
// (observedDestination absent) has not moved: it has not arrived anywhere yet.
func destinationMoved(target *configbutleraiv1alpha3.GitTarget) bool {
	observed := target.Status.ObservedDestination
	if observed == nil {
		return false
	}
	return !sameDestination(*observed, specDestination(target))
}

// retargetAlreadyTornDown reports whether the old materialization was already torn down
// for the CURRENT generation. It is generation-scoped rather than a plain "is Retargeting
// true?" so that a second destination change, arriving while the first retarget is still
// building, tears down again instead of quietly continuing to build the first one.
func retargetAlreadyTornDown(target *configbutleraiv1alpha3.GitTarget) bool {
	c := conditionByType(target.Status.Conditions, GitTargetConditionRetargeting)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == target.Generation
}

// beginRetarget tears the old materialization down before anything is validated or written
// at the new destination.
//
// Teardown happens FIRST, and that ordering is the whole safety property. The writer reads
// spec.path fresh on every write, while the branch worker is bound to the branch the event
// stream was registered against. So between a spec change and a completed retarget, a live
// event would otherwise be written to the NEW path on the OLD branch. Cancelling the
// watches and unregistering the stream means nothing is written anywhere until the new
// destination has passed its own validation gate.
//
// It is idempotent per generation: a reconcile that finds the teardown already done for
// this generation skips it and lets the rebuild proceed.
func (r *GitTargetReconciler) beginRetarget(
	ctx context.Context,
	target *configbutleraiv1alpha3.GitTarget,
	log logr.Logger,
) error {
	abandoned := *target.Status.ObservedDestination
	want := specDestination(target)

	log.Info("GitTarget destination changed; tearing down the old materialization",
		"from", destinationString(abandoned), "to", destinationString(want))

	if r.EventRouter != nil {
		gitDest := types.NewResourceReference(target.Name, target.Namespace).WithUID(string(target.UID))
		// Release the old branch worker so evaluateWorkerWiringGate binds a fresh stream to
		// the new branch's worker.
		r.EventRouter.UnregisterGitTargetEventStream(types.NewResourceReference(target.Name, target.Namespace))
		if r.EventRouter.WatchManager != nil {
			// Drops the watches AND the durable resume cursors: the new folder must be built
			// from a full replay, not resumed into mid-stream.
			if err := r.EventRouter.WatchManager.RetargetGitTarget(ctx, gitDest); err != nil {
				return fmt.Errorf("retarget %s/%s: %w", target.Namespace, target.Name, err)
			}
		}
	}

	// The push time belonged to the abandoned folder.
	target.Status.LastPushTime = nil
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionTrue,
		GitTargetReasonDestinationChanged,
		fmt.Sprintf("moving from %s to %s; the old folder is left in place as unmanaged content "+
			"and must be removed by hand if it is no longer wanted",
			destinationString(abandoned), destinationString(want)))
	return nil
}

// settleDestination records that the current materialization now belongs to the spec's
// destination. It is called only once the new folder actually holds a snapshot the
// acceptance gate approved — that is what keeps the invariant honest: a successful snapshot
// is valid for the destination recorded in status.observedDestination.
func (r *GitTargetReconciler) settleDestination(target *configbutleraiv1alpha3.GitTarget, log logr.Logger) {
	want := specDestination(target)
	observed := target.Status.ObservedDestination

	switch {
	case observed == nil:
		r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
			GitTargetReasonDestinationSettled,
			"materialized at "+destinationString(want))
	case !sameDestination(*observed, want):
		abandoned := *observed
		log.Info("GitTarget retarget complete; the old folder is now unmanaged Git content",
			"abandoned", destinationString(abandoned), "current", destinationString(want))
		r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
			GitTargetReasonDestinationSettled,
			fmt.Sprintf("materialized at %s; %s was abandoned and is now unmanaged Git content — "+
				"remove it by hand if it is no longer wanted",
				destinationString(want), destinationString(abandoned)))
	default:
		r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionFalse,
			GitTargetReasonDestinationSettled,
			"materialized at "+destinationString(want))
	}

	target.Status.ObservedDestination = &want
}

// markRetargetingUnknown records that the destination could not be evaluated because a
// control-plane gate blocked the reconcile before the data plane ran.
func (r *GitTargetReconciler) markRetargetingUnknown(target *configbutleraiv1alpha3.GitTarget, message string) {
	// A retarget already under way keeps its True: the old materialization really is torn
	// down, and a blocked gate does not put it back.
	if retargetAlreadyTornDown(target) {
		return
	}
	r.setCondition(target, GitTargetConditionRetargeting, metav1.ConditionUnknown,
		GitTargetReasonNotChecked, message)
}

func destinationString(d configbutleraiv1alpha3.GitTargetDestination) string {
	return d.Branch + ":" + d.Path
}
