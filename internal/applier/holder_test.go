// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// rival is an applier claim of kind in namespace team-b naming the same applier as the one the dispatcher
// fixture made for the GitTarget, in the same cluster, created an hour before it (older) or after.
func rival(t *testing.T, f *dispatcherFixture, kind, name string, older bool) {
	t.Helper()
	own := v1alpha3.NewApplierClaim(kind)
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: name}, own))
	own.SetCreationTimestamp(metav1.NewTime(time.Unix(1_000_000, 0)))
	require.NoError(t, f.control.Update(context.Background(), own.(client.Object)))
	created := own.GetCreationTimestamp().Add(time.Hour)
	if older {
		created = own.GetCreationTimestamp().Add(-time.Hour)
	}
	other := own.DeepCopyObject().(v1alpha3.ApplierClaim)
	other.SetNamespace("team-b")
	other.SetResourceVersion("")
	other.SetCreationTimestamp(metav1.NewTime(created))
	// The fixture's own applier claim names its applier in its own namespace's terms; spell it out.
	applier := own.Applier()
	ref := fluxmeta.NamespacedObjectReference{Namespace: applier.Namespace, Name: applier.Name}
	switch typed := other.(type) {
	case *v1alpha3.FluxClaim:
		typed.Spec.KustomizationRef = ref
	case *v1alpha3.ArgoCDClaim:
		typed.Spec.ApplicationRef = ref
	}
	require.NoError(t, f.control.Create(context.Background(), other.(client.Object)))
	referenceFrom(t, f.control, other)
}

// referenceFrom puts a GitTarget in claim's namespace that names it, and admits that namespace
// on the KubeProviders, so the applier claim is in line for its applier.
func referenceFrom(t *testing.T, c client.Client, claim v1alpha3.ApplierClaim) {
	t.Helper()
	ctx := context.Background()
	target := gitTarget("names-"+claim.GetName(), nil, "")
	target.Namespace = claim.GetNamespace()
	target.Spec.ApplierClaimRef = &v1alpha3.ApplierClaimReference{Kind: claim.ClaimKind(), Name: claim.GetName()}
	require.NoError(t, c.Create(ctx, target))
	for _, name := range []string{localCluster, "prod-eu-1", claim.KubeProviderKey().Name} {
		provider := &v1alpha3.KubeProvider{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: kubeProviderNS, Name: name}, provider); err != nil {
			require.NoError(t, c.Create(ctx, kubeProvider(name, targetNS, claim.GetNamespace())))
			continue
		}
		provider.Spec.AccessFrom.Names = append(provider.Spec.AccessFrom.Names, claim.GetNamespace())
		require.NoError(t, c.Update(ctx, provider))
	}
}

// TestDispatcher_OnlyTheHolderAsksItsApplier is review 5's finding 5, the dispatcher's half: two
// applier claims naming one applier both asked it. Now only the one first in line (created first) does;
// a GitTarget naming the other asks nothing.
func TestDispatcher_OnlyTheHolderAsksItsApplier(t *testing.T) {
	for name, tc := range map[string]struct {
		target  func() *v1alpha3.GitTarget
		kind    string
		claim   string
		applier git.ApplierRef
	}{
		"FluxClaim": {
			target: func() *v1alpha3.GitTarget { return gitTarget("a", checkoutRef(), "") },
			kind:   v1alpha3.ApplierClaimKindFlux,
			claim:  "checkout", applier: checkout,
		},
		"ArgoCDClaim": {
			target: argoTarget, kind: v1alpha3.ApplierClaimKindArgoCD,
			claim: "platform", applier: argoPlatform,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("another applier claim was first", func(t *testing.T) {
				f := newDispatcherFixture(t, []client.Object{tc.target()}, append(behindSource(), manualApp())...)
				rival(t, f, tc.kind, tc.claim, true)

				res := f.attempt(request(tc.applier, v1alpha3.ApplyReasonForeignCommit, "a"))

				assert.Equal(t, git.ApplyDropped, res.Outcome)
				assert.Empty(t, f.patches(), "nothing is resumed")
				assert.Empty(t, f.updates(), "no sync is set")
			})
			t.Run("this applier claim was first", func(t *testing.T) {
				f := newDispatcherFixture(t, []client.Object{tc.target()}, append(behindSource(), manualApp())...)
				rival(t, f, tc.kind, tc.claim, false)

				res := f.attempt(request(tc.applier, v1alpha3.ApplyReasonForeignCommit, "a"))

				assert.Equal(t, git.ApplyRequested, res.Outcome, res.Err)
			})
		})
	}
}

// TestHolder_FirstInLine: the holder is the applier claim in line created first, then by namespace and
// name; one being deleted still holds until it is gone, one no eligible GitTarget references is not
// in line, and neither is an applier claim naming another applier, or the same one in another cluster.
func TestHolder_FirstInLine(t *testing.T) {
	at := func(minutes int) metav1.Time {
		return metav1.NewTime(time.Unix(0, 0).Add(time.Duration(minutes) * time.Minute))
	}
	claim := func(namespace, name, cluster, ks string, created metav1.Time) *v1alpha3.FluxClaim {
		return &v1alpha3.FluxClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, CreationTimestamp: created},
			Spec: v1alpha3.FluxClaimSpec{
				KubeProviderRef:  kubeProviderRef(cluster),
				KustomizationRef: fluxmeta.NamespacedObjectReference{Namespace: "flux-system", Name: ks},
			},
		}
	}
	self := claim("team-c", "mine", localCluster, "checkout", at(10))
	deleting := claim("team-z", "old", localCluster, "checkout", at(5))
	deleting.Finalizers = []string{v1alpha3.FluxClaimFinalizer}
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Unix(100, 0)}
	objects := []client.Object{
		self,
		claim("team-a", "elsewhere", "hub", "checkout", at(1)),
		claim("team-a", "other", localCluster, "infra", at(1)),
		claim("team-b", "same-second", localCluster, "checkout", at(10)),
	}
	unreferenced := claim("team-a", "stray", localCluster, "checkout", at(1))
	f := newDispatcherFixture(t, nil)
	for _, obj := range append(objects, unreferenced) {
		require.NoError(t, f.control.Create(context.Background(), obj))
	}

	holder, err := firstInLine(context.Background(), f.control, self)
	require.NoError(t, err)
	assert.Equal(t, "team-c/mine", holder.GetNamespace()+"/"+holder.GetName(),
		"older applier claims nothing eligible references keep no younger one out")

	referenceFrom(t, f.control, objects[3].(*v1alpha3.FluxClaim))
	holder, err = firstInLine(context.Background(), f.control, self)
	require.NoError(t, err)
	assert.Equal(t, "team-b/same-second", holder.GetNamespace()+"/"+holder.GetName(),
		"created in the same second: the namespace decides")

	require.NoError(t, f.control.Create(context.Background(), deleting))
	holder, err = HeldByAnother(context.Background(), f.control, self)
	require.NoError(t, err)
	require.NotNil(t, holder)
	assert.Equal(
		t,
		"team-z/old",
		holder.GetNamespace()+"/"+holder.GetName(),
		"an applier claim being deleted still holds",
	)

	others, err := ClaimsOfApplier(context.Background(), f.control, self)
	require.NoError(t, err)
	assert.Len(t, others, 3, "the same Kustomization in another cluster, or another one, is not in line")
}

// TestHolder_AnApplyInFlightComesFirst is review 5's P2: a FluxClaim with an operation in flight
// comes first in line, so an applier claim that comes before it by creation and name waits for that apply
// to end. An ArgoCDClaim's operation does not count: a sync somebody else started is never
// replaced, so the Application can change hands mid-sync.
func TestHolder_AnApplyInFlightComesFirst(t *testing.T) {
	created := metav1.NewTime(time.Unix(1_000_000, 0))
	applying := &v1alpha3.ApplyOperation{Phase: v1alpha3.ApplyOperationRunning, Token: "T1"}
	flux := func(namespace string, operation *v1alpha3.ApplyOperation) *v1alpha3.FluxClaim {
		return &v1alpha3.FluxClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "checkout", CreationTimestamp: created},
			Status:     v1alpha3.FluxClaimStatus{Operation: operation},
		}
	}
	argo := func(namespace string, operation *v1alpha3.ApplyOperation) *v1alpha3.ArgoCDClaim {
		return &v1alpha3.ArgoCDClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "platform", CreationTimestamp: created},
			Status:     v1alpha3.ArgoCDClaimStatus{Operation: operation},
		}
	}

	assert.True(t, precedes(flux("team-a", nil), flux("team-b", nil)), "by namespace, with nothing in flight")
	assert.True(t, precedes(flux("team-b", applying), flux("team-a", nil)), "the apply in flight first")
	assert.False(t, precedes(flux("team-a", nil), flux("team-b", applying)))
	done := applying.DeepCopy()
	done.Phase = v1alpha3.ApplyOperationSucceeded
	assert.True(t, precedes(flux("team-a", nil), flux("team-b", done)), "a completed one does not count")
	assert.True(t, precedes(argo("team-a", nil), argo("team-b", applying)), "nor an Argo CD sync")
	assert.Contains(t, HeldByAnotherMessage(flux("team-a", nil), flux("team-b", applying)),
		"is held by FluxClaim team-b/checkout, whose apply is in flight; this applier claim takes it over once that "+
			"apply ends")
}
