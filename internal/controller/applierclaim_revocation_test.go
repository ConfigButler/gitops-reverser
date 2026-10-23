// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

var localProviderKey = k8stypes.NamespacedName{Namespace: claimProviderNamespace, Name: claimLocalProvider}

// setApplierGrants replaces the local KubeProvider's allowedApplierClaims.
func (f *holdFixture) setApplierGrants(t *testing.T, grants []configbutleraiv1alpha3.ApplierGrant) {
	t.Helper()
	provider := &configbutleraiv1alpha3.KubeProvider{}
	require.NoError(t, f.r.Get(context.Background(), localProviderKey, provider))
	provider.Spec.AllowedApplierClaims = grants
	require.NoError(t, f.r.Update(context.Background(), provider))
}

// reconcileUntil reconciles the applier claim until done reports true for it.
func (f *holdFixture) reconcileUntil(t *testing.T, done func(*configbutleraiv1alpha3.FluxClaim) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		return done(f.claim(t))
	}, 5*time.Second, 5*time.Millisecond)
}

func handedBack(claim *configbutleraiv1alpha3.FluxClaim) bool {
	return !controllerutil.ContainsFinalizer(claim, configbutleraiv1alpha3.FluxClaimFinalizer)
}

// TestFluxClaim_ADenialHandsTheKustomizationBack: an applier claim its KubeProvider's
// allowedApplierClaims no longer admit takes the retire path: it hands its Kustomization back and lets
// go, and says why.
func TestFluxClaim_ADenialHandsTheKustomizationBack(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
	reconcileClaim(t, f.r)
	require.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	f.events()

	f.setApplierGrants(t, nil)
	f.reconcileUntil(t, handedBack)

	got := f.claim(t)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "handed back to Flux")
	configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, authz.ReasonNamespaceNotAuthorized, configured.Reason)
	assert.Contains(t, configured.Message, "spec.allowedApplierClaims")
	held := findCondition(got.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	assert.Contains(t, held.Message, "held again once the KubeProvider's spec.allowedApplierClaims admit")
	events := f.events()
	require.Len(t, events, 1)
	assert.Contains(t, events[0], EventReasonReleased)
}

// TestFluxClaim_AReadErrorKeepsTheHold: a KubeProvider that cannot be read is no denial, so nothing
// is handed back.
func TestFluxClaim_AReadErrorKeepsTheHold(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
	reconcileClaim(t, f.r)
	f.events()
	f.r.Client = interceptor.NewClient(f.r.Client.(ctrlclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object,
			opts ...ctrlclient.GetOption) error {
			if _, ok := obj.(*configbutleraiv1alpha3.KubeProvider); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)

	got := f.claim(t)
	assert.True(t, controllerutil.ContainsFinalizer(got, configbutleraiv1alpha3.FluxClaimFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "still held")
	assert.Empty(t, f.events())
}

// TestFluxClaim_ADenialKeepsASuspendedKustomizationSuspended: the hand-back restores the state
// recorded before adoption.
func TestFluxClaim_ADenialKeepsASuspendedKustomizationSuspended(t *testing.T) {
	objs := applierFluxObjects()
	objs[0] = fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, "flux",
		metav1.ManagedFieldsOperationUpdate)
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider), objs...)
	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)
	require.Equal(t, configbutleraiv1alpha3.SuspendStateSuspended, got.Status.PreviousSuspendState)

	f.setApplierGrants(t, nil)
	f.reconcileUntil(t, handedBack)

	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)),
		"suspended before adoption, suspended after the hand-back")
}

// TestFluxClaim_ADenialThatCannotHandBackGivesUp takes the retire path's two ways out. A Kustomization
// the identity may no longer patch is let go at once; a cluster that answers with errors keeps the
// applier claim Releasing in status until releaseDeadline. Both say so in a Warning Event.
func TestFluxClaim_ADenialThatCannotHandBackGivesUp(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		atOnce    bool
		wantEvent string
	}{
		"the identity lost its RBAC": {
			err: apierrors.NewForbidden(fluxsim.KustomizationGVR().GroupResource(), "checkout",
				errors.New("no patch for you")),
			atOnce: true, wantEvent: "Warning ReleaseFailed could not hand Kustomization",
		},
		"the cluster keeps failing": {
			err:       apierrors.NewServiceUnavailable("unavailable"),
			wantEvent: "Warning ReleaseFailed gave up handing Kustomization",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
			reconcileClaim(t, f.r)
			resume(t, f) // the hand-back must suspend once more, and that patch fails
			f.events()
			f.sim.Client.PrependReactor(
				"patch",
				"kustomizations",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.err
				},
			)
			f.setApplierGrants(t, nil)

			_, _ = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
			got := f.claim(t)
			if !tc.atOnce {
				require.False(t, handedBack(got))
				assert.Equal(t, ReasonReleasing, heldReason(t, got), "the hand-back shows in status")
				assert.True(t, releasingForDenial(got))
				held := findCondition(got.Status.Conditions, ApplierClaimConditionHeld)
				held.LastTransitionTime = metav1.NewTime(time.Now().Add(-releaseDeadline - time.Minute))
				require.NoError(t, f.r.Status().Update(context.Background(), got))
				_, _ = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
				got = f.claim(t)
			}
			assert.True(t, handedBack(got), "it lets go")
			events := f.events()
			require.NotEmpty(t, events)
			assert.True(t, strings.HasPrefix(events[len(events)-1], tc.wantEvent), events)
		})
	}
}

// TestFluxClaim_ARegrantDuringTheHandBackLetsItFinish: a regrant while the hand-back waits on its
// suspend lets the hand-back finish, and then the applier claim adopts the Kustomization anew.
func TestFluxClaim_ARegrantDuringTheHandBackLetsItFinish(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider), fluxTarget())
	f.settle(t, claimKey)
	f.events()
	grants := applierGrants([]configbutleraiv1alpha3.ScopeNamespace{"*"}, "team-a")
	resume(t, f)
	unpause := f.sim.PauseKustomizations()
	f.setApplierGrants(t, nil)
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	require.Equal(t, ReasonReleasing, heldReason(t, f.claim(t)))
	assert.True(t, releasingForDenial(f.claim(t)))

	f.setApplierGrants(t, grants)
	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.Equal(t, ReasonReleasing, heldReason(t, f.claim(t)), "the hand-back is not abandoned")

	unpause()
	f.reconcileUntil(t, func(claim *configbutleraiv1alpha3.FluxClaim) bool {
		return heldReason(t, claim) == ReasonSuspended
	})
	assert.True(t, controllerutil.ContainsFinalizer(f.claim(t), configbutleraiv1alpha3.FluxClaimFinalizer))
	var reasons []string
	for _, event := range f.events() {
		for _, reason := range []string{EventReasonReleased, EventReasonAdopted} {
			if strings.Contains(event, " "+reason+" ") {
				reasons = append(reasons, reason)
			}
		}
	}
	assert.Equal(t, []string{EventReasonReleased, EventReasonAdopted}, reasons,
		"handed back first, then adopted anew")
}
