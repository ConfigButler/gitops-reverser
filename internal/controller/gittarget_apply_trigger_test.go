// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
)

// triggerTarget is the GitTarget team-a/checkout on branch main, naming the FluxTrigger checkout
// when named is set.
func triggerTarget(named bool) *configbutleraiv1alpha3.GitTarget {
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.NamespacedObjectReference{Name: "platform"}, Branch: "main", Path: "apps",
		},
	}
	if named {
		target.Spec.ApplyTriggerRef = &configbutleraiv1alpha3.ApplyTriggerReference{
			Kind: configbutleraiv1alpha3.ApplyTriggerKindFlux, Name: "checkout",
		}
	}
	return target
}

func triggerProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{Spec: configbutleraiv1alpha3.GitProviderSpec{URL: applierTestRepo}}
}

// readyTrigger is the FluxTrigger checkout, as its reconciler published it: Ready with status,
// Stalled when stalled, and applying branch of the test repository.
func readyTrigger(status metav1.ConditionStatus, reason string, stalled bool, branch string) ctrlclient.Object {
	trigger := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	stalledStatus := metav1.ConditionFalse
	if stalled {
		stalledStatus = metav1.ConditionTrue
	}
	trigger.Status.Conditions = []metav1.Condition{
		{Type: ConditionTypeReady, Status: status, Reason: reason, Message: "as published", ObservedGeneration: 1,
			LastTransitionTime: metav1.Now()},
		{Type: ConditionTypeStalled, Status: stalledStatus, Reason: reason, ObservedGeneration: 1,
			LastTransitionTime: metav1.Now()},
	}
	trigger.Status.Source = &configbutleraiv1alpha3.FluxTriggerSource{
		URL:    "https://github.com/acme/platform",
		Branch: branch,
	}
	return trigger
}

func TestObserveApplyTrigger_NoReferenceRemovesTheCondition(t *testing.T) {
	target := triggerTarget(false)
	target.Status.Conditions = []metav1.Condition{{Type: GitTargetConditionApplyTriggerReady, Status: "True"}}
	st := beginStatus(nil, nil, target)

	r := &GitTargetReconciler{Client: triggerTestClient(t)}
	_, present := r.observeApplyTrigger(context.Background(), st, target, triggerProvider())

	assert.False(t, present)
	assert.Nil(t, findCondition(target.Status.Conditions, GitTargetConditionApplyTriggerReady))
}

func TestObserveApplyTrigger_Verdicts(t *testing.T) {
	cases := []struct {
		name          string
		trigger       ctrlclient.Object
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantMessage   string
		wantStalled   bool
		wantConverged bool
	}{
		{name: "a Ready trigger whose applier applies this repository and branch",
			trigger:    readyTrigger(metav1.ConditionTrue, ReasonSucceeded, false, "main"),
			wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded, wantConverged: true,
			wantMessage: "FluxTrigger team-a/checkout holds Kustomization flux-system/checkout"},
		{name: "no such trigger, which may be on its way",
			wantStatus: metav1.ConditionFalse, wantReason: ReasonApplyTriggerNotFound,
			wantMessage: "FluxTrigger team-a/checkout not found"},
		{name: "a trigger whose applier applies another branch",
			trigger:    readyTrigger(metav1.ConditionTrue, ReasonSucceeded, false, "release"),
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonSourceMismatch, wantStalled: true,
			wantMessage: `applies branch "release", not main`},
		{name: "a trigger stalled itself",
			trigger:    readyTrigger(metav1.ConditionFalse, applier.ReasonForbidden, true, "main"),
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true,
			wantMessage: "FluxTrigger team-a/checkout: as published"},
		{name: "a trigger whose operation is unresolved, which clears on its own",
			trigger:    readyTrigger(metav1.ConditionFalse, ReasonOperationUnresolved, false, "main"),
			wantStatus: metav1.ConditionFalse, wantReason: ReasonOperationUnresolved},
		{name: "a trigger that has not observed this reference yet, which clears once it does",
			trigger:    readyTrigger(metav1.ConditionTrue, applier.ReasonNotReferenced, false, "main"),
			wantStatus: metav1.ConditionUnknown, wantReason: applier.ReasonNotReferenced,
			wantMessage: "has not observed that this GitTarget names it yet"},
		{name: "a trigger not checked yet",
			trigger:    testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName),
			wantStatus: metav1.ConditionUnknown, wantReason: ReasonProgressing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []ctrlclient.Object
			if tc.trigger != nil {
				objects = append(objects, tc.trigger)
			}
			r := &GitTargetReconciler{Client: triggerTestClient(t, objects...)}
			target := triggerTarget(true)
			st := beginStatus(nil, nil, target)

			verdict, present := r.observeApplyTrigger(context.Background(), st, target, triggerProvider())

			require.True(t, present)
			assert.Equal(t, tc.wantStatus, verdict.value.Status, verdict.value.Message)
			assert.Equal(t, tc.wantReason, verdict.value.Reason)
			assert.Contains(t, verdict.value.Message, tc.wantMessage)
			published := findCondition(target.Status.Conditions, GitTargetConditionApplyTriggerReady)
			require.NotNil(t, published)
			assert.Equal(t, tc.wantReason, published.Reason)

			rd := newGitTargetReadiness()
			applyTriggerReadiness(rd, verdict, present)
			assert.Equal(t, tc.wantConverged, rd.converged())
			assert.Equal(t, tc.wantStalled, !rd.converged() && !rd.converging())
		})
	}
}

// TestObserveApplyTrigger_AReadyOfAnOlderGenerationIsNotReady: a Ready the trigger's reconciler
// published for an earlier spec says nothing about the one it has now.
func TestObserveApplyTrigger_AReadyOfAnOlderGenerationIsNotReady(t *testing.T) {
	trigger := readyTrigger(metav1.ConditionTrue, ReasonSucceeded, false, "main")
	trigger.SetGeneration(2)
	r := &GitTargetReconciler{Client: triggerTestClient(t, trigger)}
	target := triggerTarget(true)

	verdict, _ := r.observeApplyTrigger(context.Background(), beginStatus(nil, nil, target), target, triggerProvider())

	assert.Equal(t, metav1.ConditionUnknown, verdict.value.Status)
}

// TestApplyTriggerToGitTargets_FindsTheTargetsNamingIt: a trigger event re-runs the GitTargets in its
// namespace that name it by kind and name, and no other.
func TestApplyTriggerToGitTargets_FindsTheTargetsNamingIt(t *testing.T) {
	argo := triggerTarget(true)
	argo.Name = "argo"
	argo.Spec.ApplyTriggerRef.Kind = configbutleraiv1alpha3.ApplyTriggerKindArgoCD
	unnamed := triggerTarget(false)
	unnamed.Name = "unnamed"
	r := &GitTargetReconciler{Client: triggerTestClient(t, triggerTarget(true), argo, unnamed)}

	requests := r.applyTriggerToGitTargets(context.Background(),
		testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))

	require.Len(t, requests, 1)
	assert.Equal(t, "checkout", requests[0].Name)
}

// TestApplyTriggerReadinessChanged_IgnoresTheWorkersWrites: the GitTarget re-reads its trigger when
// Ready, Stalled, the source or the spec moved, and not on every operation transition.
func TestApplyTriggerReadinessChanged_IgnoresTheWorkersWrites(t *testing.T) {
	p := applyTriggerReadinessChanged()
	before := readyTrigger(metav1.ConditionTrue, ReasonSucceeded, false, "main")

	operation := before.DeepCopyObject().(*configbutleraiv1alpha3.FluxTrigger)
	operation.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning,
	}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: operation}))

	source := before.DeepCopyObject().(*configbutleraiv1alpha3.FluxTrigger)
	source.Status.Source.Branch = "release"
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: source}))

	notReady := readyTrigger(metav1.ConditionFalse, applier.ReasonForbidden, true, "main")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: notReady}))
}

// TestObserveApplyTrigger_AFailedReadKeepsThePublishedVerdict: a blip reading the trigger says
// nothing about it.
func TestObserveApplyTrigger_AFailedReadKeepsThePublishedVerdict(t *testing.T) {
	failing := interceptor.NewClient(triggerTestClient(t).(ctrlclient.WithWatch), interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object,
			...ctrlclient.GetOption) error {
			return errors.New("the API server is away")
		},
	})
	r := &GitTargetReconciler{Client: failing}

	target := triggerTarget(true)
	verdict, _ := r.observeApplyTrigger(context.Background(), beginStatus(nil, nil, target), target, triggerProvider())
	assert.Equal(t, metav1.ConditionUnknown, verdict.value.Status, "nothing published yet")

	target = triggerTarget(true)
	target.Status.Conditions = []metav1.Condition{{
		Type: GitTargetConditionApplyTriggerReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
	}}
	verdict, _ = r.observeApplyTrigger(context.Background(), beginStatus(nil, nil, target), target, triggerProvider())
	assert.Equal(t, metav1.ConditionTrue, verdict.value.Status, "the published verdict stands")
}
