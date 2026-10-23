// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

var checkoutKustomization = k8stypes.NamespacedName{Namespace: "flux-system", Name: "checkout"}

// holdFixture is a FluxClaim team-a/checkout over a simulated Flux cluster holding objs, with
// Events recorded.
type holdFixture struct {
	r        *ApplierClaimReconciler
	sim      *fluxsim.Sim
	recorder *record.FakeRecorder
}

func newHoldFixture(t *testing.T, claim *configbutleraiv1alpha3.FluxClaim, objs ...runtime.Object) *holdFixture {
	t.Helper()
	if objs == nil {
		objs = applierFluxObjects()
	}
	sim := applierSim(t, objs...)
	r := claimReconciler(claimTestClient(t, claim), configbutleraiv1alpha3.ApplierClaimKindFlux, sim.Client, nil,
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
				EventReasonAdopted, EventReasonClaimedResourceReconciled, EventReasonReleased, EventReasonReleaseFailed,
				EventReasonOperationUnfollowed,
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

func (f *holdFixture) claim(t *testing.T) *configbutleraiv1alpha3.FluxClaim {
	t.Helper()
	claim := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), claimKey, claim))
	return claim
}

func suspendedSpec(ks *unstructured.Unstructured) bool {
	value, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	return value
}

// TestFluxClaim_AdoptsItsKustomization: a Configured applier claim suspends its Kustomization under GitOps
// Reverser's field manager, and is Ready only once that suspend's token is handled, so nothing that
// ran before the adoption is still running.
func TestFluxClaim_AdoptsItsKustomization(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))

	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)

	held := findCondition(got.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionTrue, held.Status, held.Message)
	assert.Equal(t, ReasonSuspended, held.Reason)
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

// TestFluxClaim_AnExternalResumeIsSuspendedAgain is decision 3: `flux resume` on a held
// Kustomization is suspended again with a token, every time, with a Normal Event each time, and
// recorded as an Unresolved operation that completes once nothing that resume started is running.
func TestFluxClaim_AnExternalResumeIsSuspendedAgain(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
	reconcileClaim(t, f.r)
	f.events()

	for round := range 2 {
		patch := []byte(`{"spec":{"suspend":false}}`)
		_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").
			Patch(context.Background(), "checkout", k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: "flux"})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(f.sim.Applied()) == round+1 }, 5*time.Second,
			time.Millisecond, "Flux applies the moment it is resumed")

		_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		recorded := f.claim(t).Status.Operation
		require.NotNil(t, recorded, "round %d", round)
		assert.Equal(t, configbutleraiv1alpha3.ApplyOperationUnresolved, recorded.Phase)
		assert.NotEmpty(t, recorded.SuspendToken)
		assert.Empty(t, recorded.Token, "no request of GitOps Reverser's")
		assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
		events := f.events()
		require.Len(t, events, 1, "an Event every time")
		assert.True(t, strings.HasPrefix(events[0], "Normal ClaimedResourceReconciled "+
			"Kustomization flux-system/checkout was resumed outside GitOps Reverser"), events[0])

		got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)
		require.NotNil(t, got.Status.Operation)
		assert.Equal(t, configbutleraiv1alpha3.ApplyOperationSucceeded, got.Status.Operation.Phase,
			"complete once the suspend's token is handled")
		assert.NotNil(t, got.Status.Operation.FinishedAt)
		assert.Equal(t, recorded.SuspendToken, got.Status.Operation.SuspendToken)
		assert.True(t, conditionIsTrue(got.Status.Conditions, ApplierClaimConditionHeld))
	}
	assert.Len(t, f.sim.Applied(), 2, "each resume applied once, unchecked; that is what the record says")
}

// TestFluxClaim_LeavesABranchWorkersOperationAlone: a worker records its operation before it
// resumes the Kustomization, so a resumed Kustomization with an operation of the worker's in flight
// is that operation, not an external resume; the worker suspends it again itself.
func TestFluxClaim_LeavesABranchWorkersOperationAlone(t *testing.T) {
	claim := testFluxClaim(claimLocalProvider)
	claim.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
	claim.Status.Conditions = []metav1.Condition{{
		Type: ApplierClaimConditionHeld, Status: metav1.ConditionTrue, Reason: ReasonSuspended,
		LastTransitionTime: metav1.Now(),
	}}
	claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonRefusal,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	f := newHoldFixture(t, claim)

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left resumed for the worker")
	assert.Empty(t, f.events())
	assert.Equal(t, "T1", f.claim(t).Status.Operation.Token)
}

// followerStub answers Following for the operations it lists, by token.
type followerStub map[string]bool

func (f followerStub) Following(_ git.ApplierClaimRef, token string) bool { return f[token] }

// workerOperationClaim is a held applier claim whose worker recorded operation T1 in flight, the
// Kustomization left resumed for it.
func workerOperationClaim() *configbutleraiv1alpha3.FluxClaim {
	claim := testFluxClaim(claimLocalProvider)
	claim.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
	claim.Status.Conditions = []metav1.Condition{{
		Type: ApplierClaimConditionHeld, Status: metav1.ConditionTrue, Reason: ReasonSuspended,
		LastTransitionTime: metav1.Now(),
	}}
	claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonRefusal,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	return claim
}

// TestFluxClaim_AnOperationNobodyFollowsIsSuspended is review 4's B backstop: the reconciler left a
// Kustomization resumed for any worker operation in flight, for ever, though nothing followed it
// (abandoned, undelivered, lost at startup). Now it leaves it only while this process follows the
// operation, or for a grace after it first saw nobody following it; past that it suspends the
// Kustomization, and completes the operation Failed once the suspend is handled.
func TestFluxClaim_AnOperationNobodyFollowsIsSuspended(t *testing.T) {
	f := newHoldFixture(t, workerOperationClaim())
	f.r.UnfollowedGrace = time.Nanosecond
	f.r.Follower = followerStub{"T1": true}

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "followed: left to the worker")
	applying := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), claimKey, applying))
	held := findCondition(applying.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionTrue, held.Status, "an operation of ours in flight is held")
	assert.Equal(t, ReasonApplying, held.Reason)

	f.r.Follower = followerStub{}
	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "followed by nobody: suspended")
	require.NotNil(t, got.Status.Operation)
	assert.Equal(t, configbutleraiv1alpha3.ApplyOperationFailed, got.Status.Operation.Phase)
	assert.Equal(t, "T1", got.Status.Operation.Token, "the worker's operation, completed")
	assert.NotEmpty(t, got.Status.Operation.SuspendToken)
	assert.NotNil(t, got.Status.Operation.FinishedAt)
	assert.Contains(t, got.Status.Operation.Message, "followed by nobody")
	assert.True(t, conditionIsTrue(got.Status.Conditions, ApplierClaimConditionHeld))
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning OperationUnfollowed the Refusal apply"), events[0])
}

// operationFlipper is an APIReader whose reads of the applier claim show its operation in flight until the
// Kustomization was read to be held (the second GET of it, after the Configured check), and completed
// after, as a worker that completes it in between leaves it.
type operationFlipper struct {
	ctrlclient.Reader

	kustomizationReads atomic.Int32
}

func (o *operationFlipper) Get(
	ctx context.Context, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption,
) error {
	if err := o.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if claim, ok := obj.(*configbutleraiv1alpha3.FluxClaim); ok && claim.Status.Operation != nil &&
		o.kustomizationReads.Load() >= 2 {
		claim.Status.Operation.Phase = configbutleraiv1alpha3.ApplyOperationSucceeded
	}
	return nil
}

// TestFluxClaim_AnOperationThatCompletesWhileReadIsNoExternalResume is review 4's F, the
// reconciler's half: the Kustomization read resumed for a worker's operation, which completed before
// the operation was read, looked like an external resume. Now the operation is read before and
// after the Kustomization, and a move between the two reads again.
func TestFluxClaim_AnOperationThatCompletesWhileReadIsNoExternalResume(t *testing.T) {
	f := newHoldFixture(t, workerOperationClaim())
	flipper := &operationFlipper{Reader: f.r.Client}
	f.r.APIReader = flipper
	f.sim.Client.PrependReactor("get", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		flipper.kustomizationReads.Add(1)
		return false, nil, nil
	})

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "nothing suspended on a moving read")
	assert.Empty(t, f.events(), "and no external resume recorded")
	assert.Equal(t, "T1", f.claim(t).Status.Operation.Token)
}

// TestFluxClaim_ASuspendAppliedElsewhereIsNeverHeld: a Kustomization whose manifest sets
// spec.suspend, applied by another manager, is Configured=False and is never suspended: the next
// apply of that manifest would resume it.
func TestFluxClaim_ASuspendAppliedElsewhereIsNeverHeld(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), false, git.FluxFieldManager,
		metav1.ManagedFieldsOperationApply)
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider), ks, objs[1])

	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)

	configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonSuspendOwnedElsewhere, configured.Reason)
	assert.True(t, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled), "a human fixes the manifest")
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
}

// TestFluxClaim_DeletionHandsTheKustomizationBack: deleting the applier claim suspends its Kustomization
// once more when it is resumed, waits until that suspend is handled, and then releases spec.suspend,
// so Flux applies on its own again; then the finalizer goes.
func TestFluxClaim_DeletionHandsTheKustomizationBack(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
	reconcileClaim(t, f.r)
	f.events()
	// A worker's apply is running as the applier claim is deleted.
	patch := []byte(`{"spec":{"suspend":false}}`)
	_, err := f.sim.Client.Resource(fluxsim.KustomizationGVR()).Namespace("flux-system").Patch(context.Background(),
		"checkout", k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: applier.SuspendFieldManager})
	require.NoError(t, err)
	require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))

	require.Eventually(t, func() bool {
		result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		if err := f.r.Get(
			context.Background(),
			claimKey,
			&configbutleraiv1alpha3.FluxClaim{},
		); apierrors.IsNotFound(
			err,
		) {
			return true
		}
		assert.Equal(t, applierClaimSuspendingInterval, result.RequeueAfter)
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

// TestFluxClaim_DeletionLetsGoOfWhatIsGone: a Kustomization or a KubeProvider that is gone has
// nothing to hand back, and must not hold the applier claim.
func TestFluxClaim_DeletionLetsGoOfWhatIsGone(t *testing.T) {
	for name, cluster := range map[string]string{
		"the Kustomization is gone": claimLocalProvider,
		"the KubeProvider is gone":  "absent",
	} {
		t.Run(name, func(t *testing.T) {
			claim := testFluxClaim(cluster)
			claim.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
			sim := applierSim(t)
			r := claimReconciler(claimTestClient(t, claim), configbutleraiv1alpha3.ApplierClaimKindFlux,
				sim.Client, nil, true)
			r.ApplierClients = func(context.Context, string) (dynamic.Interface, error) { return sim.Client, nil }
			require.NoError(t, r.Delete(context.Background(), claim))

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
			require.NoError(t, err)
			err = r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
			assert.True(t, apierrors.IsNotFound(err), "the finalizer is gone: %v", err)
		})
	}
}

// TestFluxClaim_AGoneKubeProviderStillHandsBack is review 7's C1: a folder holding both a
// KubeProvider and a FluxClaim is removed, and the provider goes first. The hand-back was
// skipped with an Info log line, and the Kustomization stayed suspended under gitops-reverser for
// good, though for an inCluster provider its cluster is still reachable. Now the release
// goes on through whatever client still resolves, and only when none does is the applier claim let go,
// with a Warning ReleaseFailed saying the Kustomization is left suspended.
func TestFluxClaim_AGoneKubeProviderStillHandsBack(t *testing.T) {
	held := func(t *testing.T) *holdFixture {
		t.Helper()
		f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
		reconcileClaim(t, f.r)
		f.events()
		require.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
		provider := &configbutleraiv1alpha3.KubeProvider{}
		require.NoError(t, f.r.Get(context.Background(),
			ctrlclient.ObjectKey{Namespace: claimProviderNamespace, Name: claimLocalProvider}, provider))
		require.NoError(t, f.r.Delete(context.Background(), provider))
		require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))
		return f
	}
	gone := func(t *testing.T, f *holdFixture) {
		t.Helper()
		require.Eventually(t, func() bool {
			_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
			require.NoError(t, err)
			err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
			return apierrors.IsNotFound(err)
		}, 5*time.Second, 5*time.Millisecond, "the finalizer goes")
	}

	t.Run("its cluster still answers, so the Kustomization is handed back", func(t *testing.T) {
		f := held(t)
		gone(t, f)
		assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "handed back to Flux")
		events := f.events()
		require.Len(t, events, 1)
		assert.True(t, strings.HasPrefix(events[0], "Normal Released handed Kustomization flux-system/checkout back"),
			events[0])
	})
	t.Run("its cluster no longer answers, so the applier claim lets go loudly", func(t *testing.T) {
		f := held(t)
		f.r.ApplierClients = func(context.Context, string) (dynamic.Interface, error) {
			return nil, errors.New(`KubeProvider clusters/local not found`)
		}
		gone(t, f)
		assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left suspended, as the Event says")
		events := f.events()
		require.Len(t, events, 1)
		assert.True(t, strings.HasPrefix(events[0], "Warning ReleaseFailed could not hand Kustomization "+
			"flux-system/checkout back to Flux: its KubeProvider clusters/local is gone; it is left suspended, "+
			"and `flux resume` resumes it"), events[0])
	})
}

// TestFluxClaim_TakesOverASuspendBySomebodyElse: a Kustomization `flux suspend` suspended is not
// held: that write may have let a reconcile run, so the applier claim takes the field over with a token of
// its own and is Ready once it is handled.
func TestFluxClaim_TakesOverASuspendBySomebodyElse(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, "flux",
		metav1.ManagedFieldsOperationUpdate)
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider), ks, objs[1])

	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)

	assert.True(t, conditionIsTrue(got.Status.Conditions, ApplierClaimConditionHeld))
	assert.Contains(t, kustomizationManagers(f.sim.Kustomization(checkoutKustomization)), applier.SuspendFieldManager)
	assert.Empty(t, f.events(), "neither an adoption of a running Kustomization nor an external resume")
}

// TestFluxClaim_AHandBackItMayNotMakeLetsGo: an identity that may not write the Kustomization
// cannot hand it back; the applier claim goes anyway, with a Warning that says what is left behind.
func TestFluxClaim_AHandBackItMayNotMakeLetsGo(t *testing.T) {
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider))
	reconcileClaim(t, f.r)
	f.events()
	f.sim.Client.PrependReactor("patch", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(fluxsim.KustomizationGVR().GroupResource(), "checkout",
			errors.New("no patch for you"))
	})
	require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
	assert.True(t, apierrors.IsNotFound(err), "the applier claim is let go: %v", err)
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

// TestFluxClaim_PublishesTheSourcesOtherConsumers is decision 2: the consumers of the
// GitRepository that no FluxClaim holds are listed, information only.
func TestFluxClaim_PublishesTheSourcesOtherConsumers(t *testing.T) {
	objs := applierFluxObjects()
	infra := objs[0].(*unstructured.Unstructured).DeepCopy()
	infra.SetName("infra")
	release := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"namespace": "apps", "name": "podinfo"},
		"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{
			"chart":     "./charts/podinfo",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "platform", "namespace": "flux-system"},
		}}},
	}}
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider),
		append(objs, infra, release)...)

	source := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim).Status.Source
	require.NotNil(t, source)
	assert.Equal(t, []meta.NamespacedObjectKindReference{
		{Kind: "HelmRelease", Namespace: "apps", Name: "podinfo"},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "infra"},
	}, source.OtherConsumers, "itself is not another consumer")
	assert.True(t, conditionIsTrue(f.claim(t).Status.Conditions, ConditionTypeReady), "information only")

	// The fourth review's finding C: an applier claim holds its Kustomization only once it says so
	// (Held=True). One that is not Configured (SuspendOwnedElsewhere), or still suspending, leaves
	// a Kustomization Flux applies on its own.
	infraClaim := testFluxClaim(claimLocalProvider)
	infraClaim.Name, infraClaim.Spec.KustomizationRef.Name = "infra", "infra"
	require.NoError(t, f.r.Create(context.Background(), infraClaim))
	infraOther := []meta.NamespacedObjectKindReference{
		{Kind: "HelmRelease", Namespace: "apps", Name: "podinfo"},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "infra"},
	}
	for _, suspended := range []metav1.Condition{
		{Type: ApplierClaimConditionHeld, Status: metav1.ConditionUnknown, Reason: ReasonNotConfigured},
		{Type: ApplierClaimConditionHeld, Status: metav1.ConditionFalse, Reason: ReasonSuspending},
	} {
		setClaimCondition(t, f.r, "infra", suspended)
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		assert.Equal(t, infraOther, f.claim(t).Status.Source.OtherConsumers,
			"an applier claim that is %s does not hold its Kustomization", suspended.Reason)
	}

	setClaimCondition(t, f.r, "infra", metav1.Condition{
		Type: ApplierClaimConditionHeld, Status: metav1.ConditionTrue, Reason: ReasonSuspended,
	})
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.Equal(t, []meta.NamespacedObjectKindReference{{Kind: "HelmRelease", Namespace: "apps", Name: "podinfo"}},
		f.claim(t).Status.Source.OtherConsumers, "a Kustomization another applier claim holds is not listed")

	require.NoError(t, f.sim.Client.Resource(k8sschema.GroupVersionResource{
		Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases",
	}).Namespace("apps").Delete(context.Background(), "podinfo", metav1.DeleteOptions{}))
	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.Empty(t, f.claim(t).Status.Source.OtherConsumers, "every consumer is held")
}

// setClaimCondition publishes a condition on the FluxClaim team-a/name.
func setClaimCondition(t *testing.T, c ctrlclient.Client, name string, condition metav1.Condition) {
	t.Helper()
	claim := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, c.Get(context.Background(), k8stypes.NamespacedName{Namespace: "team-a", Name: name}, claim))
	condition.LastTransitionTime = metav1.Now()
	claim.Status.Conditions = []metav1.Condition{condition}
	require.NoError(t, c.Status().Update(context.Background(), claim))
}

// TestFluxClaim_CompletesTheLiveExternalOperation is the fourth review's finding K: the operation
// an external resume was recorded as is completed from the uncached read, which is what the hold
// decided on. The cached applier claim may not show it yet, and completing that one dereferenced nil.
func TestFluxClaim_CompletesTheLiveExternalOperation(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, applier.SuspendFieldManager,
		metav1.ManagedFieldsOperationApply)
	ks.SetAnnotations(map[string]string{meta.ReconcileRequestAnnotation: "T2"})
	require.NoError(t, unstructured.SetNestedField(ks.Object, "T2", "status", "lastHandledReconcileAt"))
	cached := testFluxClaim(claimLocalProvider)
	cached.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
	f := newHoldFixture(t, cached, ks, objs[1])
	live := cached.DeepCopy()
	live.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, SuspendToken: "T2", RequestedAt: metav1.Now(),
	}
	f.r.APIReader = claimTestClient(t, live)

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)

	got := f.claim(t)
	require.NotNil(t, got.Status.Operation, "the live operation is completed")
	assert.True(t, got.Status.Operation.Phase.Completed(), got.Status.Operation.Phase)
	assert.Equal(t, "T2", got.Status.Operation.SuspendToken)
	assert.NotNil(t, got.Status.Operation.FinishedAt)
}

// TestFluxClaim_ASuspendItMayNotMakeLetsGo is the fourth review's finding D: a resumed
// Kustomization the identity may not suspend again cannot be handed back either, so the applier claim goes,
// with a Warning, as it does for a release it may not make. It used to retry forever.
func TestFluxClaim_ASuspendItMayNotMakeLetsGo(t *testing.T) {
	objs := applierFluxObjects()
	ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), false, applier.SuspendFieldManager,
		metav1.ManagedFieldsOperationApply)
	claim := testFluxClaim(claimLocalProvider)
	claim.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
	f := newHoldFixture(t, claim, ks, objs[1])
	f.sim.Client.PrependReactor("patch", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(fluxsim.KustomizationGVR().GroupResource(), "checkout",
			errors.New("no patch for you"))
	})
	require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
	assert.True(t, apierrors.IsNotFound(err), "the applier claim is let go: %v", err)
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning ReleaseFailed could not hand Kustomization"), events[0])
}

// TestFluxClaim_AHandBackThatNeverEndsLetsGoAtTheDeadline is the fourth review's finding D: with
// kustomize-controller down, the suspend of a hand-back is never handled. The applier claim waits for it
// until releaseDeadline after its deletion, and then goes, with a Warning that says what the
// Kustomization is left as.
func TestFluxClaim_AHandBackThatNeverEndsLetsGoAtTheDeadline(t *testing.T) {
	for name, deleted := range map[string]time.Duration{
		"before the deadline": 0,
		"past the deadline":   releaseDeadline + time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			objs := applierFluxObjects()
			ks := fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, applier.SuspendFieldManager,
				metav1.ManagedFieldsOperationApply)
			// No source: the simulated kustomize-controller never reconciles it, as one that is down.
			unstructured.RemoveNestedField(ks.Object, "spec", "sourceRef")
			ks.SetAnnotations(map[string]string{meta.ReconcileRequestAnnotation: "T2"})
			claim := testFluxClaim(claimLocalProvider)
			claim.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
			claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			// The deadline counts from when Held turned Releasing, not from the deletion.
			claim.Status.Conditions = []metav1.Condition{{
				Type: ApplierClaimConditionHeld, Status: metav1.ConditionFalse, Reason: ReasonReleasing,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-deleted)),
			}}
			f := newHoldFixture(t, claim, ks, objs[1])

			result, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
			require.NoError(t, err)
			err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
			events := f.events()
			if deleted == 0 {
				require.NoError(t, err, "still waiting for the suspend")
				assert.Equal(t, applierClaimSuspendingInterval, result.RequeueAfter)
				assert.Empty(t, events)
				return
			}
			assert.True(t, apierrors.IsNotFound(err), "the applier claim is let go: %v", err)
			require.Len(t, events, 1)
			assert.True(t, strings.HasPrefix(events[0], "Warning ReleaseFailed gave up handing Kustomization "+
				"flux-system/checkout back to Flux 10m0s after it began: it is suspended, and has "+
				"not handled the suspend yet"), events[0])
			assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)),
				"left suspended, as the Event says")
		})
	}
}

// TestFluxClaim_HandsBackASuspendedKustomizationSuspended: adoption records how the
// Kustomization stood in status.previousSuspendState. One somebody had suspended before the applier claim held it is
// handed back suspended, as it was, and not resumed; one that was running is resumed, as before.
func TestFluxClaim_HandsBackASuspendedKustomizationSuspended(t *testing.T) {
	for name, wasSuspended := range map[string]bool{"suspended before": true, "running before": false} {
		t.Run(name, func(t *testing.T) {
			objs := applierFluxObjects()
			if wasSuspended {
				objs[0] = fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, "flux",
					metav1.ManagedFieldsOperationUpdate)
			}
			f := newHoldFixture(t, testFluxClaim(claimLocalProvider), objs...)
			got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)
			require.Equal(t, suspendState(wasSuspended), got.Status.PreviousSuspendState)
			f.events()

			require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))
			require.Eventually(t, func() bool {
				_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
				require.NoError(t, err)
				err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
				return apierrors.IsNotFound(err)
			}, 5*time.Second, 5*time.Millisecond, "the finalizer goes once the Kustomization is handed back")

			ks := f.sim.Kustomization(checkoutKustomization)
			assert.Equal(t, wasSuspended, suspendedSpec(ks))
			assert.NotContains(t, ks.GetAnnotations(), meta.ReconcileRequestAnnotation,
				"GitOps Reverser's requestedAt is given up either way")
			events := f.events()
			require.Len(t, events, 1)
			want := "it is resumed, and applies on its own again"
			if wasSuspended {
				want = "it stays suspended, as it was before GitOps Reverser held it"
			}
			assert.Contains(t, events[0], want)
		})
	}
}

// commitFailingClient refuses the next failures status writes that carry conditions: the commit a
// reconcile makes after its writes to the applier.
type commitFailingClient struct {
	ctrlclient.Client

	failures int
}

func (c *commitFailingClient) Status() ctrlclient.SubResourceWriter {
	return commitFailingWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type commitFailingWriter struct {
	ctrlclient.SubResourceWriter

	c *commitFailingClient
}

func (w commitFailingWriter) Patch(
	ctx context.Context, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.SubResourcePatchOption,
) error {
	if data, err := patch.Data(obj); err == nil && strings.Contains(string(data), `"conditions"`) && w.c.failures > 0 {
		w.c.failures--
		return errors.New("status write refused")
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// TestFluxClaim_RecordsHowItStoodBeforeTheSuspend: status.previousSuspendState is written before
// the suspend that hides it. With the commit after the suspend lost, the next reconcile finds the
// Kustomization held and cannot read how it stood, yet the hand-back still leaves it suspended.
func TestFluxClaim_RecordsHowItStoodBeforeTheSuspend(t *testing.T) {
	objs := applierFluxObjects()
	objs[0] = fluxsim.OwnSuspend(t, objs[0].(*unstructured.Unstructured), true, "flux",
		metav1.ManagedFieldsOperationUpdate)
	f := newHoldFixture(t, testFluxClaim(claimLocalProvider), objs...)
	f.r.Client = &commitFailingClient{Client: f.r.Client, failures: 1}

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.Error(t, err, "the commit after the suspend is lost")
	assert.Contains(t, kustomizationManagers(f.sim.Kustomization(checkoutKustomization)), applier.SuspendFieldManager)
	assert.Equal(t, configbutleraiv1alpha3.SuspendStateSuspended, f.claim(t).Status.PreviousSuspendState,
		"recorded before the suspend")

	reconcileClaim(t, f.r)
	require.NoError(t, f.r.Delete(context.Background(), f.claim(t)))
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		err = f.r.Get(context.Background(), claimKey, &configbutleraiv1alpha3.FluxClaim{})
		return apierrors.IsNotFound(err)
	}, 5*time.Second, 5*time.Millisecond, "the finalizer goes once the Kustomization is handed back")
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "suspended, as it was")
}

// TestFluxClaim_RecordsNothingOfAKustomizationAnotherHolds: an applier claim Configured from a cached read
// that finds its Kustomization resumed, and another applier claim holding it on the uncached read, adopts
// nothing, so it records nothing: the resume is the holder's apply, not how the Kustomization stood.
func TestFluxClaim_RecordsNothingOfAKustomizationAnotherHolds(t *testing.T) {
	claim := testFluxClaim(claimLocalProvider)
	claim.CreationTimestamp = metav1.NewTime(time.Unix(2_000_000, 0))
	f := newHoldFixture(t, claim)
	holder := testFluxClaim(claimLocalProvider)
	holder.Name = "checkout-first"
	holder.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	holder.Finalizers = []string{configbutleraiv1alpha3.FluxClaimFinalizer}
	f.r.APIReader = claimTestClient(t, claim.DeepCopy(), holder)

	got := reconcileClaim(t, f.r).(*configbutleraiv1alpha3.FluxClaim)

	held := findCondition(got.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, ReasonNotConfigured, held.Reason)
	assert.Contains(t, held.Message, "team-a/checkout-first")
	assert.Empty(t, got.Status.PreviousSuspendState)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "nothing written")
}
