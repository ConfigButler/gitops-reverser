// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
)

// The second FluxTrigger on flux-system/checkout, in another namespace and created after
// team-a/checkout.
var inLineKey = k8stypes.NamespacedName{Namespace: "team-b", Name: "checkout"}

// duplicateFixture is the hold fixture with two FluxTriggers naming flux-system/checkout in the
// default cluster: team-a/checkout, created first, and team-b/checkout, an hour later. The default
// ClusterProvider admits both namespaces.
func duplicateFixture(t *testing.T) *holdFixture {
	t.Helper()
	first := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	second := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	second.Namespace = inLineKey.Namespace
	second.CreationTimestamp = metav1.NewTime(first.CreationTimestamp.Add(time.Hour))

	sim := applierSim(t, applierFluxObjects()...)
	c := inLineClient(t, configbutleraiv1alpha3.ApplyTriggerKindFlux, first, second)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// inLineClient is a control cluster holding triggers, in team-a and team-b, each referenced by an
// eligible GitTarget of its namespace, with the default ClusterProvider admitting both.
func inLineClient(t *testing.T, kind string, triggers ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	objects := append([]ctrlclient.Object{admitting(configbutleraiv1alpha3.DefaultClusterProviderName,
		"team-a", "team-b")}, triggers...)
	objects = append(objects, referencedIn("team-a", kind)...)
	objects = append(objects, referencedIn("team-b", kind)...)
	return triggerTestClient(t, objects...)
}

// reconcileKey reconciles one trigger a few times, enough to settle a suspend, and returns it.
func (f *holdFixture) reconcileKey(t *testing.T, key k8stypes.NamespacedName) *configbutleraiv1alpha3.FluxTrigger {
	t.Helper()
	trigger := &configbutleraiv1alpha3.FluxTrigger{}
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, f.r.Get(context.Background(), key, trigger))
		held := findCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld)
		return held == nil || held.Reason != ReasonSuspending
	}, 5*time.Second, 5*time.Millisecond)
	return trigger
}

// deleteUntilGone deletes a trigger and reconciles it until its finalizer is gone.
func (f *holdFixture) deleteUntilGone(t *testing.T, key k8stypes.NamespacedName) {
	t.Helper()
	trigger := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), key, trigger))
	require.NoError(t, f.r.Delete(context.Background(), trigger))
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		return apierrors.IsNotFound(f.r.Get(context.Background(), key, &configbutleraiv1alpha3.FluxTrigger{}))
	}, 5*time.Second, 5*time.Millisecond, "the finalizer goes")
}

// TestFluxTrigger_OneTriggerHoldsAKustomization is review 5's finding 5: two FluxTriggers naming one
// Kustomization both held it, under the same field manager. Each took the other's resume for an
// external one and suspended it mid-apply, and deleting either handed back the hold the other still
// needed. Now the one created first holds it; the other is Configured=False (HeldByAnotherTrigger),
// naming the holder, and does nothing to the Kustomization, ever.
func TestFluxTrigger_OneTriggerHoldsAKustomization(t *testing.T) {
	f := duplicateFixture(t)

	second := f.reconcileKey(t, inLineKey)
	first := f.reconcileKey(t, triggerKey)

	assert.True(t, conditionIsTrue(first.Status.Conditions, ApplyTriggerConditionHeld), "the first holds it")
	assert.True(t, conditionIsTrue(first.Status.Conditions, ConditionTypeReady))
	configured := findCondition(second.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionFalse, configured.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, configured.Reason)
	assert.Equal(t, "Kustomization flux-system/checkout in the cluster of ClusterProvider \"default\" is held by "+
		"FluxTrigger team-a/checkout, which was created first; this trigger takes it over once that one is deleted "+
		"or no eligible GitTarget references it", configured.Message)
	ready := findCondition(second.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, ready.Reason)
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeStalled), "a human removes one of them")
	held := findCondition(second.Status.Conditions, ApplyTriggerConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionUnknown, held.Status)
	assert.Equal(t, ReasonNotConfigured, held.Reason)

	events := f.events()
	require.Len(t, events, 1, "one adoption, by the holder: %v", events)
	assert.True(t, strings.HasPrefix(events[0], "Normal Adopted"), events[0])

	// A GitTarget naming the trigger in line says why it is not Ready.
	target := triggerTarget(true)
	target.Namespace = inLineKey.Namespace
	gt := &GitTargetReconciler{Client: f.r.Client}
	verdict, _ := gt.observeApplyTrigger(context.Background(), beginStatus(nil, nil, target), target,
		triggerProvider())
	assert.Equal(t, metav1.ConditionFalse, verdict.value.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, verdict.value.Reason)
	assert.Contains(t, verdict.value.Message, "held by FluxTrigger team-a/checkout")
	assert.True(t, verdict.stalled)
}

// TestFluxTrigger_TheHoldersResumeIsNoExternalResumeToTheOther: a branch worker's apply through the
// holder resumes the Kustomization; the trigger in line does not take that for `flux resume`, and
// leaves it running.
func TestFluxTrigger_TheHoldersResumeIsNoExternalResumeToTheOther(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, triggerKey)
	f.reconcileKey(t, inLineKey)
	f.events()

	// The worker records its operation on the holder, and resumes the Kustomization.
	holder := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), triggerKey, holder))
	holder.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonRefusal,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	require.NoError(t, f.r.Status().Update(context.Background(), holder))
	patch := []byte(`{"spec":{"suspend":false}}`)
	_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").Patch(context.Background(),
		"checkout", k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: applier.SuspendFieldManager})
	require.NoError(t, err)

	second := f.reconcileKey(t, inLineKey)

	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left running for the holder")
	assert.Empty(t, f.events(), "no external resume")
	assert.Nil(t, second.Status.Operation, "nothing recorded on the trigger in line")
}

// TestFluxTrigger_DeletingTheTriggerInLineHandsNothingBack: the trigger in line never held the
// Kustomization, so its finalizer goes without touching it: it stays suspended under GitOps
// Reverser's field manager, for the holder.
func TestFluxTrigger_DeletingTheTriggerInLineHandsNothingBack(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, triggerKey)
	f.reconcileKey(t, inLineKey)
	f.events()

	f.deleteUntilGone(t, inLineKey)

	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks), "still held")
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager)
	assert.Empty(t, f.events(), "nothing released")
	holder := f.reconcileKey(t, triggerKey)
	assert.True(t, conditionIsTrue(holder.Status.Conditions, ApplyTriggerConditionHeld))
}

// TestFluxTrigger_DeletingTheHolderHandsOverToTheNextInLine: the holder suspends the Kustomization as
// it goes, and leaves it suspended for the trigger in line instead of releasing it: released, Flux
// would apply it at once, unchecked against the branch, before the next trigger could suspend it
// again (review 6). Its going enqueues the trigger in line (a deletion maps to the others naming the
// applier), which then holds the Kustomization as it was left, with nothing to adopt.
func TestFluxTrigger_DeletingTheHolderHandsOverToTheNextInLine(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, triggerKey)
	f.reconcileKey(t, inLineKey)
	f.events()
	gone := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), triggerKey, gone))
	applied := len(f.sim.Applied())

	f.deleteUntilGone(t, triggerKey)
	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks), "never released between the two")
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager)
	assert.Len(t, f.sim.Applied(), applied, "so Flux applied nothing on its own")
	requests := f.r.triggersInLine(context.Background(), gone)
	require.Len(t, requests, 1)
	assert.Equal(t, inLineKey, requests[0].NamespacedName)
	assert.True(t, triggerLineChanged().Delete(event.DeleteEvent{Object: gone}))
	assert.False(t, triggerLineChanged().Update(event.UpdateEvent{ObjectOld: gone, ObjectNew: gone}))

	second := f.reconcileKey(t, inLineKey)

	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplyTriggerConditionConfigured))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplyTriggerConditionHeld))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeReady))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	assert.Equal(t, []string{"Normal Released left Kustomization flux-system/checkout suspended for FluxTrigger " +
		"team-b/checkout, which holds it next"}, f.events())
}

// TestFluxTrigger_AHolderWithAnApplyInFlightKeepsIt is review 5's second finding (P2): a trigger
// created in the same second as the holder, which comes before it by name, appears while the
// holder's apply is in flight. It waits: it is Configured=False, leaves the resumed Kustomization to
// the holder's worker, and records nothing. Once that apply ends, the holder still keeps it: a
// trigger that holds its Kustomization comes before any that does not, so the Kustomization changes
// hands only once the holder lets go of it.
func TestFluxTrigger_AHolderWithAnApplyInFlightKeepsIt(t *testing.T) {
	holderTrigger := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	holderTrigger.Namespace = inLineKey.Namespace // team-b: after team-a by name
	holderTrigger.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	holderKey := k8stypes.NamespacedName{Namespace: holderTrigger.Namespace, Name: holderTrigger.Name}
	sim := applierSim(t, applierFluxObjects()...)
	c := inLineClient(t, configbutleraiv1alpha3.ApplyTriggerKindFlux, holderTrigger)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	f := &holdFixture{r: r, sim: sim, recorder: recorder}
	f.reconcileKey(t, holderKey)
	f.events()

	// The holder's worker records its operation, and resumes the Kustomization.
	held := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), holderKey, held))
	held.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonRefusal,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	require.NoError(t, f.r.Status().Update(context.Background(), held))
	_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").Patch(context.Background(),
		"checkout", k8stypes.MergePatchType, []byte(`{"spec":{"suspend":false}}`),
		metav1.PatchOptions{FieldManager: applier.SuspendFieldManager})
	require.NoError(t, err)
	// The newcomer: same second, team-a.
	newcomer := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	newcomer.CreationTimestamp = holderTrigger.CreationTimestamp
	require.NoError(t, f.r.Create(context.Background(), newcomer))

	waiting := f.reconcileKey(t, triggerKey)

	configured := findCondition(waiting.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, configured.Reason)
	assert.Contains(t, configured.Message, "whose apply is in flight; this trigger takes it over once that apply ends")
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left running for the holder")
	assert.Nil(t, waiting.Status.Operation, "no external resume recorded")
	assert.Empty(t, f.events())
	assert.True(t, conditionIsTrue(f.reconcileKey(t, holderKey).Status.Conditions, ApplyTriggerConditionConfigured),
		"the holder keeps it")

	// The worker suspends the Kustomization again and completes its operation.
	_, err = f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").Patch(context.Background(),
		"checkout", k8stypes.MergePatchType, []byte(`{"spec":{"suspend":true}}`),
		metav1.PatchOptions{FieldManager: applier.SuspendFieldManager})
	require.NoError(t, err)
	require.NoError(t, f.r.Get(context.Background(), holderKey, held))
	finished := metav1.Now()
	held.Status.Operation.Phase, held.Status.Operation.FinishedAt = configbutleraiv1alpha3.ApplyOperationSucceeded,
		&finished
	require.NoError(t, f.r.Status().Update(context.Background(), held))

	still := findCondition(f.reconcileKey(t, triggerKey).Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, still)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, still.Reason)
	assert.Contains(t, still.Message, "which holds it now")
	assert.True(t, conditionIsTrue(f.reconcileKey(t, holderKey).Status.Conditions, ApplyTriggerConditionConfigured),
		"the holder keeps it between applies too")
	assert.Empty(t, f.events(), "and nothing is adopted twice")
}

// TestArgoCDTrigger_OneTriggerHoldsAnApplication: two ArgoCDTriggers on one Application would both
// start syncs; the one created later is Configured=False (HeldByAnotherTrigger).
func TestArgoCDTrigger_OneTriggerHoldsAnApplication(t *testing.T) {
	first := testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	second := testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	second.Namespace = inLineKey.Namespace
	second.CreationTimestamp = first.CreationTimestamp
	c := inLineClient(t, configbutleraiv1alpha3.ApplyTriggerKindArgoCD, first, second)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindArgoCD,
		applierFakeSource(t, applierArgoApplication(nil)), nil, true)

	for _, key := range []k8stypes.NamespacedName{triggerKey, inLineKey} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}

	got := &configbutleraiv1alpha3.ArgoCDTrigger{}
	require.NoError(t, c.Get(context.Background(), triggerKey, got))
	assert.True(t, conditionIsTrue(got.Status.Conditions, ApplyTriggerConditionConfigured),
		"created in the same second: the namespace decides")
	require.NoError(t, c.Get(context.Background(), inLineKey, got))
	configured := findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, configured.Reason)
	assert.Contains(t, configured.Message, "Application argocd/checkout")
	assert.Contains(t, configured.Message, "held by ArgoCDTrigger team-a/checkout")
}
