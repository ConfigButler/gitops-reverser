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

// rival is a trigger of kind in namespace team-b naming the same applier as the one the dispatcher
// fixture made for the GitTarget, in the same cluster, created an hour before it (older) or after.
func rival(t *testing.T, f *dispatcherFixture, kind, name string, older bool) {
	t.Helper()
	own := v1alpha3.NewApplyTrigger(kind)
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: name}, own))
	own.SetCreationTimestamp(metav1.NewTime(time.Unix(1_000_000, 0)))
	require.NoError(t, f.control.Update(context.Background(), own.(client.Object)))
	created := own.GetCreationTimestamp().Add(time.Hour)
	if older {
		created = own.GetCreationTimestamp().Add(-time.Hour)
	}
	other := own.DeepCopyObject().(v1alpha3.ApplyTrigger)
	other.SetNamespace("team-b")
	other.SetResourceVersion("")
	other.SetCreationTimestamp(metav1.NewTime(created))
	// The fixture's own trigger names its applier in its own namespace's terms; spell it out.
	applier := own.Applier()
	ref := fluxmeta.NamespacedObjectReference{Namespace: applier.Namespace, Name: applier.Name}
	switch typed := other.(type) {
	case *v1alpha3.FluxTrigger:
		typed.Spec.KustomizationRef = ref
	case *v1alpha3.ArgoCDTrigger:
		typed.Spec.ApplicationRef = ref
	}
	require.NoError(t, f.control.Create(context.Background(), other.(client.Object)))
}

// TestDispatcher_OnlyTheHolderAsksItsApplier is review 5's finding 5, the dispatcher's half: two
// triggers naming one applier both asked it. Now only the one first in line (created first) does;
// a GitTarget naming the other asks nothing.
func TestDispatcher_OnlyTheHolderAsksItsApplier(t *testing.T) {
	for name, tc := range map[string]struct {
		target  func() *v1alpha3.GitTarget
		kind    string
		trigger string
		applier git.ApplierRef
	}{
		"FluxTrigger": {
			target:  func() *v1alpha3.GitTarget { return gitTarget("a", checkoutRef(), "") },
			kind:    v1alpha3.ApplyTriggerKindFlux,
			trigger: "checkout", applier: checkout,
		},
		"ArgoCDTrigger": {
			target: argoTarget, kind: v1alpha3.ApplyTriggerKindArgoCD,
			trigger: "platform", applier: argoPlatform,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("another trigger was first", func(t *testing.T) {
				f := newDispatcherFixture(t, []client.Object{tc.target()}, append(behindSource(), manualApp())...)
				rival(t, f, tc.kind, tc.trigger, true)

				res := f.attempt(request(tc.applier, v1alpha3.ApplyReasonForeignCommit, "a"))

				assert.Equal(t, git.ApplyDropped, res.Outcome)
				assert.Empty(t, f.patches(), "nothing is resumed")
				assert.Empty(t, f.updates(), "no sync is set")
			})
			t.Run("this trigger was first", func(t *testing.T) {
				f := newDispatcherFixture(t, []client.Object{tc.target()}, append(behindSource(), manualApp())...)
				rival(t, f, tc.kind, tc.trigger, false)

				res := f.attempt(request(tc.applier, v1alpha3.ApplyReasonForeignCommit, "a"))

				assert.Equal(t, git.ApplyRequested, res.Outcome, res.Err)
			})
		})
	}
}

// TestHolder_FirstInLine: the holder is the trigger created first, then by namespace and name; one
// being deleted still holds until it is gone, and a trigger naming another applier, or the same one
// in another cluster, is not in line.
func TestHolder_FirstInLine(t *testing.T) {
	at := func(minutes int) metav1.Time {
		return metav1.NewTime(time.Unix(0, 0).Add(time.Duration(minutes) * time.Minute))
	}
	trigger := func(namespace, name, cluster, ks string, created metav1.Time) *v1alpha3.FluxTrigger {
		return &v1alpha3.FluxTrigger{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, CreationTimestamp: created},
			Spec: v1alpha3.FluxTriggerSpec{
				ClusterProviderRef: &fluxmeta.LocalObjectReference{Name: cluster},
				KustomizationRef:   fluxmeta.NamespacedObjectReference{Namespace: "flux-system", Name: ks},
			},
		}
	}
	self := trigger("team-c", "mine", "default", "checkout", at(10))
	deleting := trigger("team-z", "old", "default", "checkout", at(5))
	deleting.Finalizers = []string{v1alpha3.FluxTriggerFinalizer}
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Unix(100, 0)}
	objects := []client.Object{
		self,
		trigger("team-a", "elsewhere", "hub", "checkout", at(1)),
		trigger("team-a", "other", "default", "infra", at(1)),
		trigger("team-b", "same-second", "default", "checkout", at(10)),
	}
	f := newDispatcherFixture(t, nil)
	for _, obj := range objects {
		require.NoError(t, f.control.Create(context.Background(), obj))
	}

	holder, err := Holder(context.Background(), f.control, self)
	require.NoError(t, err)
	assert.Equal(t, "team-b/same-second", holder.GetNamespace()+"/"+holder.GetName(),
		"created in the same second: the namespace decides")

	require.NoError(t, f.control.Create(context.Background(), deleting))
	holder, err = HeldByAnother(context.Background(), f.control, self)
	require.NoError(t, err)
	require.NotNil(t, holder)
	assert.Equal(t, "team-z/old", holder.GetNamespace()+"/"+holder.GetName(), "a trigger being deleted still holds")

	others, err := TriggersOfApplier(context.Background(), f.control, self)
	require.NoError(t, err)
	assert.Len(t, others, 2, "the same Kustomization in another cluster, or another one, is not in line")
}
