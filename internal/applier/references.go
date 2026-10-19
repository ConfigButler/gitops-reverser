// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/repourl"
)

// A trigger acts only while an eligible GitTarget references it: one that names it, is admitted by
// its ClusterProviders, and writes the repository and branch the applier fetches. A trigger nothing
// eligible names serves nobody, so it reads and writes nothing in the applier's cluster, and is not
// in line for its applier. Eligibility is judged from those inputs alone, never from the
// GitTarget's Ready, which reads the trigger's own Ready back (ApplyTriggerReady) and would make
// the two circular.

// ReasonNotReferenced is Configured=Unknown (and Ready=True) on a trigger no GitTarget names: it is
// asked nothing, so it does nothing.
const ReasonNotReferenced = "NotReferenced"

// ReasonNotBound is Configured on a trigger whose naming GitTargets are none of them eligible:
// Unknown while one cannot be judged yet, False once each is refused for different reasons.
const ReasonNotBound = "NotBound"

// ReferenceState is how one GitTarget naming a trigger stands with it.
type ReferenceState int

const (
	// ReferenceEligible names the trigger, is admitted, and writes what the applier fetches.
	ReferenceEligible ReferenceState = iota
	// ReferenceUndecided cannot be judged yet: its GitProvider or a ClusterProvider is missing, or
	// the applier's source has not been read.
	ReferenceUndecided
	// ReferenceRefused is not admitted, or writes another repository or branch.
	ReferenceRefused
)

// Reference is one GitTarget naming a trigger, judged.
type Reference struct {
	Target types.NamespacedName
	State  ReferenceState
	// Reason is the refusal's condition reason (ReasonSourceMismatch,
	// authz.ReasonNamespaceNotAuthorized), set only for a refused reference.
	Reason string
	// Message says why a reference is not eligible.
	Message string
}

// References are the GitTargets naming one trigger, judged.
type References []Reference

// Named reports whether any GitTarget names the trigger.
func (refs References) Named() bool { return len(refs) > 0 }

// Eligible reports whether at least one GitTarget naming the trigger is eligible.
func (refs References) Eligible() bool {
	for _, ref := range refs {
		if ref.State == ReferenceEligible {
			return true
		}
	}
	return false
}

// Refused reports whether every GitTarget naming the trigger is conclusively refused: there is at
// least one, and none is eligible or may yet be.
func (refs References) Refused() bool {
	for _, ref := range refs {
		if ref.State != ReferenceRefused {
			return false
		}
	}
	return refs.Named()
}

// Reason is the Configured reason of references that are not eligible: the refusal reason they
// share when every one is refused for the same one, and ReasonNotBound otherwise.
func (refs References) Reason() string {
	reason := ""
	for _, ref := range refs {
		switch {
		case ref.State != ReferenceRefused:
			return ReasonNotBound
		case reason == "":
			reason = ref.Reason
		case reason != ref.Reason:
			return ReasonNotBound
		}
	}
	if reason == "" {
		return ReasonNotBound
	}
	return reason
}

// Message lists each GitTarget that is not eligible, and why.
func (refs References) Message() string {
	var parts []string
	for _, ref := range refs {
		if ref.State != ReferenceEligible {
			parts = append(parts, fmt.Sprintf("GitTarget %s: %s", ref.Target, ref.Message))
		}
	}
	return "no GitTarget naming this trigger is eligible: " + strings.Join(parts, "; ")
}

// NamingTargets lists the GitTargets that name trigger, in its namespace, and are not being
// deleted, by name.
func NamingTargets(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger,
) ([]*v1alpha3.GitTarget, error) {
	var list v1alpha3.GitTargetList
	if err := reader.List(ctx, &list, client.InNamespace(trigger.GetNamespace())); err != nil {
		return nil, fmt.Errorf("list GitTargets: %w", err)
	}
	var targets []*v1alpha3.GitTarget
	for i := range list.Items {
		target := &list.Items[i]
		if target.DeletionTimestamp.IsZero() && Names(target, trigger) {
			targets = append(targets, target)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// Names reports whether target's spec.applyTriggerRef names trigger.
func Names(target *v1alpha3.GitTarget, trigger v1alpha3.ApplyTrigger) bool {
	ref := target.Spec.ApplyTriggerRef
	return ref != nil && target.Namespace == trigger.GetNamespace() && ref.Kind == trigger.TriggerKind() &&
		ref.Name == trigger.GetName()
}

// JudgeReferences judges each of targets, which name trigger, against the source its applier
// fetches, or with source nil, on admission alone: a reference that is admitted is then undecided.
// A read that fails with anything but NotFound is an error, so the caller keeps what it published.
func JudgeReferences(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger, targets []*v1alpha3.GitTarget,
	source *Source,
) (References, error) {
	refs := make(References, 0, len(targets))
	for _, target := range targets {
		ref, err := judgeReference(ctx, reader, trigger, target, source)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func judgeReference(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger, target *v1alpha3.GitTarget,
	source *Source,
) (Reference, error) {
	ref := Reference{Target: types.NamespacedName{Namespace: target.Namespace, Name: target.Name}}
	decision, err := authz.ApplierAdmitted(ctx, reader, target, trigger)
	switch {
	case err != nil:
		return ref, fmt.Errorf("check GitTarget %s against its ClusterProviders: %w", ref.Target, err)
	case decision.Allowed:
	case decision.Reason == authz.ReasonClusterProviderNotFound:
		// A provider may be on its way, as the trigger's own ClusterProviderNotFound may.
		ref.State, ref.Message = ReferenceUndecided, decision.Message
		return ref, nil
	default:
		ref.State, ref.Reason, ref.Message = ReferenceRefused, decision.Reason, decision.Message
		return ref, nil
	}

	provider := &v1alpha3.GitProvider{}
	key := client.ObjectKey{Namespace: target.Namespace, Name: target.Spec.GitProviderRef.Name}
	if err := reader.Get(ctx, key, provider); err != nil {
		if !apierrors.IsNotFound(err) {
			return ref, fmt.Errorf("read GitProvider %s: %w", key, err)
		}
		ref.State, ref.Message = ReferenceUndecided, fmt.Sprintf("GitProvider %s not found", key)
		return ref, nil
	}
	if source == nil {
		ref.State, ref.Message = ReferenceUndecided, "the applier's source has not been read yet"
		return ref, nil
	}
	want := Binding{RepositoryURL: provider.Spec.URL, Branch: target.Spec.Branch}
	if mismatch := bindingMismatch(trigger, source.URL, source.Revision, want); mismatch != "" {
		ref.State, ref.Reason, ref.Message = ReferenceRefused, ReasonSourceMismatch, mismatch
		return ref, nil
	}
	return ref, nil
}

// bindingMismatch returns why an applier fetching url and branch does not apply what want writes,
// or "" when it does.
func bindingMismatch(trigger v1alpha3.ApplyTrigger, url, branch string, want Binding) string {
	name := trigger.TriggerKind() + " " + trigger.GetName()
	if !repourl.Same(url, want.RepositoryURL) {
		return fmt.Sprintf("the applier of %s applies %s, not %s", name, url, want.RepositoryURL)
	}
	if branch != want.Branch {
		return fmt.Sprintf("the applier of %s applies branch %q, not %s", name, branch, want.Branch)
	}
	return ""
}

// eligible reports whether at least one GitTarget naming trigger is eligible, judged against
// source, the applier's source as the caller read it: triggers in line for one applier share it.
// With no source read, a reference that is not refused counts: a trigger that may be eligible is
// taken to be, so a caller that cannot tell waits rather than acts.
func eligible(
	ctx context.Context, reader client.Reader, trigger v1alpha3.ApplyTrigger, source *Source,
) (bool, error) {
	if !trigger.GetDeletionTimestamp().IsZero() {
		return false, nil
	}
	targets, err := NamingTargets(ctx, reader, trigger)
	if err != nil || len(targets) == 0 {
		return false, err
	}
	refs, err := JudgeReferences(ctx, reader, trigger, targets, source)
	if err != nil || source != nil {
		return refs.Eligible(), err
	}
	for _, ref := range refs {
		if ref.State != ReferenceRefused {
			return true, nil
		}
	}
	return false, nil
}
