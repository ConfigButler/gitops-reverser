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

// claimTarget is the GitTarget team-a/checkout mirroring through the KubeProvider
// claimLocalProvider on branch main, naming the FluxClaim checkout when named is set.
func claimTarget(named bool) *configbutleraiv1alpha3.GitTarget {
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: claimProviderRef(claimLocalProvider),
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("platform", "main")},
			Path:            "apps",
		},
	}
	if named {
		target.Spec.ApplierClaimRef = &configbutleraiv1alpha3.ApplierClaimReference{
			Kind: configbutleraiv1alpha3.ApplierClaimKindFlux, Name: "checkout",
		}
	}
	return target
}

// claimDestination is what claimTarget's spec.gitBranchRef resolves to: branch main of the test
// repository.
func claimDestination() gitDestination {
	branch := gitBranch("platform", "team-a")
	return gitDestination{
		key:    ctrlclient.ObjectKeyFromObject(branch),
		branch: branch,
		provider: &configbutleraiv1alpha3.GitProvider{
			Spec: configbutleraiv1alpha3.GitProviderSpec{URL: applierTestRepo},
		},
	}
}

// readyClaim is the FluxClaim checkout, as its reconciler published it: Ready with status,
// Stalled when stalled, and applying branch of the test repository.
func readyClaim(status metav1.ConditionStatus, reason string, stalled bool, branch string) ctrlclient.Object {
	applierClaim := testFluxClaim(claimLocalProvider)
	stalledStatus := metav1.ConditionFalse
	if stalled {
		stalledStatus = metav1.ConditionTrue
	}
	applierClaim.Status.Conditions = []metav1.Condition{
		{Type: ConditionTypeReady, Status: status, Reason: reason, Message: "as published", ObservedGeneration: 1,
			LastTransitionTime: metav1.Now()},
		{Type: ConditionTypeStalled, Status: stalledStatus, Reason: reason, ObservedGeneration: 1,
			LastTransitionTime: metav1.Now()},
	}
	applierClaim.Status.Source = &configbutleraiv1alpha3.FluxClaimSource{
		URL:    "https://github.com/acme/platform",
		Branch: branch,
	}
	return applierClaim
}

func TestObserveApplierClaim_NoReferenceRemovesTheCondition(t *testing.T) {
	target := claimTarget(false)
	target.Status.Conditions = []metav1.Condition{{Type: GitTargetConditionApplierClaimReady, Status: "True"}}
	st := beginStatus(nil, nil, target)

	r := &GitTargetReconciler{Client: claimTestClient(t)}
	_, present := r.observeApplierClaim(context.Background(), st, target, claimDestination())

	assert.False(t, present)
	assert.Nil(t, findCondition(target.Status.Conditions, GitTargetConditionApplierClaimReady))
}

func TestObserveApplierClaim_Verdicts(t *testing.T) {
	cases := []struct {
		name          string
		applierClaim  ctrlclient.Object
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantMessage   string
		wantStalled   bool
		wantConverged bool
	}{
		{name: "a Ready applier claim whose applier applies this repository and branch",
			applierClaim: readyClaim(metav1.ConditionTrue, ReasonSucceeded, false, "main"),
			wantStatus:   metav1.ConditionTrue, wantReason: ReasonSucceeded, wantConverged: true,
			wantMessage: "FluxClaim team-a/checkout holds Kustomization flux-system/checkout"},
		{name: "no such applier claim, which may be on its way",
			wantStatus: metav1.ConditionFalse, wantReason: ReasonApplierClaimNotFound,
			wantMessage: "FluxClaim team-a/checkout not found"},
		{name: "an applier claim whose applier applies another branch",
			applierClaim: readyClaim(metav1.ConditionTrue, ReasonSucceeded, false, "release"),
			wantStatus:   metav1.ConditionFalse, wantReason: applier.ReasonSourceMismatch, wantStalled: true,
			wantMessage: `applies branch "release", not main`},
		{name: "an applier claim stalled itself",
			applierClaim: readyClaim(metav1.ConditionFalse, applier.ReasonForbidden, true, "main"),
			wantStatus:   metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true,
			wantMessage: "FluxClaim team-a/checkout: as published"},
		{name: "an applier claim whose operation is unresolved, which clears on its own",
			applierClaim: readyClaim(metav1.ConditionFalse, ReasonOperationUnresolved, false, "main"),
			wantStatus:   metav1.ConditionFalse, wantReason: ReasonOperationUnresolved},
		{name: "an applier claim that has not observed this reference yet, which clears once it does",
			applierClaim: readyClaim(metav1.ConditionTrue, applier.ReasonNotReferenced, false, "main"),
			wantStatus:   metav1.ConditionUnknown, wantReason: applier.ReasonNotReferenced,
			wantMessage: "has not observed that this GitTarget names it yet"},
		{name: "an applier claim not checked yet",
			applierClaim: testFluxClaim(claimLocalProvider),
			wantStatus:   metav1.ConditionUnknown, wantReason: ReasonProgressing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []ctrlclient.Object
			if tc.applierClaim != nil {
				objects = append(objects, tc.applierClaim)
			}
			r := &GitTargetReconciler{Client: claimTestClient(t, objects...)}
			target := claimTarget(true)
			st := beginStatus(nil, nil, target)

			verdict, present := r.observeApplierClaim(context.Background(), st, target, claimDestination())

			require.True(t, present)
			assert.Equal(t, tc.wantStatus, verdict.value.Status, verdict.value.Message)
			assert.Equal(t, tc.wantReason, verdict.value.Reason)
			assert.Contains(t, verdict.value.Message, tc.wantMessage)
			published := findCondition(target.Status.Conditions, GitTargetConditionApplierClaimReady)
			require.NotNil(t, published)
			assert.Equal(t, tc.wantReason, published.Reason)

			rd := newGitTargetReadiness()
			applierClaimReadiness(rd, verdict, present)
			assert.Equal(t, tc.wantConverged, rd.converged())
			assert.Equal(t, tc.wantStalled, !rd.converged() && !rd.converging())
		})
	}
}

// TestObserveApplierClaim_AReadyOfAnOlderGenerationIsNotReady: a Ready the applier claim's reconciler
// published for an earlier spec says nothing about the one it has now.
func TestObserveApplierClaim_AReadyOfAnOlderGenerationIsNotReady(t *testing.T) {
	applierClaim := readyClaim(metav1.ConditionTrue, ReasonSucceeded, false, "main")
	applierClaim.SetGeneration(2)
	r := &GitTargetReconciler{Client: claimTestClient(t, applierClaim)}
	target := claimTarget(true)

	verdict, _ := r.observeApplierClaim(
		context.Background(),
		beginStatus(nil, nil, target),
		target,
		claimDestination(),
	)

	assert.Equal(t, metav1.ConditionUnknown, verdict.value.Status)
}

// TestApplierClaimToGitTargets_FindsTheTargetsNamingIt: an applier claim event re-runs the GitTargets in its
// namespace that name it by kind and name, and no other.
func TestApplierClaimToGitTargets_FindsTheTargetsNamingIt(t *testing.T) {
	argo := claimTarget(true)
	argo.Name = "argo"
	argo.Spec.ApplierClaimRef.Kind = configbutleraiv1alpha3.ApplierClaimKindArgoCD
	unnamed := claimTarget(false)
	unnamed.Name = "unnamed"
	r := &GitTargetReconciler{Client: claimTestClient(t, claimTarget(true), argo, unnamed)}

	requests := r.applierClaimToGitTargets(context.Background(),
		testFluxClaim(claimLocalProvider))

	require.Len(t, requests, 1)
	assert.Equal(t, "checkout", requests[0].Name)
}

// TestApplierClaimReadinessChanged_IgnoresTheWorkersWrites: the GitTarget re-reads its applier claim when
// Ready, Stalled, the source or the spec moved, and not on every operation transition.
func TestApplierClaimReadinessChanged_IgnoresTheWorkersWrites(t *testing.T) {
	p := applierClaimReadinessChanged()
	before := readyClaim(metav1.ConditionTrue, ReasonSucceeded, false, "main")

	operation := before.DeepCopyObject().(*configbutleraiv1alpha3.FluxClaim)
	operation.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationRunning,
	}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: operation}))

	source := before.DeepCopyObject().(*configbutleraiv1alpha3.FluxClaim)
	source.Status.Source.Branch = "release"
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: source}))

	notReady := readyClaim(metav1.ConditionFalse, applier.ReasonForbidden, true, "main")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: notReady}))
}

// TestObserveApplierClaim_AFailedReadKeepsThePublishedVerdict: a blip reading the applier claim says
// nothing about it.
func TestObserveApplierClaim_AFailedReadKeepsThePublishedVerdict(t *testing.T) {
	failing := interceptor.NewClient(claimTestClient(t).(ctrlclient.WithWatch), interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object,
			...ctrlclient.GetOption) error {
			return errors.New("the API server is away")
		},
	})
	r := &GitTargetReconciler{Client: failing}

	target := claimTarget(true)
	verdict, _ := r.observeApplierClaim(
		context.Background(),
		beginStatus(nil, nil, target),
		target,
		claimDestination(),
	)
	assert.Equal(t, metav1.ConditionUnknown, verdict.value.Status, "nothing published yet")

	target = claimTarget(true)
	target.Status.Conditions = []metav1.Condition{{
		Type: GitTargetConditionApplierClaimReady, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
	}}
	verdict, _ = r.observeApplierClaim(
		context.Background(),
		beginStatus(nil, nil, target),
		target,
		claimDestination(),
	)
	assert.Equal(t, metav1.ConditionTrue, verdict.value.Status, "the published verdict stands")
}
