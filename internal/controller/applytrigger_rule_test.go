// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
)

// The rule (review 7): a trigger acts only while an eligible GitTarget references it. These tests
// pin its three cases (never referenced or released, referenced, retiring), the binding verdict, and
// the line of triggers for one applier.

// ruleFixture is the hold fixture over a control cluster holding objects, the ClusterProviders and
// the GitProvider platform, and no GitTarget unless objects carries one.
func ruleFixture(t *testing.T, objects ...ctrlclient.Object) *holdFixture {
	t.Helper()
	sim := applierSim(t, applierFluxObjects()...)
	c := triggerTestClient(t, append(objects, unreferenced{})...)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, sim.Client, nil, true)
	recorder := record.NewFakeRecorder(64)
	r.Recorder = recorder
	return &holdFixture{r: r, sim: sim, recorder: recorder}
}

// fluxTarget is the GitTarget team-a/checkout-flux naming the FluxTrigger checkout, writing main.
func fluxTarget() *configbutleraiv1alpha3.GitTarget {
	return referencingTarget("checkout-flux", configbutleraiv1alpha3.ApplyTriggerKindFlux)
}

// settle reconciles the trigger at key until its Held condition is neither Suspending nor
// Releasing, and returns it.
func (f *holdFixture) settle(t *testing.T, key k8stypes.NamespacedName) *configbutleraiv1alpha3.FluxTrigger {
	t.Helper()
	trigger := &configbutleraiv1alpha3.FluxTrigger{}
	require.Eventually(t, func() bool {
		_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, f.r.Get(context.Background(), key, trigger))
		held := findCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld)
		return held != nil && held.Reason != ReasonSuspending && held.Reason != ReasonReleasing
	}, 5*time.Second, 5*time.Millisecond)
	return trigger
}

// heldReason is the trigger's Held reason.
func heldReason(t *testing.T, trigger *configbutleraiv1alpha3.FluxTrigger) string {
	t.Helper()
	held := findCondition(trigger.Status.Conditions, ApplyTriggerConditionHeld)
	require.NotNil(t, held)
	return held.Reason
}

// TestApplyTriggerRule_AnUnreferencedTriggerDoesNothingAndIsReady: no call reaches the applier's
// cluster, nothing is held, no finalizer is added, and the trigger is Ready, so a Flux health check
// that waits on it does not wait for the GitTarget that will name it.
func TestApplyTriggerRule_AnUnreferencedTriggerDoesNothingAndIsReady(t *testing.T) {
	for _, kind := range []string{configbutleraiv1alpha3.ApplyTriggerKindFlux, configbutleraiv1alpha3.ApplyTriggerKindArgoCD} {
		t.Run(kind, func(t *testing.T) {
			var trigger ctrlclient.Object = testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
			if kind == configbutleraiv1alpha3.ApplyTriggerKindArgoCD {
				trigger = testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
			}
			r := triggerReconciler(triggerTestClient(t, trigger, unreferenced{}), kind, nil, nil, true)
			calls := 0
			r.ApplierClients = func(context.Context, string) (dynamic.Interface, error) {
				calls++
				return nil, errors.New("the applier's cluster must not be asked")
			}

			got := reconcileTrigger(t, r)

			assert.Zero(t, calls, "nothing is read in the applier's cluster")
			configured := findCondition(*got.StatusConditions(), ApplyTriggerConditionConfigured)
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
			if flux, ok := got.(*configbutleraiv1alpha3.FluxTrigger); ok {
				assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, flux))
			}
		})
	}
}

// TestApplyTriggerRule_TheFirstEligibleReferenceAdoptsAndTheLastReleases: naming the trigger is the
// adoption moment; the GitTarget going hands the Kustomization back, and the trigger is idle again.
func TestApplyTriggerRule_TheFirstEligibleReferenceAdoptsAndTheLastReleases(t *testing.T) {
	f := ruleFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
	idle := f.settle(t, triggerKey)
	assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, idle))
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "Flux applies it on its own")

	target := fluxTarget()
	require.NoError(t, f.r.Create(context.Background(), target))
	held := f.settle(t, triggerKey)
	assert.Equal(t, ReasonSuspended, heldReason(t, held))
	assert.True(t, conditionIsTrue(held.Status.Conditions, ConditionTypeReady))
	assert.True(t, controllerutil.ContainsFinalizer(held, configbutleraiv1alpha3.FluxTriggerFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Adopted"), events[0])

	require.NoError(t, f.r.Delete(context.Background(), target))
	released := f.settle(t, triggerKey)
	assert.Equal(t, applier.ReasonNotReferenced, heldReason(t, released))
	ready := findCondition(released.Status.Conditions, ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, applier.ReasonNotReferenced, ready.Reason)
	assert.Empty(t, released.Finalizers, "a trigger that holds nothing deletes at once")
	assert.Nil(t, released.Status.Source)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "handed back to Flux")
	events = f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released handed Kustomization flux-system/checkout back"),
		events[0])
}

// TestApplyTriggerRule_ARecreatedGitTargetReleasesAndReadopts is the trade-off the rule accepts:
// deleting and re-creating the GitTarget hands the Kustomization back, and adopts it again.
func TestApplyTriggerRule_ARecreatedGitTargetReleasesAndReadopts(t *testing.T) {
	f := ruleFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), fluxTarget())
	f.settle(t, triggerKey)
	f.events()

	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))
	f.settle(t, triggerKey)
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	require.NoError(t, f.r.Create(context.Background(), fluxTarget()))
	again := f.settle(t, triggerKey)

	assert.Equal(t, ReasonSuspended, heldReason(t, again))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
	var reasons []string
	for _, event := range f.events() {
		reasons = append(reasons, strings.Fields(event)[1])
	}
	assert.Equal(t, []string{EventReasonReleased, EventReasonAdopted}, reasons)
}

// TestApplyTriggerRule_TheBindingAggregatesItsGitTargets: one eligible GitTarget among mismatched
// ones binds the trigger; none that can be judged yet is progressing; every one refused is stalled,
// with the reason they share, or NotBound for mixed ones, and each GitTarget in the message.
func TestApplyTriggerRule_TheBindingAggregatesItsGitTargets(t *testing.T) {
	onBranch := func(name, branch string) *configbutleraiv1alpha3.GitTarget {
		target := referencingTarget(name, configbutleraiv1alpha3.ApplyTriggerKindFlux)
		target.Spec.Branch = branch
		return target
	}
	elsewhere := onBranch("elsewhere", "main")
	elsewhere.Spec.ClusterProviderRef = &meta.LocalObjectReference{Name: "closed"}
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
				target.Spec.GitProviderRef.Name = "absent"
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
				&configbutleraiv1alpha3.ClusterProvider{
					ObjectMeta: metav1.ObjectMeta{Name: "closed"},
					Spec: configbutleraiv1alpha3.ClusterProviderSpec{
						AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{Names: []string{"team-b"}},
					},
				}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]ctrlclient.Object{testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)},
				tc.objects...)
			f := ruleFixture(t, objects...)

			got := f.settle(t, triggerKey)

			configured := findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured)
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

// TestApplyTriggerRule_AnOlderTriggerNotInLineKeepsNoneOut: an older trigger whose GitTarget writes
// another branch, or whose namespace is not admitted, no longer keeps a younger eligible one from
// holding the Kustomization.
func TestApplyTriggerRule_AnOlderTriggerNotInLineKeepsNoneOut(t *testing.T) {
	for name, older := range map[string]func(*configbutleraiv1alpha3.GitTarget, *[]ctrlclient.Object){
		"bound to another branch": func(target *configbutleraiv1alpha3.GitTarget, _ *[]ctrlclient.Object) {
			target.Spec.Branch = "release"
		},
		"not admitted": func(target *configbutleraiv1alpha3.GitTarget, objects *[]ctrlclient.Object) {
			target.Spec.ClusterProviderRef = &meta.LocalObjectReference{Name: "closed"}
			*objects = append(*objects, admitting("closed", "team-a"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			stray := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
			stray.Namespace = inLineKey.Namespace
			stray.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
			mine := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
			mine.CreationTimestamp = metav1.NewTime(stray.CreationTimestamp.Add(time.Hour))
			objects := []ctrlclient.Object{stray, mine}
			strayRefs := referencedIn(inLineKey.Namespace, configbutleraiv1alpha3.ApplyTriggerKindFlux)
			older(strayRefs[1].(*configbutleraiv1alpha3.GitTarget), &objects)
			objects = append(objects, strayRefs...)
			objects = append(objects, referencedIn("team-a", configbutleraiv1alpha3.ApplyTriggerKindFlux)...)
			objects = append(objects, admitting(configbutleraiv1alpha3.DefaultClusterProviderName, "team-a", "team-b"))
			f := ruleFixture(t, objects...)

			got := f.settle(t, triggerKey)

			assert.True(t, conditionIsTrue(got.Status.Conditions, ApplyTriggerConditionConfigured),
				findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured).Message)
			assert.Equal(t, ReasonSuspended, heldReason(t, got))
			strayConfigured := findCondition(f.settle(t, inLineKey).Status.Conditions, ApplyTriggerConditionConfigured)
			require.NotNil(t, strayConfigured)
			assert.NotEqual(t, metav1.ConditionTrue, strayConfigured.Status)
			assert.NotEqual(t, applier.ReasonHeldByAnotherTrigger, strayConfigured.Reason,
				"it is refused for its own binding, before the line is asked")
		})
	}
}

// TestApplyTriggerRule_ARetiringHolderStaysInLineUntilItsReleaseIsDone: the holder's GitTarget goes
// while a successor waits. Until the holder has suspended the Kustomization and seen that handled,
// it stays first in line, Held=False (Releasing), and the successor does nothing; then the holder
// leaves the Kustomization suspended for it, and the successor takes it over.
func TestApplyTriggerRule_ARetiringHolderStaysInLineUntilItsReleaseIsDone(t *testing.T) {
	holder := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	holder.CreationTimestamp = metav1.NewTime(time.Unix(1_000_000, 0))
	successor := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	successor.Namespace = inLineKey.Namespace
	successor.CreationTimestamp = metav1.NewTime(holder.CreationTimestamp.Add(time.Hour))
	objects := []ctrlclient.Object{holder, successor,
		admitting(configbutleraiv1alpha3.DefaultClusterProviderName, "team-a", "team-b")}
	objects = append(objects, referencedIn("team-a", configbutleraiv1alpha3.ApplyTriggerKindFlux)...)
	objects = append(objects, referencedIn("team-b", configbutleraiv1alpha3.ApplyTriggerKindFlux)...)
	f := ruleFixture(t, objects...)
	f.settle(t, triggerKey)
	f.settle(t, inLineKey)
	f.events()

	// A worker's apply has the Kustomization resumed, and kustomize-controller is busy.
	resume(t, f)
	unpause := f.sim.PauseKustomizations()
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-fluxtrigger"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
	require.NoError(t, err)
	retiring := f.trigger(t)
	assert.Equal(t, ReasonReleasing, heldReason(t, retiring))
	assert.True(t, controllerutil.ContainsFinalizer(retiring, configbutleraiv1alpha3.FluxTriggerFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "suspended first")
	_, err = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: inLineKey})
	require.NoError(t, err)
	waiting := &configbutleraiv1alpha3.FluxTrigger{}
	require.NoError(t, f.r.Get(context.Background(), inLineKey, waiting))
	configured := findCondition(waiting.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonHeldByAnotherTrigger, configured.Reason, "the retiring holder is still first")
	assert.Contains(t, configured.Message, "which holds it now")

	unpause()
	done := f.settle(t, triggerKey)
	assert.Empty(t, done.Finalizers, "handed over")
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "left suspended for the successor")
	taken := f.settle(t, inLineKey)
	assert.True(t, conditionIsTrue(taken.Status.Conditions, ApplyTriggerConditionConfigured))
	assert.Equal(t, ReasonSuspended, heldReason(t, taken))
	events := f.events()
	require.NotEmpty(t, events)
	assert.True(t, strings.HasPrefix(events[0], "Normal Released left Kustomization flux-system/checkout suspended "+
		"for FluxTrigger team-b/checkout"), events[0])
}

// resume resumes the Kustomization as a branch worker's apply does, under GitOps Reverser's field
// manager.
func resume(t *testing.T, f *holdFixture) {
	t.Helper()
	f.sim.Edit(checkoutKustomization, func(ks *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(ks.Object, false, "spec", "suspend")
	})
}

// TestApplyTriggerRule_AReferenceReturningDuringTheReleaseHoldsAgain: the GitTarget is gone, and
// back, before the release finished: the trigger holds the Kustomization again, as it is, and
// hands nothing back.
func TestApplyTriggerRule_AReferenceReturningDuringTheReleaseHoldsAgain(t *testing.T) {
	f := ruleFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), fluxTarget())
	f.settle(t, triggerKey)
	f.events()
	resume(t, f)
	unpause := f.sim.PauseKustomizations()
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
	require.NoError(t, err)
	assert.Equal(t, ReasonReleasing, heldReason(t, f.trigger(t)))

	require.NoError(t, f.r.Create(context.Background(), fluxTarget()))
	unpause()
	back := f.settle(t, triggerKey)

	assert.Equal(t, ReasonSuspended, heldReason(t, back))
	assert.True(t, controllerutil.ContainsFinalizer(back, configbutleraiv1alpha3.FluxTriggerFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "never handed back")
	for _, event := range f.events() {
		assert.NotContains(t, event, EventReasonReleased)
	}
}

// TestApplyTriggerRule_ReleasingWithAnApplyInFlightSuspendsFirst: the last GitTarget goes while an
// apply has the Kustomization resumed, followed by nobody here. The release suspends it, waits until
// that is handled, then hands it back, and completes the operation nobody would complete.
func TestApplyTriggerRule_ReleasingWithAnApplyInFlightSuspendsFirst(t *testing.T) {
	f := ruleFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), fluxTarget())
	f.settle(t, triggerKey)
	f.events()
	trigger := f.trigger(t)
	trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning, Reason: configbutleraiv1alpha3.ApplyReasonCatchUp,
		Token: "T1", RequestedAt: metav1.Now(),
	}
	require.NoError(t, f.r.Status().Update(context.Background(), trigger))
	resume(t, f)
	before := f.sim.Reconciles(checkoutKustomization)
	target := &configbutleraiv1alpha3.GitTarget{}
	require.NoError(t, f.r.Get(context.Background(),
		k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout-flux"}, target))
	require.NoError(t, f.r.Delete(context.Background(), target))

	released := f.settle(t, triggerKey)

	assert.Greater(t, f.sim.Reconciles(checkoutKustomization), before, "the suspend was handled before the release")
	assert.False(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)), "then handed back")
	require.NotNil(t, released.Status.Operation)
	assert.Equal(t, configbutleraiv1alpha3.ApplyOperationFailed, released.Status.Operation.Phase)
	assert.Contains(t, released.Status.Operation.Message, "handed its Kustomization back")
}

// TestApplyTriggerRule_AHolderLeftUnconfiguredSaysTheKustomizationIsSuspended is C3: a holder whose
// configuration turns invalid keeps its finalizer and the Kustomization suspended, and its Held
// message says so.
func TestApplyTriggerRule_AHolderLeftUnconfiguredSaysTheKustomizationIsSuspended(t *testing.T) {
	f := ruleFixture(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName), fluxTarget())
	f.settle(t, triggerKey)
	f.r.newApplier = triggerReconciler(nil, configbutleraiv1alpha3.ApplyTriggerKindFlux, nil, nil, false).newApplier

	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: triggerKey})
	require.NoError(t, err)
	got := f.trigger(t)

	configured := findCondition(got.Status.Conditions, ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, applier.ReasonForbidden, configured.Reason)
	held := findCondition(got.Status.Conditions, ApplyTriggerConditionHeld)
	require.NotNil(t, held)
	assert.Equal(t, metav1.ConditionUnknown, held.Status)
	assert.Equal(t, ReasonNotConfigured, held.Reason)
	assert.Contains(t, held.Message, "is left suspended under GitOps Reverser's field manager")
	assert.True(t, controllerutil.ContainsFinalizer(got, configbutleraiv1alpha3.FluxTriggerFinalizer))
	assert.True(t, suspendedSpec(f.sim.Kustomization(checkoutKustomization)))
}

// TestApplyTriggerRule_AGitTargetEventRerunsTheTriggersItNames: a GitTarget naming a trigger, or
// that named one before an update, re-runs its check; a GitProvider re-runs the triggers of the
// GitTargets writing through it.
func TestApplyTriggerRule_AGitTargetEventRerunsTheTriggersItNames(t *testing.T) {
	c := triggerTestClient(t, fluxTarget(), platformProvider())
	r := &ApplyTriggerReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplyTriggerKindFlux}

	requests := r.gitProviderToTriggers(context.Background(), platformProvider())
	require.Len(t, requests, 1)
	assert.Equal(t, triggerKey, requests[0].NamespacedName)

	argo := &ApplyTriggerReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplyTriggerKindArgoCD}
	assert.Empty(t, argo.gitProviderToTriggers(context.Background(), platformProvider()), "another kind's")
}
