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
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

var checkoutKustomization = k8stypes.NamespacedName{Namespace: "flux-system", Name: "checkout"}

// holdFixture is a FluxTrigger team-a/checkout over a simulated Flux cluster holding objs, with
// Events recorded.
type holdFixture struct {
	r        *ApplyTriggerReconciler
	sim      *fluxsim.Sim
	recorder *record.FakeRecorder
}

func newHoldFixture(t *testing.T, trigger *configbutleraiv1alpha3.FluxTrigger, objs ...runtime.Object) *holdFixture {
	t.Helper()
	if objs == nil {
		objs = applierFluxObjects()
	}
	sim := applierSim(t, objs...)
	r := triggerReconciler(triggerTestClient(t, trigger), configbutleraiv1alpha3.ApplyTriggerKindFlux, sim.Client, nil,
		true)
	recorder := record.NewFakeRecorder(32)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// events drains the Events of the suspend lifecycle, leaving out the Ready transitions the status
// session records.
func (f *holdFixture) events() []string {
	var out []string
	for {
		select {
		case event := <-f.recorder.Events:
			for _, reason := range []string{
				EventReasonAdopted, EventReasonResumedElsewhere, EventReasonReleased, EventReasonReleaseFailed,
			} {
				if strings.Contains(event, " "+reason+" ") {
					out = append(out, event)
				}
			}
		default:
			return out
		}
	}
}

func (f *holdFixture) trigger(t *testing.T) *configbutleraiv1alpha3.FluxTrigger {
	t.Helper()
	trigger := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), triggerKey, trigger))
	return trigger
}

func suspendedSpec(ks *unstructured.Unstructured) bool {
	value, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	return value
}

// TestFluxTrigger_AdoptsItsKustomization: a Configured trigger suspends its Kustomization under GitOps
// Reverser's field manager, and is Ready only once that suspend's token is handled, so nothing that
// ran before the adoption is still running.
func TestFluxTrigger_AdoptsItsKustomization(t *testing.T) {
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))

	got := reconcileTrigger(t, f.r).(*configbutleraiv1alpha3.FluxTrigger)

	held := findCondition(got.Status.Conditions, ApplyTriggerConditionSuspended)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionTrue, held.Status, held.Message)
	assert.Equal(t, ReasonHeld, held.Reason)
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeReady))
	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks))
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager,
		"held under GitOps Reverser's field manager")
	assert.Nil(t, got.Status.Operation, "an adoption is no operation")
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Adopted suspended Kustomization flux-system/checkout"),
		events[0])
}

// TestFluxTrigger_AnExternalResumeIsSuspendedAgain is decision 3: `flux resume` on a held
// Kustomization is suspended again with a token, every time, with a Normal Event each time, and
// recorded as an Unresolved operation that completes once nothing that resume started is running.
func TestFluxTrigger_AnExternalResumeIsSuspendedAgain(t *testing.T) {
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
	reconcileTrigger(t, f.r)
	f.events()

	for round := range 2 {
		patch := []byte(`{"spec":{"suspend":false}}`)
		_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").
			Patch(context.Background(), "checkout", k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(f.sim.Applied()) == round+1 }, 5*time.Second,
			time.Millisecond, "Flux applies the moment it is resumed")

		_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
		require.NoError(t, err)
		recorded := f.trigger(t).Status.Operation
		require.NotNil(t, recorded, "round %d", round)
		assert.Equal(t, configbutleraiv1alpha3.ApplyOperationUnresolved, recorded.Phase)
		assert.NotEmpty(t, recorded.FenceToken)
		assert.Empty(t, recorded.Token, "no request of GitOps Reverser's")
		assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
		events := f.events()
		require.Len(t, events, 1, "an Event every time")
		assert.True(t, strings.HasPrefix(events[0], "Normal ResumedElsewhere Kustomization flux-system/checkout "+
			"was resumed outside GitOps Reverser"), events[0])

		got := reconcileTrigger(t, f.r).(*configbutleraiv1alpha3.FluxTrigger)
		require.NotNil(t, got.Status.Operation)
		assert.Equal(t, configbutleraiv1alpha3.ApplyOperationSucceeded, got.Status.Operation.Phase,
			"complete once the suspend's token is handled")
		assert.NotNil(t, got.Status.Operation.FinishedAt)
		assert.Equal(t, recorded.FenceToken, got.Status.Operation.FenceToken)
		assert.True(t, conditionIsTrue(got.Status.Conditions, ApplyTriggerConditionSuspended))
	}
	assert.Len(t, f.sim.Applied(), 2, "each resume applied once, unchecked; that is what the record says")
}

// TestFluxTrigger_LeavesABranchWorkersOperationAlone: a worker records its operation before it
// resumes the Kustomization, so a resumed Kustomization with an operation of the worker's in flight
// is that operation, not an external resume; the worker suspends it again itself.
func TestFluxTrigger_LeavesABranchWorkersOperationAlone(t *testing.T) {
	trigger := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	trigger.Finalizers = []string{configbutleraiv1alpha3.FluxTriggerFinalizer}
	trigger.Status.Conditions = []metav1.Condition{{
		Type: ApplyTriggerConditionSuspended, Status: metav1.ConditionTrue, Reason: ReasonHeld,
		LastTransitionTime: metav1.Now(),
	}}
	trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonRefusal,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	f := newHoldFixture(t, trigger)

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
	require.NoError(t, err)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left resumed for the worker")
	assert.Empty(t, f.events())
	assert.Equal(t, "T1", f.trigger(t).Status.Operation.Token)
}

// TestFluxTrigger_ASuspendAppliedElsewhereIsNeverHeld: a Kustomization whose manifest sets
// spec.suspend, applied by another manager, is Configured=False and is never suspended: the next
// apply of that manifest would resume it.
func TestFluxTrigger_ASuspendAppliedElsewhereIsNeverHeld(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), false, git.FluxFieldManager,
		metav1.ManagedFieldsOperationApply)
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), ks, objs[1])

	got := reconcileTrigger(t, f.r).(*configbutleraiv1alpha3.FluxTrigger)

	configured := findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonSuspendOwnedElsewhere, configured.Reason)
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled), "a human fixes the manifest")
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
}

// TestFluxTrigger_DeletionHandsTheKustomizationBack: deleting the trigger suspends its Kustomization
// once more when it is resumed, waits until that suspend is handled, and then releases spec.suspend,
// so Flux applies on its own again; then the finalizer goes.
func TestFluxTrigger_DeletionHandsTheKustomizationBack(t *testing.T) {
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
	reconcileTrigger(t, f.r)
	f.events()
	// A worker's apply is running as the trigger is deleted.
	patch := []byte(`{"spec":{"suspend":false}}`)
	_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").Patch(context.Background(),
		"checkout", k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: applier.SuspendFieldManager})
	require.NoError(t, err)
	require.NoError(t, f.r.Delete(context.Background(), f.trigger(t)))

	require.Eventually(t, func() bool {
		result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
		require.NoError(t, err)
		if err := f.r.Get(
			context.Background(),
			triggerKey,
			&configbutleraiv1alpha3.FluxTrigger{},
		); apierrors.IsNotFound(
			err,
		) {
			return true
		}
		assert.Equal(t, applyTriggerFenceInterval, result.RequeueAfter)
		return false
	}, 5*time.Second, 5*time.Millisecond, "the finalizer goes once the Kustomization is handed back")

	ks := f.sim.Kustomization(checkoutKustomization)
	assert.False(t, suspendedSpec(ks), "resumed: Flux owns its applies again")
	for _, entry := range ks.GetManagedFields() {
		assert.NotEqual(t, applier.SuspendFieldManager, entry.Manager, "nothing of GitOps Reverser's is left")
	}
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released handed Kustomization flux-system/checkout back"),
		events[0])
}

// TestFluxTrigger_DeletionLetsGoOfWhatIsGone: a Kustomization or a ClusterProvider that is gone has
// nothing to hand back, and must not hold the trigger.
func TestFluxTrigger_DeletionLetsGoOfWhatIsGone(t *testing.T) {
	for name, cluster := range map[string]string{
		"the Kustomization is gone":   configbutleraiv1alpha3.DefaultClusterProviderName,
		"the ClusterProvider is gone": "absent",
	} {
		t.Run(name, func(t *testing.T) {
			trigger := testFluxTrigger(cluster)
			trigger.Finalizers = []string{configbutleraiv1alpha3.FluxTriggerFinalizer}
			sim := applierSim(t)
			r := triggerReconciler(triggerTestClient(t, trigger), configbutleraiv1alpha3.ApplyTriggerKindFlux,
				sim.Client, nil, true)
			r.ApplierClients = func(context.Context, string) (dynamic.Interface, error) { return sim.Client, nil }
			require.NoError(t, r.Delete(context.Background(), trigger))

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
			require.NoError(t, err)
			err = r.Get(context.Background(), triggerKey, &configbutleraiv1alpha3.FluxTrigger{})
			assert.True(t, apierrors.IsNotFound(err), "the finalizer is gone: %v", err)
		})
	}
}

// TestFluxTrigger_TakesOverASuspendBySomebodyElse: a Kustomization `flux suspend` suspended is not
// held: that write may have let a reconcile run, so the trigger takes the field over with a token of
// its own and is Ready once it is handled.
func TestFluxTrigger_TakesOverASuspendBySomebodyElse(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, "flux",
		metav1.ManagedFieldsOperationUpdate)
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), ks, objs[1])

	got := reconcileTrigger(t, f.r).(*configbutleraiv1alpha3.FluxTrigger)

	assert.True(t, conditionIsTrue(got.Status.Conditions, ApplyTriggerConditionSuspended))
	assert.Contains(t, kustomizationManagers(f.sim.Kustomization(checkoutKustomization)), applier.SuspendFieldManager)
	assert.Empty(t, f.events(), "neither an adoption of a running Kustomization nor an external resume")
}

// TestFluxTrigger_AHandBackItMayNotMakeLetsGo: an identity that may not write the Kustomization
// cannot hand it back; the trigger goes anyway, with a Warning that says what is left behind.
func TestFluxTrigger_AHandBackItMayNotMakeLetsGo(t *testing.T) {
	f := newHoldFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
	reconcileTrigger(t, f.r)
	f.events()
	f.sim.Client.PrependReactor("patch", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(fluxsim.KustomizationGVR().GroupResource(), "checkout",
			errors.New("no patch for you"))
	})
	require.NoError(t, f.r.Delete(context.Background(), f.trigger(t)))

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
	require.NoError(t, err)
	err = f.r.Get(context.Background(), triggerKey, &configbutleraiv1alpha3.FluxTrigger{})
	assert.True(t, apierrors.IsNotFound(err), "the trigger is let go: %v", err)
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning ReleaseFailed could not hand Kustomization"), events[0])
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left suspended, as the Event says")
}

// kustomizationManagers lists a Kustomization's field managers.
func kustomizationManagers(ks *unstructured.Unstructured) []string {
	var managers []string
	for _, entry := range ks.GetManagedFields() {
		managers = append(managers, entry.Manager)
	}
	return managers
}
