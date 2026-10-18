// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// One applier, one trigger: two triggers holding the same Kustomization would both suspend it under
// the same field manager, each would take the other's resume for an external one and suspend it
// mid-apply, and deleting either would hand back the hold the other still needs. Two triggers on one
// Application would both start syncs. So exactly one trigger holds an applier, by a rule every party
// computes alike from the triggers alone (no status to race): the trigger reconciler, which holds and
// hands back only for the holder, and the dispatcher, which asks the applier only for it.

// ReasonHeldByAnotherTrigger is Configured=False on a trigger whose applier another trigger of its
// kind holds: it names the same object in the same ClusterProvider's cluster, and was created first.
// The trigger takes the applier over once that one is deleted.
const ReasonHeldByAnotherTrigger = "HeldByAnotherTrigger"

// SameApplier reports whether two triggers name one applier: the same kind of object, in the same
// ClusterProvider's cluster, with the same namespace and name.
func SameApplier(a, b v1alpha3.ApplyTrigger) bool {
	return a.TriggerKind() == b.TriggerKind() && a.ClusterProvider() == b.ClusterProvider() &&
		a.Applier() == b.Applier()
}

// sameTrigger reports whether a and b are the same trigger, by kind, namespace and name.
func sameTrigger(a, b v1alpha3.ApplyTrigger) bool {
	return a.TriggerKind() == b.TriggerKind() && a.GetNamespace() == b.GetNamespace() && a.GetName() == b.GetName()
}

// precedes reports whether a comes before b in line for an applier: created first, then by namespace,
// then by name.
func precedes(a, b v1alpha3.ApplyTrigger) bool {
	at, bt := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	switch {
	case !at.Equal(&bt):
		return at.Before(&bt)
	case a.GetNamespace() != b.GetNamespace():
		return a.GetNamespace() < b.GetNamespace()
	default:
		return a.GetName() < b.GetName()
	}
}

// Holder is the trigger that holds trigger's applier: of the triggers of its kind that name the same
// applier, the one first in line (precedes). A trigger being deleted still holds it, until it is
// gone: its finalizer is handing the applier back. Holder is trigger itself when no other comes
// before it.
func Holder(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger,
) (v1alpha3.ApplyTrigger, error) {
	triggers, err := ListApplyTriggers(ctx, reader, trigger.TriggerKind())
	if err != nil {
		return nil, err
	}
	holder := trigger
	for _, other := range triggers {
		if !sameTrigger(other, trigger) && SameApplier(other, trigger) && precedes(other, holder) {
			holder = other
		}
	}
	return holder, nil
}

// HeldByAnother is the trigger that holds trigger's applier when it is another, or nil when trigger
// holds it itself.
func HeldByAnother(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger,
) (v1alpha3.ApplyTrigger, error) {
	holder, err := Holder(ctx, reader, trigger)
	if err != nil || sameTrigger(holder, trigger) {
		return nil, err
	}
	return holder, nil
}

// TriggersOfApplier lists the triggers other than trigger that name its applier.
func TriggersOfApplier(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger,
) ([]v1alpha3.ApplyTrigger, error) {
	triggers, err := ListApplyTriggers(ctx, reader, trigger.TriggerKind())
	if err != nil {
		return nil, err
	}
	var others []v1alpha3.ApplyTrigger
	for _, other := range triggers {
		if !sameTrigger(other, trigger) && SameApplier(other, trigger) {
			others = append(others, other)
		}
	}
	return others, nil
}

// ListApplyTriggers lists the triggers of kind, with opts (everywhere without a namespace option).
func ListApplyTriggers(
	ctx context.Context, reader client.Reader, kind string, opts ...client.ListOption,
) ([]v1alpha3.ApplyTrigger, error) {
	var triggers []v1alpha3.ApplyTrigger
	switch kind {
	case v1alpha3.ApplyTriggerKindFlux:
		var list v1alpha3.FluxTriggerList
		if err := reader.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("list %ss: %w", kind, err)
		}
		for i := range list.Items {
			triggers = append(triggers, &list.Items[i])
		}
	case v1alpha3.ApplyTriggerKindArgoCD:
		var list v1alpha3.ArgoCDTriggerList
		if err := reader.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("list %ss: %w", kind, err)
		}
		for i := range list.Items {
			triggers = append(triggers, &list.Items[i])
		}
	default:
		return nil, fmt.Errorf("no apply trigger kind %q", kind)
	}
	return triggers, nil
}

// describeTrigger names a trigger in a message: its kind and namespaced name.
func describeTrigger(trigger v1alpha3.ApplyTrigger) string {
	return fmt.Sprintf("%s %s/%s", trigger.TriggerKind(), trigger.GetNamespace(), trigger.GetName())
}

// HeldByAnotherMessage is the Configured message of a trigger whose applier holder holds.
func HeldByAnotherMessage(trigger, holder v1alpha3.ApplyTrigger) string {
	return fmt.Sprintf("%s %s in the cluster of ClusterProvider %q is held by %s, which was created first; "+
		"this trigger takes it over once that one is deleted", trigger.ApplierKind(), trigger.Applier(),
		trigger.ClusterProvider(), describeTrigger(holder))
}
