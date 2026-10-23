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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// The rule (review 7): an applier claim acts only while an eligible GitTarget references it. These tests
// pin its three cases (never referenced or released, referenced, retiring), the binding verdict, and
// the line of applier claims for one applier.

// ruleFixture is the hold fixture over a control cluster holding objects, the KubeProviders and
// the GitProvider platform, and no GitTarget unless objects carries one.
func ruleFixture(t *testing.T, objects ...ctrlclient.Object) *holdFixture {
	t.Helper()
	sim := applierSim(t, applierFluxObjects()...)
	c := claimTestClient(t, append(objects, unreferenced{})...)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// fluxTarget is the GitTarget team-a/checkout-flux naming the FluxClaim checkout, writing main.
func fluxTarget() *configbutleraiv1alpha3.GitTarget {
	return referencingTarget("checkout-flux", configbutleraiv1alpha3.ApplierClaimKindFlux)
}

// settle reconciles the applier claim at key until its Held condition is neither Suspending nor
// Releasing, and returns it.
func (f *holdFixture) settle(t *testing.T, key k8stypes.NamespacedName) *configbutleraiv1alpha3.FluxClaim {
	t.Helper()
	claim := &configbutleraiv1alpha3.FluxClaim{}
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, f.r.Get(context.Background(), key, claim))
		held := findCondition(claim.Status.Conditions, ApplierClaimConditionHeld)
		return held != nil && held.Reason != ReasonSuspending && held.Reason != ReasonReleasing
	}, 5*time.Second, 5*time.Millisecond)
	return claim
}

// heldReason is the applier claim's Held reason.
func heldReason(t *testing.T, claim *configbutleraiv1alpha3.FluxClaim) string {
	t.Helper()
	held := findCondition(claim.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	return held.Reason
}

// TestApplierClaimRule_AnUnreferencedClaimDoesNothingAndIsReady: no call reaches the applier's
// cluster, nothing is held, no finalizer is added, and the applier claim is Ready, so a Flux health check
// that waits on it does not wait for the GitTarget that will name it.
func TestApplierClaimRule_AnUnreferencedClaimDoesNothingAndIsReady(t *testing.T) {
	for _, kind := range []string{configbutleraiv1alpha3.ApplierClaimKindFlux, configbutleraiv1alpha3.ApplierClaimKindArgoCD} {
		t.Run(kind, func(t *testing.T) {
			var claim ctrlclient.Object = testFluxClaim(claimLocalProvider)
			if kind == configbutleraiv1alpha3.ApplierClaimKindArgoCD {
				claim = testArgoCDClaim(claimLocalProvider)
			}
			r := claimReconciler(claimTestClient(t, claim, unreferenced{}), kind, nil, nil, true)
			calls := 0
			r.ApplierClients = func(context.Context, string) (dynamic.Interface, error) {
				calls++
				return nil, errors.New("the applier's cluster must not be asked")
			}

			got := reconcileClaim(t, r)

			assert.Zero(t, calls, "nothing is read in the applier's cluster")
			configured := findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, metav1.ConditionUnknown, configured.Status)
			assert.Equal(t, applier.ReasonNotReferenced, configured.Reason)
			ready := findCondition(*got.StatusConditions(), ConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionTrue, ready.Status)
			assert.Equal(t, applier.ReasonNotReferenced, ready.Reason)
			assert.False(t, conditionIsTrue(*got.StatusConditions(), ConditionTypeReconciling))
			assert.Empty(t, got.GetFinalizers())
			url, _, known := got.SourceBinding()
			assert.False(t, known, "no source is published: %s", url)
			if flux, ok := got.(*configbutleraiv1alpha3.FluxClaim); ok {
				assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, flux))
			}
		})
	}
}

// TestApplierClaimRule_TheFirstEligibleReferenceAdoptsAndTheLastReleases: naming the applier claim is the
// adoption moment; the GitTarget going hands the Kustomization back, and the applier claim is idle again.
func TestApplierClaimRule_TheFirstEligibleReferenceAdoptsAndTheLastReleases(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider))
	idle := f.settle(t, claimKey)
	assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, idle))
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "Flux applies it on its own")

	target := fluxTarget()
	require.NoError(t, f.r.Create(context.Background(), target))
	held := f.settle(t, claimKey)
	assert.Equal(t, ReasonSuspended, heldReason(t, held))
	assert.True(t, conditionIsTrue(held.Status.Conditions, ConditionTypeReady))
	assert.True(t, controllerutil.ContainsFinalizer(held, configbutleraiv1alpha3.FluxClaimFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Adopted"), events[0])

	require.NoError(t, f.r.Delete(context.Background(), target))
	released := f.settle(t, claimKey)
	assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, released))
	ready := findCondition(released.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, applier.ReasonNotReferenced, ready.Reason)
	assert.Empty(t, released.Finalizers, "an applier claim that holds nothing deletes at once")
	assert.Nil(t, released.Status.Source)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "handed back to Flux")
	events = f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released handed Kustomization flux-system/checkout back"),
		events[0])
}

// TestApplierClaimRule_ARecreatedGitTargetReleasesAndReadopts is the trade-off the rule accepts:
// deleting and re-creating the GitTarget hands the Kustomization back, and adopts it again.
func TestApplierClaimRule_ARecreatedGitTargetReleasesAndReadopts(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider), fluxTarget())
	f.settle(t, claimKey)
	f.events()

	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))
	f.settle(t, claimKey)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	require.NoError(t, f.r.Create(context.Background(), fluxTarget()))
	again := f.settle(t, claimKey)

	assert.Equal(t, ReasonSuspended, heldReason(t, again))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	var reasons []string
	for _, event := range f.events() {
		reasons = append(reasons, strings.Fields(event)[1])
	}
	assert.Equal(t, []string{EventReasonReleased, EventReasonAdopted}, reasons)
}

// TestApplierClaimRule_TheBindingAggregatesItsGitTargets: one eligible GitTarget among mismatched
// ones binds the applier claim; none that can be judged yet is progressing; every one refused is stalled,
// with the reason they share, or NotBound for mixed ones, and each GitTarget in the message.
func TestApplierClaimRule_TheBindingAggregatesItsGitTargets(t *testing.T) {
	onBranch := func(name, branch string) *configbutleraiv1alpha3.GitTarget {
		target := referencingTarget(name, configbutleraiv1alpha3.ApplierClaimKindFlux)
		target.Spec.GitBranchRef.Name = gitBranchNameForTest("platform", branch)
		return target
	}
	elsewhere := onBranch("elsewhere", "main")
	elsewhere.Spec.KubeProviderRef = claimProviderRef("closed")
	cases := []struct {
		name        string
		objects     []ctrlclient.Object
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
		wantNames   []string
	}{
		{name: "one eligible among mismatches holds", wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded,
			objects: []ctrlclient.Object{onBranch("release", "release"), onBranch("main", "main")}},
		{name: "none that can be judged yet is progressing", wantStatus: metav1.ConditionUnknown,
			wantReason: applier.ReasonNotBound, wantNames: []string{"team-a/unprovided"},
			objects: []ctrlclient.Object{func() ctrlclient.Object {
				target := onBranch("unprovided", "main")
				target.Spec.GitBranchRef.Name = "absent"
				return target
			}(), onBranch("release", "release")}},
		{name: "every one mismatched is stalled, with the shared reason", wantStatus: metav1.ConditionFalse,
			wantReason: applier.ReasonSourceMismatch, wantStalled: true,
			wantNames: []string{"team-a/release", "team-a/staging"},
			objects:   []ctrlclient.Object{onBranch("release", "release"), onBranch("staging", "staging")}},
		{name: "refused for different reasons is stalled, NotBound", wantStatus: metav1.ConditionFalse,
			wantReason: applier.ReasonNotBound, wantStalled: true,
			wantNames: []string{"team-a/elsewhere", "team-a/release"},
			objects: []ctrlclient.Object{onBranch("release", "release"), elsewhere,
				admitting("closed", "team-b")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]ctrlclient.Object{testFluxClaim(claimLocalProvider),
				gitBranchOn("platform", "team-a", "release"), gitBranchOn("platform", "team-a", "staging")},
				tc.objects...)
			f := ruleFixture(t, objects...)

			got := f.settle(t, claimKey)

			configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(got.Status.Conditions, ConditionTypeStalled))
			for _, name := range tc.wantNames {
				assert.Contains(t, configured.Message, "GitTarget "+name+":")
			}
			assert.Equal(t, tc.wantStatus == metav1.ConditionTrue,
				suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "held only once bound")
			assert.Equal(t, tc.wantStatus == metav1.ConditionTrue, got.Status.Source != nil,
				"the source is published only once bound")
		})
	}
}

// TestApplierClaimRule_AnOlderClaimNotInLineKeepsNoneOut: an older applier claim whose GitTarget writes
// another branch, or whose namespace is not admitted, no longer keeps a younger eligible one from
// holding the Kustomization.
func TestApplierClaimRule_AnOlderClaimNotInLineKeepsNoneOut(t *testing.T) {
	for name, older := range map[string]func(*configbutleraiv1alpha3.GitTarget, *[]ctrlclient.Object){
		"bound to another branch": func(target *configbutleraiv1alpha3.GitTarget, objects *[]ctrlclient.Object) {
			target.Spec.GitBranchRef.Name = gitBranchNameForTest("platform", "release")
			*objects = append(*objects, gitBranchOn("platform", target.Namespace, "release"))
		},
		"not admitted": func(target *configbutleraiv1alpha3.GitTarget, objects *[]ctrlclient.Object) {
			target.Spec.KubeProviderRef = claimProviderRef("closed")
			*objects = append(*objects, admitting("closed", "team-a"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			stray := testFluxClaim(claimLocalProvider)
			stray.Namespace = inLineKey.Namespace
			stray.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
			mine := testFluxClaim(claimLocalProvider)
			mine.CreationTimestamp = metav1.NewTime(stray.CreationTimestamp.Add(time.Hour))
			objects := []ctrlclient.Object{stray, mine}
			strayRefs := referencedIn(inLineKey.Namespace, configbutleraiv1alpha3.ApplierClaimKindFlux)
			older(strayRefs[1].(*configbutleraiv1alpha3.GitTarget), &objects)
			objects = append(objects, strayRefs...)
			objects = append(objects, referencedIn("team-a", configbutleraiv1alpha3.ApplierClaimKindFlux)...)
			objects = append(objects, admitting(claimLocalProvider, "team-a", "team-b"))
			f := ruleFixture(t, objects...)

			got := f.settle(t, claimKey)

			assert.True(t, conditionIsTrue(got.Status.Conditions, ApplierClaimConditionConfigured),
				findCondition(got.Status.Conditions, ApplierClaimConditionConfigured).Message)
			assert.Equal(t, ReasonSuspended, heldReason(t, got))
			strayConfigured := findCondition(f.settle(t, inLineKey).Status.Conditions, ApplierClaimConditionConfigured)
			require.NotNil(t, strayConfigured)
			assert.NotEqual(t, metav1.ConditionTrue, strayConfigured.Status)
			assert.NotEqual(t, applier.ReasonHeldByAnotherClaim, strayConfigured.Reason,
				"it is refused for its own binding, before the line is asked")
		})
	}
}

// TestApplierClaimRule_ARetiringHolderStaysInLineUntilItsReleaseIsDone: the holder's GitTarget goes
// while a successor waits. Until the holder has suspended the Kustomization and seen that handled,
// it stays first in line, Held=False (Releasing), and the successor does nothing; then the holder
// leaves the Kustomization suspended for it, and the successor takes it over.
func TestApplierClaimRule_ARetiringHolderStaysInLineUntilItsReleaseIsDone(t *testing.T) {
	holder := testFluxClaim(claimLocalProvider)
	holder.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	successor := testFluxClaim(claimLocalProvider)
	successor.Namespace = inLineKey.Namespace
	successor.CreationTimestamp = metav1.NewTime(holder.CreationTimestamp.Add(time.Hour))
	objects := []ctrlclient.Object{holder, successor,
		admitting(claimLocalProvider, "team-a", "team-b")}
	objects = append(objects, referencedIn("team-a", configbutleraiv1alpha3.ApplierClaimKindFlux)...)
	objects = append(objects, referencedIn("team-b", configbutleraiv1alpha3.ApplierClaimKindFlux)...)
	f := ruleFixture(t, objects...)
	f.settle(t, claimKey)
	f.settle(t, inLineKey)
	f.events()

	// A worker's apply has the Kustomization resumed, and kustomize-controller is busy.
	resume(t, f)
	unpause := f.sim.PauseKustomizations()
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-fluxclaim"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	retiring := f.claim(t)
	assert.Equal(t, ReasonReleasing, heldReason(t, retiring))
	assert.True(t, controllerutil.ContainsFinalizer(retiring, configbutleraiv1alpha3.FluxClaimFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "suspended first")
	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: inLineKey})
	require.NoError(t, err)
	waiting := &configbutleraiv1alpha3.FluxClaim{}
	require.NoError(t, f.r.Get(context.Background(), inLineKey, waiting))
	configured := findCondition(waiting.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherClaim, configured.Reason, "the retiring holder is still first")
	assert.Contains(t, configured.Message, "which holds it now")

	unpause()
	done := f.settle(t, claimKey)
	assert.Empty(t, done.Finalizers, "handed over")
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left suspended for the successor")
	taken := f.settle(t, inLineKey)
	assert.True(t, conditionIsTrue(taken.Status.Conditions, ApplierClaimConditionConfigured))
	assert.Equal(t, ReasonSuspended, heldReason(t, taken))
	assert.Equal(t, configbutleraiv1alpha3.SuspendStateNotSuspended, taken.Status.PreviousSuspendState,
		"passed on by the holder: the successor took the Kustomization over suspended already")
	events := f.events()
	require.NotEmpty(t, events)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released left Kustomization flux-system/checkout suspended "+
		"for FluxClaim team-b/checkout"), events[0])
}

// resume resumes the Kustomization as a branch worker's apply does, under GitOps Reverser's field
// manager.
func resume(t *testing.T, f *holdFixture) {
	t.Helper()
	f.sim.Edit(checkoutKustomization, func(ks *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(ks.Object, false, "spec", "suspend")
	})
}

// TestApplierClaimRule_AReferenceReturningDuringTheReleaseHoldsAgain: the GitTarget is gone, and
// back, before the release finished: the applier claim holds the Kustomization again, as it is, and
// hands nothing back.
func TestApplierClaimRule_AReferenceReturningDuringTheReleaseHoldsAgain(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider), fluxTarget())
	f.settle(t, claimKey)
	f.events()
	resume(t, f)
	unpause := f.sim.PauseKustomizations()
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	assert.Equal(t, ReasonReleasing, heldReason(t, f.claim(t)))

	require.NoError(t, f.r.Create(context.Background(), fluxTarget()))
	unpause()
	back := f.settle(t, claimKey)

	assert.Equal(t, ReasonSuspended, heldReason(t, back))
	assert.True(t, controllerutil.ContainsFinalizer(back, configbutleraiv1alpha3.FluxClaimFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "never handed back")
	for _, event := range f.events() {
		assert.NotContains(t, event, EventReasonReleased)
	}
}

// TestApplierClaimRule_ReleasingWithAnApplyInFlightSuspendsFirst: the last GitTarget goes while an
// apply has the Kustomization resumed, followed by nobody here. The release suspends it, waits until
// that is handled, then hands it back, and completes the operation nobody would complete.
func TestApplierClaimRule_ReleasingWithAnApplyInFlightSuspendsFirst(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider), fluxTarget())
	f.settle(t, claimKey)
	f.events()
	claim := f.claim(t)
	claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonCatchUp,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	require.NoError(t, f.r.Status().Update(context.Background(), claim))
	resume(t, f)
	before := f.sim.Reconciles(checkoutKustomization)
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))

	released := f.settle(t, claimKey)

	assert.Greater(t, f.sim.Reconciles(checkoutKustomization), before, "the suspend was handled before the release")
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "then handed back")
	require.NotNil(t, released.Status.Operation)
	assert.Equal(t, configbutleraiv1alpha3.ApplyOperationFailed, released.Status.Operation.Phase)
	assert.Contains(t, released.Status.Operation.Message, "handed its Kustomization back")
}

// TestApplierClaimRule_AHolderLeftUnconfiguredSaysTheKustomizationIsSuspended is C3: a holder whose
// configuration turns invalid keeps its finalizer and the Kustomization suspended, and its Held
// message says so.
func TestApplierClaimRule_AHolderLeftUnconfiguredSaysTheKustomizationIsSuspended(t *testing.T) {
	f := ruleFixture(t, testFluxClaim(claimLocalProvider), fluxTarget())
	f.settle(t, claimKey)
	f.r.newApplier = claimReconciler(nil, configbutleraiv1alpha3.ApplierClaimKindFlux, nil, nil, false).newApplier

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
	require.NoError(t, err)
	got := f.claim(t)

	configured := findCondition(got.Status.Conditions, ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonForbidden, configured.Reason)
	held := findCondition(got.Status.Conditions, ApplierClaimConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionUnknown, held.Status)
	assert.Equal(t, ReasonNotConfigured, held.Reason)
	assert.Contains(t, held.Message, "is left suspended under GitOps Reverser's field manager")
	assert.True(t, controllerutil.ContainsFinalizer(got, configbutleraiv1alpha3.FluxClaimFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
}

// TestApplierClaimRule_AGitTargetEventRerunsTheClaimsItNames: a GitTarget naming an applier claim, or
// that named one before an update, re-runs its check; a GitProvider re-runs the applier claims of the
// GitTargets writing through it.
func TestApplierClaimRule_AGitTargetEventRerunsTheClaimsItNames(t *testing.T) {
	c := claimTestClient(t, fluxTarget(), platformProvider())
	r := &ApplierClaimReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplierClaimKindFlux}

	requests := r.gitDestinationToClaims(context.Background(), platformProvider())
	require.Len(t, requests, 1)
	assert.Equal(t, claimKey, requests[0].NamespacedName)

	argo := &ApplierClaimReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplierClaimKindArgoCD}
	assert.Empty(t, argo.gitDestinationToClaims(context.Background(), platformProvider()), "another kind's")
}

// TestApplierClaimRule_AnApplierInAnotherNamespaceNeedsTheOptIn is B1: a namespace a KubeProvider
// admits may suspend and apply an applier in another namespace only when the provider sets
// spec.allowCrossNamespace.appliers; one in the applier claim's own namespace needs nothing. Refused, the
// applier claim never asks the applier's cluster, and holds nothing.
func TestApplierClaimRule_AnApplierInAnotherNamespaceNeedsTheOptIn(t *testing.T) {
	for _, kind := range []string{configbutleraiv1alpha3.ApplierClaimKindFlux, configbutleraiv1alpha3.ApplierClaimKindArgoCD} {
		for name, tc := range map[string]struct {
			sameNamespace, optIn bool
			wantStatus           metav1.ConditionStatus
			wantReason           string
		}{
			"another namespace, without the opt-in": {
				wantStatus: metav1.ConditionFalse, wantReason: authz.ReasonApplierNamespaceNotAllowed,
			},
			"another namespace, with the opt-in": {optIn: true, wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded},
			"its own namespace, which needs nothing": {
				sameNamespace: true, wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded,
			},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				r, calls := applierNamespaceReconciler(t, kind, tc.sameNamespace, tc.optIn)

				got := reconcileClaim(t, r)

				configured := findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
				require.NotNil(t, configured)
				assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
				assert.Equal(t, tc.wantReason, configured.Reason)
				if tc.wantStatus == metav1.ConditionFalse {
					assert.Contains(t, configured.Message, "spec.allowCrossNamespace.appliers")
					assert.True(t, conditionIsTrue(*got.StatusConditions(), ConditionTypeStalled))
					assert.Zero(t, *calls, "the applier's cluster is never asked")
					assert.Empty(t, got.GetFinalizers(), "and nothing is held")
				}
			})
		}
	}
}

// applierNamespaceReconciler is a reconciler of kind over the applier claim team-a/checkout, whose
// applier is in team-a when sameNamespace is set and in flux-system or argocd otherwise, with the
// KubeProvider claimLocalProvider setting allowCrossNamespace.appliers when optIn is set. It counts the
// lookups of the applier's cluster.
func applierNamespaceReconciler(
	t *testing.T, kind string, sameNamespace, optIn bool,
) (*ApplierClaimReconciler, *int) {
	t.Helper()
	claim := ctrlclient.Object(testFluxClaim(claimLocalProvider))
	appliers := applierFluxObjects()
	if kind == configbutleraiv1alpha3.ApplierClaimKindArgoCD {
		claim = testArgoCDClaim(claimLocalProvider)
		appliers = []runtime.Object{applierArgoApplication(nil)}
	}
	if sameNamespace {
		switch typed := claim.(type) {
		case *configbutleraiv1alpha3.FluxClaim:
			typed.Spec.KustomizationRef.Namespace = ""
		case *configbutleraiv1alpha3.ArgoCDClaim:
			typed.Spec.ApplicationRef.Namespace = ""
		}
		for _, obj := range appliers {
			obj.(*unstructured.Unstructured).SetNamespace("team-a")
		}
	}
	provider := admitting(claimLocalProvider, "team-a")
	if !optIn {
		provider.Spec.AllowCrossNamespace = nil
	}
	r := claimReconciler(claimTestClient(t, claim, provider), kind, applierFakeSource(t, appliers...), nil, true)
	calls := new(int)
	clients := r.ApplierClients
	r.ApplierClients = func(ctx context.Context, cluster string) (dynamic.Interface, error) {
		*calls++
		return clients(ctx, cluster)
	}
	return r, calls
}
