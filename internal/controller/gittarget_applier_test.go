// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const applierTestRepo = "https://github.com/acme/platform.git"

// applierFluxObjects is a Kustomization flux-system/checkout applying a GitRepository on branch.
func applierFluxObjects(branch string) []runtime.Object {
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"namespace": "flux-system", "name": "checkout"},
		"spec": map[string]any{
			"path":      "./apps",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "platform"},
		},
	}}
	repo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"namespace": "flux-system", "name": "platform"},
		"spec": map[string]any{
			"url": "https://github.com/acme/platform",
			"ref": map[string]any{"branch": branch},
		},
	}}
	return []runtime.Object{ks, repo}
}

func applierFakeSource(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}: "KustomizationList",
			{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}:   "GitRepositoryList",
		}, objs...)
}

type fixedLedger map[types.ResourceReference]configbutleraiv1alpha3.GitTargetApplierStatus

func (l fixedLedger) LastRequest(ref types.ResourceReference) (configbutleraiv1alpha3.GitTargetApplierStatus, bool) {
	status, ok := l[ref]
	return status, ok
}

func applierTarget(applierSet bool) *configbutleraiv1alpha3.GitTarget {
	target := &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a"},
		Spec:       configbutleraiv1alpha3.GitTargetSpec{Branch: "main", Path: "apps"},
	}
	if applierSet {
		target.Spec.Applier = &configbutleraiv1alpha3.GitTargetApplier{
			KustomizationRef: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "checkout"},
		}
	}
	return target
}

// applierReconciler checks appliers against a fake source cluster whose access review answers allowed.
func applierReconciler(source dynamic.Interface, sourceErr error, allowed bool) *GitTargetReconciler {
	return &GitTargetReconciler{
		ApplierClients: func(context.Context, string) (dynamic.Interface, error) { return source, sourceErr },
		newFlux: func(client dynamic.Interface) *applier.Flux {
			flux := applier.NewFlux(client)
			flux.CanI = func(context.Context, authorizationv1.ResourceAttributes) (bool, error) { return allowed, nil }
			return flux
		},
	}
}

func applierProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{Spec: configbutleraiv1alpha3.GitProviderSpec{URL: applierTestRepo}}
}

func TestObserveApplier_NoApplierRemovesTheCondition(t *testing.T) {
	target := applierTarget(false)
	target.Status.Conditions = []metav1.Condition{{Type: GitTargetConditionApplierConfigured, Status: "True"}}
	target.Status.Applier = &configbutleraiv1alpha3.GitTargetApplierStatus{RequestedCommit: "old"}
	st := beginStatus(nil, nil, target)

	_, present := applierReconciler(nil, nil, true).observeApplier(context.Background(), st, target, applierProvider())

	assert.False(t, present)
	assert.Nil(t, findCondition(target.Status.Conditions, GitTargetConditionApplierConfigured))
	assert.Nil(t, target.Status.Applier)
}

func TestObserveApplier_Verdicts(t *testing.T) {
	cases := []struct {
		name       string
		objects    []runtime.Object
		allowed    bool
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{name: "a Kustomization applying this repository and branch", objects: applierFluxObjects("main"),
			allowed: true, wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded},
		{name: "a Kustomization applying another branch", objects: applierFluxObjects("release"),
			allowed: true, wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonSourceMismatch},
		{name: "no such Kustomization", allowed: true,
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonApplierNotFound},
		{name: "an identity that may not patch it", objects: applierFluxObjects("main"),
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := applierTarget(true)
			st := beginStatus(nil, nil, target)

			value, present := applierReconciler(applierFakeSource(tc.objects...), nil, tc.allowed).
				observeApplier(context.Background(), st, target, applierProvider())

			require.True(t, present)
			assert.Equal(t, tc.wantStatus, value.Status, value.Message)
			assert.Equal(t, tc.wantReason, value.Reason)
			published := findCondition(target.Status.Conditions, GitTargetConditionApplierConfigured)
			require.NotNil(t, published)
			assert.Equal(t, tc.wantReason, published.Reason)
		})
	}
}

// TestObserveApplier_AnUnreachableSourceKeepsThePublishedVerdict: a blip reading the source cluster
// says nothing about the applier, so it must not flip a True to Unknown and back on every hiccup.
func TestObserveApplier_AnUnreachableSourceKeepsThePublishedVerdict(t *testing.T) {
	r := applierReconciler(nil, errors.New("connection refused"), true)

	target := applierTarget(true)
	value, _ := r.observeApplier(context.Background(), beginStatus(nil, nil, target), target, applierProvider())
	assert.Equal(t, metav1.ConditionUnknown, value.Status, "nothing published yet")

	target = applierTarget(true)
	target.Status.Conditions = []metav1.Condition{{
		Type: GitTargetConditionApplierConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
	}}
	value, _ = r.observeApplier(context.Background(), beginStatus(nil, nil, target), target, applierProvider())
	assert.Equal(t, metav1.ConditionTrue, value.Status, "the published verdict stands")
}

// TestObserveApplier_AHangingSourceIsBounded is the review's finding 6: the source client has no
// request timeout, so a source API server that accepts the connection and never answers held the
// reconcile, and with it every GitTarget, for as long as it liked. The check now gives up, and a
// check that could not be made keeps the published verdict.
func TestObserveApplier_AHangingSourceIsBounded(t *testing.T) {
	var sawDeadline bool
	r := &GitTargetReconciler{
		ApplierClients: func(ctx context.Context, _ string) (dynamic.Interface, error) {
			_, sawDeadline = ctx.Deadline()
			return applierFakeSource(applierFluxObjects("main")...), nil
		},
		newFlux: func(client dynamic.Interface) *applier.Flux {
			flux := applier.NewFlux(client)
			flux.CanI = func(ctx context.Context, _ authorizationv1.ResourceAttributes) (bool, error) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "the access review must run under a deadline")
				assert.LessOrEqual(t, time.Until(deadline), applierCheckTimeout)
				return false, context.DeadlineExceeded // what a server that never answers ends in
			}
			return flux
		},
	}

	target := applierTarget(true)
	target.Status.Conditions = []metav1.Condition{{
		Type: GitTargetConditionApplierConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
	}}
	value, _ := r.observeApplier(context.Background(), beginStatus(nil, nil, target), target, applierProvider())

	assert.True(t, sawDeadline, "the client lookup must run under a deadline")
	assert.Equal(t, metav1.ConditionTrue, value.Status, "a timed-out check keeps the published verdict")
}

func TestObserveApplier_PublishesTheLastRequest(t *testing.T) {
	target := applierTarget(true)
	at := metav1.NewTime(time.Date(2026, 10, 20, 10, 14, 3, 0, time.UTC))
	r := applierReconciler(applierFakeSource(applierFluxObjects("main")...), nil, true)
	r.ApplyRequests = fixedLedger{types.NewResourceReference("checkout", "team-a"): {
		RequestedCommit: "4f2c1ab9", RequestedAt: &at, Reason: configbutleraiv1alpha3.ApplyReasonForeignCommit,
	}}

	r.observeApplier(context.Background(), beginStatus(nil, nil, target), target, applierProvider())

	require.NotNil(t, target.Status.Applier)
	assert.Equal(t, "4f2c1ab9", target.Status.Applier.RequestedCommit)
	assert.Equal(t, configbutleraiv1alpha3.ApplyReasonForeignCommit, target.Status.Applier.Reason)
}

func TestApplierReadiness_StallsOnlyWhatNeedsAHuman(t *testing.T) {
	cases := []struct {
		name          string
		value         conditionValue
		present       bool
		wantConverged bool
		wantStalled   bool
	}{
		{name: "no applier", present: false, wantConverged: true},
		{name: "configured", present: true, wantConverged: true,
			value: conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded}},
		{name: "not created yet", present: true,
			value: conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonApplierNotFound}},
		{name: "another branch", present: true, wantStalled: true,
			value: conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonSourceMismatch}},
		{name: "forbidden", present: true, wantStalled: true,
			value: conditionValue{Status: metav1.ConditionFalse, Reason: applier.ReasonForbidden}},
		{name: "not checked yet", present: true,
			value: conditionValue{Status: metav1.ConditionUnknown, Reason: ReasonProgressing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := newGitTargetReadiness()
			applierReadiness(rd, tc.value, tc.present)
			assert.Equal(t, tc.wantConverged, rd.converged())
			assert.Equal(t, tc.wantStalled, !rd.converged() && !rd.converging())
		})
	}
}
