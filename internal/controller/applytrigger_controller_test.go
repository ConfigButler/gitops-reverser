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
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

const applierTestRepo = "https://github.com/acme/platform.git"

// applierFluxObjects is a Kustomization flux-system/checkout applying a GitRepository on main.
func applierFluxObjects() []runtime.Object {
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
			"ref": map[string]any{"branch": "main"},
		},
	}}
	return []runtime.Object{ks, repo}
}

// applierArgoApplication is an Application argocd/checkout syncing main of the test repository,
// with a syncPolicy when one is given.
func applierArgoApplication(syncPolicy map[string]any) runtime.Object {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"namespace": "argocd", "name": "checkout"},
		"spec": map[string]any{
			"source": map[string]any{
				"repoURL": "https://github.com/acme/platform", "targetRevision": "main", "path": "apps",
			},
		},
	}}
	if syncPolicy != nil {
		_ = unstructured.SetNestedMap(app.Object, syncPolicy, "spec", "syncPolicy")
	}
	return app
}

func applierFakeSource(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}: "KustomizationList",
			{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}:   "GitRepositoryList",
			{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}:             "ApplicationList",
		}, objs...)
}

// admitting is a ClusterProvider whose accessFrom admits namespaces.
func admitting(name string, namespaces ...string) *configbutleraiv1alpha3.ClusterProvider {
	return &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{Names: namespaces},
		},
	}
}

// testFluxTrigger is the FluxTrigger team-a/checkout holding flux-system/checkout in cluster.
func testFluxTrigger(cluster string) *configbutleraiv1alpha3.FluxTrigger {
	return &configbutleraiv1alpha3.FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.FluxTriggerSpec{
			ClusterProviderRef: meta.LocalObjectReference{Name: cluster},
			KustomizationRef:   meta.NamespacedObjectReference{Namespace: "flux-system", Name: "checkout"},
		},
	}
}

// testArgoCDTrigger is the ArgoCDTrigger team-a/checkout holding argocd/checkout in cluster.
func testArgoCDTrigger(cluster string) *configbutleraiv1alpha3.ArgoCDTrigger {
	return &configbutleraiv1alpha3.ArgoCDTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.ArgoCDTriggerSpec{
			ClusterProviderRef: meta.LocalObjectReference{Name: cluster},
			ApplicationRef:     meta.NamespacedObjectReference{Namespace: "argocd", Name: "checkout"},
		},
	}
}

// triggerTestClient is a control cluster holding objects, and the ClusterProviders "default" and
// "hub" admitting team-a unless objects carries its own.
func triggerTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	held := map[string]bool{}
	for _, obj := range objects {
		if _, ok := obj.(*configbutleraiv1alpha3.ClusterProvider); ok {
			held[obj.GetName()] = true
		}
	}
	for _, name := range []string{configbutleraiv1alpha3.DefaultClusterProviderName, "hub"} {
		if !held[name] {
			objects = append(objects, admitting(name, "team-a"))
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&configbutleraiv1alpha3.FluxTrigger{}, &configbutleraiv1alpha3.ArgoCDTrigger{}).
		Build()
}

// triggerReconciler checks appliers against a fake cluster whose access review answers allowed.
func triggerReconciler(
	c ctrlclient.Client, kind string, source dynamic.Interface, sourceErr error, allowed bool,
) *ApplyTriggerReconciler {
	return &ApplyTriggerReconciler{
		Client:         c,
		Kind:           kind,
		ApplierClients: func(context.Context, string) (dynamic.Interface, error) { return source, sourceErr },
		newApplier: func(kind string, client dynamic.Interface) applier.Client {
			canI := func(context.Context, authorizationv1.ResourceAttributes) (bool, error) { return allowed, nil }
			if kind == configbutleraiv1alpha3.ApplierKindApplication {
				argo := applier.NewArgoCD(client)
				argo.CanI = canI
				return argo
			}
			flux := applier.NewFlux(client)
			flux.CanI = canI
			return flux
		},
	}
}

// reconcileTrigger runs one reconcile of team-a/checkout and returns the trigger as it is then.
func reconcileTrigger(t *testing.T, r *ApplyTriggerReconciler) configbutleraiv1alpha3.ApplyTrigger {
	t.Helper()
	key := k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout"}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	trigger := configbutleraiv1alpha3.NewApplyTrigger(r.Kind)
	require.NoError(t, r.Get(context.Background(), key, trigger))
	want := RequeueSteadyInterval
	if !conditionIsTrue(*trigger.StatusConditions(), ConditionTypeReady) &&
		!conditionIsTrue(*trigger.StatusConditions(), ConditionTypeStalled) {
		want = applyTriggerSettleInterval
	}
	assert.Equal(t, want, result.RequeueAfter, "a trigger that may clear on its own is read again soon")
	return trigger
}

func TestApplyTriggerReconcile_FluxVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		objects     []runtime.Object
		allowed     bool
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
		wantSource  *configbutleraiv1alpha3.FluxTriggerSource
	}{
		{name: "a Kustomization applying a GitRepository on a branch", objects: applierFluxObjects(),
			allowed: true, wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded,
			wantSource: &configbutleraiv1alpha3.FluxTriggerSource{
				GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
				URL:           "https://github.com/acme/platform", Branch: "main",
			}},
		{name: "no such Kustomization, which may be on its way", allowed: true,
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonApplierNotFound},
		{name: "an identity that may not patch it", objects: applierFluxObjects(),
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true,
			wantSource: &configbutleraiv1alpha3.FluxTriggerSource{
				GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
				URL:           "https://github.com/acme/platform", Branch: "main",
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := triggerTestClient(t, testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
			r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux,
				applierFakeSource(tc.objects...), nil, tc.allowed)

			trigger := reconcileTrigger(t, r).(*configbutleraiv1alpha3.FluxTrigger)

			configured := findCondition(trigger.Status.Conditions, ApplyTriggerConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			ready := findCondition(trigger.Status.Conditions, ConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantStatus, ready.Status)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(trigger.Status.Conditions, ConditionTypeStalled))
			assert.Equal(t, tc.wantSource, trigger.Status.Source)
			assert.Equal(t, int64(1), trigger.Status.ObservedGeneration)
		})
	}
}

// TestApplyTriggerReconcile_ArgoCDVerdicts: an ArgoCDTrigger checks an Application, with automated
// sync on as a verdict of its own.
func TestApplyTriggerReconcile_ArgoCDVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		objects     []runtime.Object
		allowed     bool
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
	}{
		{name: "automated sync off", objects: []runtime.Object{applierArgoApplication(nil)}, allowed: true,
			wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded},
		{name: "automated sync switched off, keeping prune", allowed: true,
			objects: []runtime.Object{applierArgoApplication(map[string]any{
				"automated": map[string]any{"enabled": false, "prune": true},
			})},
			wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded},
		{name: "automated sync on", allowed: true,
			objects: []runtime.Object{applierArgoApplication(map[string]any{
				"automated": map[string]any{"prune": true},
			})},
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonAutomatedSyncEnabled, wantStalled: true},
		{name: "no such Application", allowed: true,
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonApplierNotFound},
		{name: "an identity that may not update it", objects: []runtime.Object{applierArgoApplication(nil)},
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := triggerTestClient(t, testArgoCDTrigger(configbutleraiv1alpha3.DefaultClusterProviderName))
			r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindArgoCD,
				applierFakeSource(tc.objects...), nil, tc.allowed)

			trigger := reconcileTrigger(t, r).(*configbutleraiv1alpha3.ArgoCDTrigger)

			configured := findCondition(trigger.Status.Conditions, ApplyTriggerConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(trigger.Status.Conditions, ConditionTypeStalled))
			if tc.objects != nil {
				assert.Equal(t, &configbutleraiv1alpha3.ArgoCDTriggerSource{
					RepoURL: "https://github.com/acme/platform", TargetRevision: "main",
				}, trigger.Status.Source)
			}
		})
	}
}

// TestApplyTriggerReconcile_AnUnreachableClusterKeepsThePublishedVerdict: a blip reading the
// applier's cluster says nothing about the applier, so it must not flip a True to Unknown and back on
// every hiccup, nor drop the source the GitTargets bind against.
func TestApplyTriggerReconcile_AnUnreachableClusterKeepsThePublishedVerdict(t *testing.T) {
	trigger := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	c := triggerTestClient(t, trigger)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, nil, errors.New("connection refused"), true)
	got := reconcileTrigger(t, r)
	configured := findCondition(*got.StatusConditions(), ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionUnknown, configured.Status, "nothing published yet")

	published := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	published.Status.Conditions = []metav1.Condition{{
		Type: ApplyTriggerConditionConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
		LastTransitionTime: metav1.Now(),
	}}
	published.Status.Source = &configbutleraiv1alpha3.FluxTriggerSource{URL: applierTestRepo, Branch: "main"}
	c = triggerTestClient(t, published)
	r = triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux, nil, errors.New("connection refused"), true)
	got = reconcileTrigger(t, r)
	configured = findCondition(*got.StatusConditions(), ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status, "the published verdict stands")
	url, branch, known := got.SourceBinding()
	assert.True(t, known, "and so does the source")
	assert.Equal(t, applierTestRepo, url)
	assert.Equal(t, "main", branch)
}

// TestApplyTriggerReconcile_AHangingClusterIsBounded: the applier's client has no request timeout,
// so a cluster that accepts the connection and never answers would hold the reconcile for as long as
// it liked. The check gives up, and a check that could not be made keeps the published verdict.
func TestApplyTriggerReconcile_AHangingClusterIsBounded(t *testing.T) {
	var sawDeadline bool
	published := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	published.Status.Conditions = []metav1.Condition{{
		Type: ApplyTriggerConditionConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
		LastTransitionTime: metav1.Now(),
	}}
	r := &ApplyTriggerReconciler{
		Client: triggerTestClient(t, published),
		Kind:   configbutleraiv1alpha3.ApplyTriggerKindFlux,
		ApplierClients: func(ctx context.Context, _ string) (dynamic.Interface, error) {
			_, sawDeadline = ctx.Deadline()
			return applierFakeSource(applierFluxObjects()...), nil
		},
		newApplier: func(_ string, client dynamic.Interface) applier.Client {
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

	got := reconcileTrigger(t, r)

	assert.True(t, sawDeadline, "the client lookup must run under a deadline")
	configured := findCondition(*got.StatusConditions(), ApplyTriggerConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status, "a timed-out check keeps the published verdict")
}

// TestApplyTriggerReconcile_TheTriggersClusterProviderAdmitsIt: the applier is reached with the
// trigger's ClusterProvider's credential, so that provider's accessFrom gates the check before the
// cluster is ever asked.
func TestApplyTriggerReconcile_TheTriggersClusterProviderAdmitsIt(t *testing.T) {
	cases := []struct {
		name        string
		cluster     string
		hub         *configbutleraiv1alpha3.ClusterProvider
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
		wantClients []string
	}{
		{name: "a provider that admits the namespace is asked", cluster: "hub", hub: admitting("hub", "team-a"),
			wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded, wantClients: []string{"hub"}},
		{name: "a provider that does not admit it is never asked", cluster: "hub", hub: admitting("hub", "team-b"),
			wantStatus: metav1.ConditionFalse, wantReason: authz.ReasonNamespaceNotAuthorized, wantStalled: true},
		{name: "a provider that does not exist is never asked, and may be on its way", cluster: "absent",
			wantStatus: metav1.ConditionFalse, wantReason: authz.ReasonClusterProviderNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := []ctrlclient.Object{testFluxTrigger(tc.cluster)}
			if tc.hub != nil {
				objects = append(objects, tc.hub)
			}
			var asked []string
			r := triggerReconciler(triggerTestClient(t, objects...), configbutleraiv1alpha3.ApplyTriggerKindFlux,
				applierFakeSource(applierFluxObjects()...), nil, true)
			source := r.ApplierClients
			r.ApplierClients = func(ctx context.Context, cluster string) (dynamic.Interface, error) {
				asked = append(asked, cluster)
				return source(ctx, cluster)
			}

			got := reconcileTrigger(t, r)

			configured := findCondition(*got.StatusConditions(), ApplyTriggerConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(*got.StatusConditions(), ConditionTypeStalled))
			assert.Equal(t, tc.wantClients, asked)
		})
	}
}

// TestApplyTriggerReconcile_AnUnresolvedOperationIsNotReady is the third review's fourth finding,
// seen from the trigger: an operation past its budget keeps its window open, and the trigger says so
// in Ready until the operation completes. It clears on its own, so it is never Stalled.
func TestApplyTriggerReconcile_AnUnresolvedOperationIsNotReady(t *testing.T) {
	trigger := testFluxTrigger(configbutleraiv1alpha3.DefaultClusterProviderName)
	trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, Reason: configbutleraiv1alpha3.ApplyReasonCatchUp,
		Commit: "4f2c1ab9d00d", RequestedAt: metav1.Now(), Message: "still applying",
	}
	c := triggerTestClient(t, trigger)
	r := triggerReconciler(c, configbutleraiv1alpha3.ApplyTriggerKindFlux,
		applierFakeSource(applierFluxObjects()...), nil, true)

	got := reconcileTrigger(t, r)

	ready := findCondition(*got.StatusConditions(), ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, ReasonOperationUnresolved, ready.Reason)
	assert.Contains(t, ready.Message, "the CatchUp apply of 4f2c1ab9")
	assert.False(t, conditionIsTrue(*got.StatusConditions(), ConditionTypeStalled))
	require.NotNil(t, got.Operation(), "the operation is the worker's, and left as it is")
	assert.Equal(t, configbutleraiv1alpha3.ApplyOperationUnresolved, got.Operation().Phase)
}

// TestApplyTriggerSpecOrOperationChanged_ReadsTheOperationPhase: the worker's status writes reach the
// reconciler only when they move the phase Ready reads.
func TestApplyTriggerSpecOrOperationChanged_ReadsTheOperationPhase(t *testing.T) {
	p := applyTriggerSpecOrOperationChanged()
	at := func(phase configbutleraiv1alpha3.ApplyOperationPhase, generation int64) ctrlclient.Object {
		trigger := testFluxTrigger("default")
		trigger.Generation = generation
		if phase != "" {
			trigger.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{Phase: phase}
		}
		return trigger
	}
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: at(configbutleraiv1alpha3.ApplyOperationRunning, 1),
		ObjectNew: at(configbutleraiv1alpha3.ApplyOperationUnresolved, 1),
	}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: at("", 1), ObjectNew: at("", 2)}), "a spec change")
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: at(configbutleraiv1alpha3.ApplyOperationRunning, 1),
		ObjectNew: at(configbutleraiv1alpha3.ApplyOperationRunning, 1),
	}), "a status write that moves nothing Ready reads")
}

// TestApplyTriggerReconcile_ClusterProviderAndNamespaceEventsFindTheirTriggers: a provider event
// re-runs the triggers naming it, and a namespace label change the triggers in it.
func TestApplyTriggerReconcile_ClusterProviderAndNamespaceEventsFindTheirTriggers(t *testing.T) {
	other := testFluxTrigger("other")
	other.Name = "elsewhere"
	c := triggerTestClient(t, testFluxTrigger("hub"), other)
	r := &ApplyTriggerReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplyTriggerKindFlux}

	requests := r.clusterProviderToTriggers(context.Background(), admitting("hub"))
	require.Len(t, requests, 1)
	assert.Equal(t, "checkout", requests[0].Name)

	ns := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	assert.Len(t, r.namespaceToTriggers(context.Background(), ns), 2)
}

// TestApplyTriggerReconcile_ArgoCDEventsFindTheirTriggers: the Argo CD kind lists its own.
func TestApplyTriggerReconcile_ArgoCDEventsFindTheirTriggers(t *testing.T) {
	c := triggerTestClient(t, testArgoCDTrigger("hub"), testFluxTrigger("hub"))
	r := &ApplyTriggerReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplyTriggerKindArgoCD}
	requests := r.clusterProviderToTriggers(context.Background(), admitting("hub"))
	assert.Len(t, requests, 1, "a FluxTrigger of the same name is another kind's")
}

func TestDescribeCommit_NamesAShortHash(t *testing.T) {
	assert.Equal(t, "4f2c1ab9", describeCommit("4f2c1ab9d00d"))
	assert.Equal(t, "4f2c", describeCommit("4f2c"))
	assert.Contains(t, describeCommit(""), "the remote does not carry")
}

// TestApplyTriggerReconcile_AGoneTriggerIsForgotten: a trigger that is gone has nothing to publish.
func TestApplyTriggerReconcile_AGoneTriggerIsForgotten(t *testing.T) {
	r := &ApplyTriggerReconciler{Client: triggerTestClient(t), Kind: configbutleraiv1alpha3.ApplyTriggerKindFlux}
	result, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: "team-a", Name: "gone"}})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	_, err = (&ApplyTriggerReconciler{Kind: "WebhookTrigger"}).Reconcile(context.Background(), ctrl.Request{})
	require.Error(t, err, "a kind this release does not serve")
}
