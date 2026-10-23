// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// One applier, one applier claim: two applier claims holding the same Kustomization would both suspend it under
// the same field manager, each would take the other's resume for an external one and suspend it
// mid-apply, and deleting either would hand back the hold the other still needs. Two applier claims on one
// Application would both start syncs. So exactly one applier claim holds an applier, by a rule every party
// computes alike (precedes, over the applier claims in line): the applier claim reconciler, which holds and
// hands back only for the holder, and the dispatcher, which asks the applier only for it. Both
// confirm it uncached before they write to a Kustomization.
//
// Only an applier claim in line can hold: one an eligible GitTarget references (references.go), or one
// that carries its finalizer, which it does from before its first write to the applier (a suspend,
// or switching automated sync off) until it has handed the applier back or over. So an older applier claim nothing eligible references
// keeps no younger one out, and one that is retiring stays the holder until its release is done.

// ReasonHeldByAnotherClaim is Configured=False on an applier claim whose applier another applier claim of its
// kind holds: one in line that names the same object in the same KubeProvider's cluster, and has
// an apply in flight, holds the Kustomization already, or was created first. The applier claim takes the
// applier over once it no longer comes first.
const ReasonHeldByAnotherClaim = "HeldByAnotherClaim"

// sameApplier reports whether two applier claims name one applier: the same kind of object, in the same
// KubeProvider's cluster, with the same namespace and name.
func sameApplier(a, b v1alpha3.ApplierClaim) bool {
	return a.ClaimKind() == b.ClaimKind() && a.KubeProviderKey() == b.KubeProviderKey() &&
		a.Applier() == b.Applier()
}

// sameClaim reports whether a and b are the same applier claim, by kind, namespace and name.
func sameClaim(a, b v1alpha3.ApplierClaim) bool {
	return a.ClaimKind() == b.ClaimKind() && a.GetNamespace() == b.GetNamespace() && a.GetName() == b.GetName()
}

// precedes reports whether a comes before b in line for an applier. A FluxClaim with an operation
// in flight comes first: its worker follows that operation until the Kustomization is suspended
// again, and an applier claim that took the Kustomization over meanwhile would take that resume for an
// external one and suspend it mid-apply. So a Kustomization changes hands only between operations,
// whatever applier claims appear; an applier claim created in the same second as the holder, which can come
// before it by name, waits for the holder's apply to end. An Application has no such hold: a sync
// somebody else started is never replaced, so an ArgoCDClaim's operation does not count. Then an
// applier claim that holds its applier already (it carries the finalizer) comes first: an applier claim that
// becomes eligible later takes it over only once the holder lets go, so an applier is never taken
// from a holder still holding or releasing it. Then the applier claim created first comes
// first, and then by namespace and name.
func precedes(a, b v1alpha3.ApplierClaim) bool {
	if applying, other := holdsAnApply(a), holdsAnApply(b); applying != other {
		return applying
	}
	if holding, other := Holding(a), Holding(b); holding != other {
		return holding
	}
	return createdBefore(a, b)
}

// Holding reports whether claim carries its kind's finalizer: it holds its applier (a
// Kustomization suspended, an Application's automated sync off), or hands it back or over.
func Holding(claim v1alpha3.ApplierClaim) bool {
	finalizer := v1alpha3.FluxClaimFinalizer
	if claim.ClaimKind() == v1alpha3.ApplierClaimKindArgoCD {
		finalizer = v1alpha3.ArgoCDClaimFinalizer
	}
	return slices.Contains(claim.GetFinalizers(), finalizer)
}

// holdsAnApply reports whether claim is a FluxClaim with an operation in flight.
func holdsAnApply(claim v1alpha3.ApplierClaim) bool {
	operation := claim.Operation()
	return claim.ClaimKind() == v1alpha3.ApplierClaimKindFlux && operation != nil && !operation.Phase.Completed()
}

// createdBefore orders applier claims by creation, then by namespace, then by name.
func createdBefore(a, b v1alpha3.ApplierClaim) bool {
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

// firstInLine is the applier claim that holds claim's applier: of the applier claims of its kind in line for
// the same applier, the one first (precedes). applier claim itself counts as in line. An applier claim being
// deleted still holds it, until it is gone: its finalizer is handing the applier back. It is claim
// itself when no other comes before it.
func firstInLine(
	ctx context.Context, reader client.Reader, claim v1alpha3.ApplierClaim,
) (v1alpha3.ApplierClaim, error) {
	others, err := ClaimsOfApplier(ctx, reader, claim)
	if err != nil {
		return nil, err
	}
	source := publishedSource(claim)
	first := claim
	for _, other := range others {
		if !precedes(other, first) {
			continue
		}
		inLine, err := inLine(ctx, reader, other, source)
		if err != nil {
			return nil, err
		}
		if inLine {
			first = other
		}
	}
	return first, nil
}

// inLine reports whether other is in line for the applier whose source the caller read: it holds
// it (Holding), or an eligible GitTarget references it.
func inLine(ctx context.Context, reader client.Reader, other v1alpha3.ApplierClaim, source *Source) (bool, error) {
	if Holding(other) {
		return true, nil
	}
	return eligible(ctx, reader, other, source)
}

// publishedSource is the source applier claim last read, which every applier claim naming its applier shares,
// or nil when it has read none.
func publishedSource(claim v1alpha3.ApplierClaim) *Source {
	url, branch, known := claim.SourceBinding()
	if !known {
		return nil
	}
	return &Source{URL: url, Revision: branch}
}

// HeldByAnother is the applier claim that holds claim's applier when it is another, or nil when claim
// holds it itself.
func HeldByAnother(
	ctx context.Context, reader client.Reader, claim v1alpha3.ApplierClaim,
) (v1alpha3.ApplierClaim, error) {
	first, err := firstInLine(ctx, reader, claim)
	if err != nil || sameClaim(first, claim) {
		return nil, err
	}
	return first, nil
}

// ClaimsOfApplier lists the applier claims other than claim that name its applier.
func ClaimsOfApplier(
	ctx context.Context, reader client.Reader, claim v1alpha3.ApplierClaim,
) ([]v1alpha3.ApplierClaim, error) {
	claims, err := ListApplierClaims(ctx, reader, claim.ClaimKind())
	if err != nil {
		return nil, err
	}
	var others []v1alpha3.ApplierClaim
	for _, other := range claims {
		if !sameClaim(other, claim) && sameApplier(other, claim) {
			others = append(others, other)
		}
	}
	return others, nil
}

// ListApplierClaims lists the applier claims of kind, with opts (everywhere without a namespace option).
func ListApplierClaims(
	ctx context.Context, reader client.Reader, kind string, opts ...client.ListOption,
) ([]v1alpha3.ApplierClaim, error) {
	var claims []v1alpha3.ApplierClaim
	switch kind {
	case v1alpha3.ApplierClaimKindFlux:
		var list v1alpha3.FluxClaimList
		if err := reader.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("list %ss: %w", kind, err)
		}
		for i := range list.Items {
			claims = append(claims, &list.Items[i])
		}
	case v1alpha3.ApplierClaimKindArgoCD:
		var list v1alpha3.ArgoCDClaimList
		if err := reader.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("list %ss: %w", kind, err)
		}
		for i := range list.Items {
			claims = append(claims, &list.Items[i])
		}
	default:
		return nil, fmt.Errorf("no applier claim kind %q", kind)
	}
	return claims, nil
}

// HeldByAnotherMessage is the Configured message of an applier claim whose applier holder holds.
func HeldByAnotherMessage(claim, holder v1alpha3.ApplierClaim) string {
	held := fmt.Sprintf("%s %s in the cluster of KubeProvider %s is held by %s %s/%s", claim.ApplierKind(),
		claim.Applier(), claim.KubeProviderKey(), holder.ClaimKind(), holder.GetNamespace(), holder.GetName())
	switch {
	case holdsAnApply(holder) && !holdsAnApply(claim):
		return held + ", whose apply is in flight; this applier claim takes it over once that apply ends"
	case Holding(holder) && !Holding(claim):
		return held + ", which holds it now; this applier claim takes it over once that one lets go of it"
	}
	return held + ", which was created first; this applier claim takes it over once that one is deleted or no " +
		"eligible GitTarget references it"
}
