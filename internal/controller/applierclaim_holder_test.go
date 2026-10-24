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

// The second FluxClaim on flux-system/checkout, in another namespace and created after
// team-a/checkout.
var inLineKey = k8stypes.NamespacedName{Namespace: "team-b", Name: "checkout"}

// duplicateFixture is the hold fixture with two FluxClaims naming flux-system/checkout in the
// cluster of claimLocalProvider: team-a/checkout, created first, and team-b/checkout, an hour
// later. That KubeProvider admits both namespaces.
func duplicateFixture(t *testing.T) *holdFixture {
	t.Helper()
	first := testFluxClaim(claimLocalProvider)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	second := testFluxClaim(claimLocalProvider)
	second.Namespace = inLineKey.Namespace
	second.CreationTimestamp = metav1.NewTime(first.CreationTimestamp.Add(time.Hour))

	sim := applierSim(t, applierFluxObjects()...)
	c := inLineClient(t, configbutleraiv1alpha3.ApplierClaimKindFlux, first, second)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// inLineClient is a control cluster holding applier claims, in team-a and team-b, each referenced by an
// eligible GitTarget of its namespace, with the KubeProvider claimLocalProvider admitting both.
func inLineClient(t *testing.T, kind string, claims ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	objects := append([]ctrlclient.Object{admitting(claimLocalProvider, "team-a", "team-b")}, claims...)
	objects = append(objects, referencedIn("team-a", kind)...)
	objects = append(objects, referencedIn("team-b", kind)...)
	return claimTestClient(t, objects...)
}

// reconcileKey reconciles one applier claim a few times, enough to settle a suspend, and returns it.
func (f *holdFixture) reconcileKey(t *testing.T, key k8stypes.NamespacedName) *configbutleraiv1alpha3.FluxClaim {
	t.Helper()
	claim := &configbutleraiv1alpha3.FluxClaim{}
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, f.r.Get(context.Background(), key, claim))
		inControl := findCondition(claim.Status.Conditions, ApplierClaimConditionInControl)
		return inControl == nil || inControl.Reason != ReasonSuspending
	}, 5*time.Second, 5*time.Millisecond)
	return claim
}

// deleteUntilGone deletes an applier claim and reconciles it until its finalizer is gone.
func (f *holdFixture) deleteUntilGone(t *testing.T, key k8stypes.NamespacedName) {
	t.Helper()
	claim := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), key, claim))
	require.NoError(t, f.r.Delete(context.Background(), claim))
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		return apierrors.IsNotFound(f.r.Get(context.Background(), key, &configbutleraiv1alpha3.FluxClaim{}))
	}, 5*time.Second, 5*time.Millisecond, "the finalizer goes")
}

// TestFluxClaim_OneClaimHoldsAKustomization is review 5's finding 5: two FluxClaims naming one
// Kustomization both held it, under the same field manager. Each took the other's resume for an
// external one and suspended it mid-apply, and deleting either handed back the hold the other still
// needed. Now the one created first holds it; the other is Configured=False (HeldByAnotherClaim),
// naming the holder, and does nothing to the Kustomization, ever.
func TestFluxClaim_OneClaimHoldsAKustomization(t *testing.T) {
	f := duplicateFixture(t)

	second := f.reconcileKey(t, inLineKey)
	first := f.reconcileKey(t, claimKey)

	assert.True(t, conditionIsTrue(first.Status.Conditions, ApplierClaimConditionInControl), "the first holds it")
	assert.True(t, conditionIsTrue(first.Status.Conditions, ConditionTypeReady))
	configured := findCondition(second.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionFalse, configured.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, configured.Reason)
	assert.Equal(t, "Kustomization flux-system/checkout in the cluster of KubeProvider clusters/local is held by "+
		"FluxClaim team-a/checkout, which was created first; this applier claim takes it over once that one is deleted "+
		"or no eligible GitTarget references it", configured.Message)
	ready := findCondition(second.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, ready.Reason)
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeStalled), "a human removes one of them")
	inControl := findCondition(second.Status.Conditions, ApplierClaimConditionInControl)
	require.NotNil(t, inControl)
	assert.Equal(t, metav1.ConditionUnknown, inControl.Status)
	assert.Equal(t, ReasonNotConfigured, inControl.Reason)

	events := f.events()
	require.Len(t, events, 1, "one adoption, by the holder: %v", events)
	assert.True(t, strings.HasPrefix(events[0], "Normal Adopted"), events[0])

	// A GitTarget naming the applier claim in line says why it is not Ready.
	target := claimTarget(true)
	target.Namespace = inLineKey.Namespace
	gt := &GitTargetReconciler{Client: f.r.Client}
	verdict, _ := gt.observeApplierClaim(context.Background(), beginStatus(nil, nil, target), target,
		claimDestination())
	assert.Equal(t, metav1.ConditionFalse, verdict.value.Status)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, verdict.value.Reason)
	assert.Contains(t, verdict.value.Message, "held by FluxClaim team-a/checkout")
	assert.True(t, verdict.stalled)
}

// TestFluxClaim_TheHoldersResumeIsNoExternalResumeToTheOther: a branch worker's apply through the
// holder resumes the Kustomization; the applier claim in line does not take that for `flux resume`, and
// leaves it running.
func TestFluxClaim_TheHoldersResumeIsNoExternalResumeToTheOther(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, claimKey)
	f.reconcileKey(t, inLineKey)
	f.events()

	// The worker records its operation on the holder, and resumes the Kustomization.
	holder := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), claimKey, holder))
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
	assert.Nil(t, second.Status.Operation, "nothing recorded on the applier claim in line")
}

// TestFluxClaim_DeletingTheClaimInLineHandsNothingBack: the applier claim in line never held the
// Kustomization, so its finalizer goes without touching it: it stays suspended under GitOps
// Reverser's field manager, for the holder.
func TestFluxClaim_DeletingTheClaimInLineHandsNothingBack(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, claimKey)
	f.reconcileKey(t, inLineKey)
	f.events()

	f.deleteUntilGone(t, inLineKey)

	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks), "still held")
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager)
	assert.Empty(t, f.events(), "nothing released")
	holder := f.reconcileKey(t, claimKey)
	assert.True(t, conditionIsTrue(holder.Status.Conditions, ApplierClaimConditionInControl))
}

// TestFluxClaim_DeletingTheHolderHandsOverToTheNextInLine: the holder suspends the Kustomization as
// it goes, and leaves it suspended for the applier claim in line instead of releasing it: released, Flux
// would apply it at once, unchecked against the branch, before the next applier claim could suspend it
// again (review 6). Its going enqueues the applier claim in line (a deletion maps to the others naming the
// applier), which then holds the Kustomization as it was left, with nothing to adopt.
func TestFluxClaim_DeletingTheHolderHandsOverToTheNextInLine(t *testing.T) {
	f := duplicateFixture(t)
	f.reconcileKey(t, claimKey)
	f.reconcileKey(t, inLineKey)
	f.events()
	gone := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), claimKey, gone))
	applied := len(f.sim.Applied())

	f.deleteUntilGone(t, claimKey)
	ks := f.sim.Kustomization(checkoutKustomization)
	assert.True(t, suspendedSpec(ks), "never released between the two")
	assert.Contains(t, kustomizationManagers(ks), applier.SuspendFieldManager)
	assert.Len(t, f.sim.Applied(), applied, "so Flux applied nothing on its own")
	requests := f.r.claimsInLine(context.Background(), gone)
	require.Len(t, requests, 1)
	assert.Equal(t, inLineKey, requests[0].NamespacedName)
	assert.True(t, claimLineChanged().Delete(event.DeleteEvent{Object: gone}))
	assert.False(t, claimLineChanged().Update(event.UpdateEvent{ObjectOld: gone, ObjectNew: gone}))

	second := f.reconcileKey(t, inLineKey)

	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplierClaimConditionConfigured))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ApplierClaimConditionInControl))
	assert.True(t, conditionIsTrue(second.Status.Conditions, ConditionTypeReady))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	assert.Equal(t, []string{"Normal Released left Kustomization flux-system/checkout suspended for FluxClaim " +
		"team-b/checkout, which holds it next"}, f.events())
}

// TestFluxClaim_AHolderWithAnApplyInFlightKeepsIt is review 5's second finding (P2): an applier claim
// created in the same second as the holder, which comes before it by name, appears while the
// holder's apply is in flight. It waits: it is Configured=False, leaves the resumed Kustomization to
// the holder's worker, and records nothing. Once that apply ends, the holder still keeps it: an
// applier claim that holds its Kustomization comes before any that does not, so the Kustomization changes
// hands only once the holder lets go of it.
func TestFluxClaim_AHolderWithAnApplyInFlightKeepsIt(t *testing.T) {
	holderClaim := testFluxClaim(claimLocalProvider)
	holderClaim.Namespace = inLineKey.Namespace // team-b: after team-a by name
	holderClaim.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	holderKey := k8stypes.NamespacedName{Namespace: holderClaim.Namespace, Name: holderClaim.Name}
	sim := applierSim(t, applierFluxObjects()...)
	c := inLineClient(t, configbutleraiv1alpha3.ApplierClaimKindFlux, holderClaim)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	f := &holdFixture{r: r, sim: sim, recorder: recorder}
	f.reconcileKey(t, holderKey)
	f.events()

	// The holder's worker records its operation, and resumes the Kustomization.
	held := &configbutleraiv1alpha3.FluxClaim{}
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
	newcomer := testFluxClaim(claimLocalProvider)
	newcomer.CreationTimestamp = holderClaim.CreationTimestamp
	require.NoError(t, f.r.Create(context.Background(), newcomer))

	waiting := f.reconcileKey(t, claimKey)

	configured := findCondition(waiting.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, configured.Reason)
	assert.Contains(
		t,
		configured.Message,
		"whose apply is in flight; this applier claim takes it over once that apply ends",
	)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left running for the holder")
	assert.Nil(t, waiting.Status.Operation, "no external resume recorded")
	assert.Empty(t, f.events())
	assert.True(t, conditionIsTrue(f.reconcileKey(t, holderKey).Status.Conditions, ApplierClaimConditionConfigured),
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

	still := findCondition(f.reconcileKey(t, claimKey).Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, still)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, still.Reason)
	assert.Contains(t, still.Message, "which holds it now")
	assert.True(t, conditionIsTrue(f.reconcileKey(t, holderKey).Status.Conditions, ApplierClaimConditionConfigured),
		"the holder keeps it between applies too")
	assert.Empty(t, f.events(), "and nothing is adopted twice")
}

// TestArgoCDClaim_OneClaimHoldsAnApplication: two ArgoCDClaims on one Application would both
// start syncs; the one created later is Configured=False (HeldByAnotherClaim).
func TestArgoCDClaim_OneClaimHoldsAnApplication(t *testing.T) {
	first := testArgoCDClaim(claimLocalProvider)
	first.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	second := testArgoCDClaim(claimLocalProvider)
	second.Namespace = inLineKey.Namespace
	second.CreationTimestamp = first.CreationTimestamp
	c := inLineClient(t, configbutleraiv1alpha3.ApplierClaimKindArgoCD, first, second)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindArgoCD,
		applierFakeSource(t, applierArgoApplication(nil)), nil, true)

	for _, key := range []k8stypes.NamespacedName{claimKey, inLineKey} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}

	got := &configbutleraiv1alpha3.ArgoCDClaim{}
	require.NoError(t, c.Get(context.Background(), claimKey, got))
	assert.True(t, conditionIsTrue(got.Status.Conditions, ApplierClaimConditionConfigured),
		"created in the same second: the namespace decides")
	require.NoError(t, c.Get(context.Background(), inLineKey, got))
	configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, configured.Reason)
	assert.Contains(t, configured.Message, "Application argocd/checkout")
	assert.Contains(t, configured.Message, "held by ArgoCDClaim team-a/checkout")
}
