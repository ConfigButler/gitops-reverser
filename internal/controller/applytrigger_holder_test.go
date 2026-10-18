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
	c := triggerTestClient(t, first, second, admitting(configbutleraiv1alpha3.DefaultClusterProviderName,
		"team-a", "team-b"))
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// reconcileKey reconciles one trigger a few times, enough to settle a fence, and returns it.
func (f *holdFixture) reconcileKey(t *testing.T, key k8stypes.NamespacedName) *configbutleraiv1alpha3.FluxTrigger {
	t.Helper()
	trigger := &configbutleraiv1alpha3.FluxTrigger{}
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, f.r.Get(context.Background(), key, trigger))
		held := findCondition(trigger.Status.Conditions, ApplyTriggerConditionSuspended)
		return held == nil || held.Reason != ReasonFencing
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

	assert.True(t, conditionIsTrue(first.Status.Conditions, ApplyTriggerConditionSuspended), "the first holds it")
	assert.True(t, conditionIsTrue(first.Status.Conditions, ConditionTypeReady))
	configured := findCondition(second.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionFalse, configured.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, configured.Reason)
	assert.Equal(t, "Kustomization flux-system/checkout in the cluster of ClusterProvider \"default\" is held by "+
		"FluxTrigger team-a/checkout, which was created first; this trigger takes it over once that one is deleted",
		configured.Message)
	ready := findCondition(second.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, ready.Reason)
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeStalled), "a human removes one of them")
	held := findCondition(second.Status.Conditions, ApplyTriggerConditionSuspended)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionUnknown, held.Status)
	assert.Equal(t, ReasonNotHeld, held.Reason)

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
	assert.True(t, conditionIsTrue(holder.Status.Conditions, ApplyTriggerConditionSuspended))
}

// TestFluxTrigger_DeletingTheHolderHandsOverToTheNextInLine: the holder hands the Kustomization back
// as it goes; its going enqueues the trigger in line (a deletion maps to the others naming the
// applier), which then holds it, starting with an adoption.
func TestFluxTrigger_DeletingTheHolderHandsOverToTheNextInLine(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, triggerKey)
	f.reconcileKey(t, inLineKey)
	f.events()
	gone := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), triggerKey, gone))

	f.deleteUntilGone(t, triggerKey)
	requests := f.r.triggersInLine(context.Background(), gone)
	require.Len(t, requests, 1)
	assert.Equal(t, inLineKey, requests[0].NamespacedName)
	assert.True(t, triggerGone().Delete(event.DeleteEvent{Object: gone}))
	assert.False(t, triggerGone().Update(event.UpdateEvent{ObjectOld: gone, ObjectNew: gone}))

	second := f.reconcileKey(t, inLineKey)

	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplyTriggerConditionConfigured))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplyTriggerConditionSuspended))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeReady))
	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks))
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager)
	events := f.events()
	require.Len(t, events, 2, "%v", events)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released handed Kustomization flux-system/checkout back"),
		events[0])
	assert.True(t, strings.HasPrefix(events[1], "Normal Adopted suspended Kustomization flux-system/checkout"),
		events[1])
}

// TestArgoCDTrigger_OneTriggerHoldsAnApplication: two ArgoCDTriggers on one Application would both
// start syncs; the one created later is Configured=False (HeldByAnotherTrigger).
func TestArgoCDTrigger_OneTriggerHoldsAnApplication(t *testing.T) {
	first := testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	second := testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	second.Namespace = inLineKey.Namespace
	second.CreationTimestamp = first.CreationTimestamp
	c := triggerTestClient(t, first, second, admitting(configbutleraiv1alpha3.DefaultClusterProviderName,
		"team-a", "team-b"))
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
