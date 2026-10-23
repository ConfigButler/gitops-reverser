// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier"
	"github.com/ConfigButler/gitops-reverser/internal/applier/argosim"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
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

// applierFakeSource is an applier cluster holding objs, with Flux's controllers simulated in it.
func applierFakeSource(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	return applierSim(t, objs...).Client
}

// applierSim is an applier cluster holding objs, with Flux's controllers simulated in it, and the
// API server's ownership of an Application's automated sync switch.
func applierSim(t *testing.T, objs ...runtime.Object) *fluxsim.Sim {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			fluxsim.KustomizationGVR(): "KustomizationList",
			fluxsim.GitRepositoryGVR(): "GitRepositoryList",
			{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}: "HelmReleaseList",
			{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		}, objs...)
	argosim.Serve(client)
	return fluxsim.New(t, client)
}

// The applier claim tests reach their appliers through KubeProviders in claimProviderNamespace, a
// namespace of their own, so a provider admits team-a only through spec.accessFrom: the namespace
// policy is what those tests exercise. claimLocalProvider is the operator's own cluster there.
const (
	claimProviderNamespace = "clusters"
	claimLocalProvider     = "local"
)

// claimProviderRef names the KubeProvider name in claimProviderNamespace.
func claimProviderRef(name string) meta.NamespacedObjectReference {
	return meta.NamespacedObjectReference{Namespace: claimProviderNamespace, Name: name}
}

// claimProviderKey is the key of the KubeProvider name in claimProviderNamespace, as
// ApplierClients is asked for it.
func claimProviderKey(name string) string {
	return claimProviderNamespace + "/" + name
}

// admitting is a KubeProvider in claimProviderNamespace that admits namespaces to mirror their own
// namespace, and to hold appliers in any namespace (the test appliers live in flux-system and
// argocd).
func admitting(name string, namespaces ...string) *configbutleraiv1alpha3.KubeProvider {
	return &configbutleraiv1alpha3.KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: claimProviderNamespace},
		Spec: configbutleraiv1alpha3.KubeProviderSpec{
			InCluster:            &configbutleraiv1alpha3.KubeProviderInCluster{},
			AllowedGitTargets:    mirrorGrants(namespaces...),
			AllowedApplierClaims: applierGrants([]configbutleraiv1alpha3.ScopeNamespace{"*"}, namespaces...),
		},
	}
}

// testFluxClaim is the FluxClaim team-a/checkout holding flux-system/checkout in the cluster of
// the KubeProvider cluster in claimProviderNamespace.
func testFluxClaim(cluster string) *configbutleraiv1alpha3.FluxClaim {
	return &configbutleraiv1alpha3.FluxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.FluxClaimSpec{
			KubeProviderRef:  claimProviderRef(cluster),
			KustomizationRef: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "checkout"},
		},
	}
}

// testArgoCDClaim is the ArgoCDClaim team-a/checkout holding argocd/checkout in the cluster of
// the KubeProvider cluster in claimProviderNamespace.
func testArgoCDClaim(cluster string) *configbutleraiv1alpha3.ArgoCDClaim {
	return &configbutleraiv1alpha3.ArgoCDClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a", Generation: 1},
		Spec: configbutleraiv1alpha3.ArgoCDClaimSpec{
			KubeProviderRef: claimProviderRef(cluster),
			ApplicationRef:  meta.NamespacedObjectReference{Namespace: "argocd", Name: "checkout"},
		},
	}
}

// referencingTarget is a GitTarget team-a/name mirroring through the KubeProvider
// claimLocalProvider and writing main of the test repository through the GitProvider platform,
// naming the applier claim kind/checkout.
func referencingTarget(name, kind string) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			KubeProviderRef: claimProviderRef(claimLocalProvider),
			GitBranchRef:    meta.NamespacedObjectReference{Name: gitBranchNameForTest("platform", "main")},
			Path:            name,
			ApplierClaimRef: &configbutleraiv1alpha3.ApplierClaimReference{Kind: kind, Name: "checkout"},
		},
	}
}

// platformProvider is the GitProvider team-a/platform of the test repository.
func platformProvider() *configbutleraiv1alpha3.GitProvider {
	return &configbutleraiv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "team-a"},
		Spec:       configbutleraiv1alpha3.GitProviderSpec{URL: applierTestRepo},
	}
}

// referencedIn is a GitProvider platform and a GitTarget checkout-<kind> naming the applier claim
// kind/checkout, both in namespace: what makes an applier claim there eligible.
func referencedIn(namespace, kind string) []ctrlclient.Object {
	provider := platformProvider()
	provider.Namespace = namespace
	target := referencingTarget("checkout-"+strings.ToLower(kind), kind)
	target.Namespace = namespace
	return []ctrlclient.Object{provider, target}
}

// unreferenced marks a claimTestClient that holds no GitTarget naming the applier claims.
type unreferenced struct{ ctrlclient.Object }

// claimTestClient is a control cluster holding objects, the KubeProviders claimLocalProvider and
// "hub" admitting team-a unless objects carries its own, the GitProvider platform unless objects carries a
// GitProvider, and a GitTarget naming each applier claim kind's checkout unless objects carries a
// GitTarget or unreferenced{}.
func claimTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	held := map[string]bool{}
	hasTarget, hasProvider := false, false
	kept := objects[:0:0]
	for _, obj := range objects {
		switch obj.(type) {
		case *configbutleraiv1alpha3.KubeProvider:
			held[obj.GetName()] = true
		case *configbutleraiv1alpha3.GitTarget, unreferenced:
			hasTarget = true
		case *configbutleraiv1alpha3.GitProvider:
			hasProvider = true
			// The GitBranch its GitTargets write through: "<provider>-main".
			kept = append(kept, gitBranch(obj.GetName(), obj.GetNamespace()))
		}
		if _, marker := obj.(unreferenced); !marker {
			kept = append(kept, obj)
		}
	}
	objects = kept
	for _, name := range []string{claimLocalProvider, "hub"} {
		if !held[name] {
			objects = append(objects, admitting(name, "team-a"))
		}
	}
	if !hasProvider {
		objects = append(objects, platformProvider(), gitBranch("platform", "team-a"))
	}
	if !hasTarget {
		objects = append(objects,
			referencingTarget("checkout-flux", configbutleraiv1alpha3.ApplierClaimKindFlux),
			referencingTarget("checkout-argo", configbutleraiv1alpha3.ApplierClaimKindArgoCD))
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithIndex(&configbutleraiv1alpha3.GitBranch{}, GitBranchProviderIndex, IndexGitBranchProvider).
		WithStatusSubresource(&configbutleraiv1alpha3.FluxClaim{}, &configbutleraiv1alpha3.ArgoCDClaim{}).
		Build()
}

// claimReconciler checks appliers against a fake cluster whose access review answers allowed.
func claimReconciler(
	c ctrlclient.Client, kind string, source dynamic.Interface, sourceErr error, allowed bool,
) *ApplierClaimReconciler {
	return &ApplierClaimReconciler{
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

// claimKey is the applier claim every test reconciles.
var claimKey = k8stypes.NamespacedName{Namespace: "team-a", Name: "checkout"}

// reconcileClaim reconciles team-a/checkout until it settles and returns the applier claim as it is
// then. While its Kustomization's suspend is being handled, a FluxClaim is read again at the
// suspending interval. Every pass publishes its conditions: the one that adopts too, which also adds
// the finalizer, since nothing re-enqueues an applier claim for a metadata-only update. A FluxClaim
// carries the finalizer exactly while it is Configured: it holds its Kustomization. An
// ArgoCDClaim does too, unless the switch it holds is somebody else's.
func reconcileClaim(t *testing.T, r *ApplierClaimReconciler) configbutleraiv1alpha3.ApplierClaim {
	t.Helper()
	var claim configbutleraiv1alpha3.ApplierClaim
	require.Eventually(t, func() bool {
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: claimKey})
		require.NoError(t, err)
		claim = configbutleraiv1alpha3.NewApplierClaim(r.Kind)
		require.NoError(t, r.Get(context.Background(), claimKey, claim))
		require.NotNil(t, findCondition(*claim.StatusConditions(), ConditionTypeReady),
			"every pass publishes Ready")
		held := findCondition(*claim.StatusConditions(), ApplierClaimConditionHeld)
		if conditionIsTrue(*claim.StatusConditions(), ApplierClaimConditionConfigured) && held != nil &&
			!strings.Contains(held.Message, "which owns the switch") {
			require.True(t, controllerutil.ContainsFinalizer(claim, claimFinalizer(claim)),
				"an applier claim that holds its applier carries the finalizer")
		}
		if held != nil && held.Reason == ReasonSuspending {
			assert.Equal(t, applierClaimSuspendingInterval, result.RequeueAfter, "a suspend is waited for closely")
			return false
		}
		want := heldClaimSteadyInterval
		if !conditionIsTrue(*claim.StatusConditions(), ConditionTypeReady) &&
			!conditionIsTrue(*claim.StatusConditions(), ConditionTypeStalled) {
			want = applierClaimSettleInterval
		}
		assert.Equal(t, want, result.RequeueAfter, "an applier claim that may clear on its own is read again soon")
		return true
	}, 5*time.Second, 5*time.Millisecond)
	return claim
}

func TestApplierClaimReconcile_FluxVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		objects     []runtime.Object
		allowed     bool
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
		wantSource  *configbutleraiv1alpha3.FluxClaimSource
	}{
		{name: "a Kustomization applying a GitRepository on a branch", objects: applierFluxObjects(),
			allowed: true, wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded,
			wantSource: &configbutleraiv1alpha3.FluxClaimSource{
				GitRepository: meta.NamespacedObjectReference{Namespace: "flux-system", Name: "platform"},
				URL:           "https://github.com/acme/platform", Branch: "main",
			}},
		{name: "no such Kustomization, which may be on its way", allowed: true,
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonApplierNotFound},
		{name: "an identity that may not patch it, which publishes no source", objects: applierFluxObjects(),
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claimTestClient(t, testFluxClaim(claimLocalProvider))
			r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux,
				applierFakeSource(t, tc.objects...), nil, tc.allowed)

			claim := reconcileClaim(t, r).(*configbutleraiv1alpha3.FluxClaim)

			configured := findCondition(claim.Status.Conditions, ApplierClaimConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			ready := findCondition(claim.Status.Conditions, ConditionTypeReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantStatus, ready.Status)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(claim.Status.Conditions, ConditionTypeStalled))
			assert.Equal(t, tc.wantSource, claim.Status.Source)
			assert.Equal(t, int64(1), claim.Status.ObservedGeneration)
		})
	}
}

// TestApplierClaimReconcile_ArgoCDVerdicts: an ArgoCDClaim checks an Application, with automated
// sync on as a verdict of its own.
func TestApplierClaimReconcile_ArgoCDVerdicts(t *testing.T) {
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
			wantStatus: metav1.ConditionTrue, wantReason: ReasonSucceeded},
		{name: "no such Application", allowed: true,
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonApplierNotFound},
		{name: "an identity that may not update it", objects: []runtime.Object{applierArgoApplication(nil)},
			wantStatus: metav1.ConditionFalse, wantReason: applier.ReasonForbidden, wantStalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claimTestClient(t, testArgoCDClaim(claimLocalProvider))
			r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindArgoCD,
				applierFakeSource(t, tc.objects...), nil, tc.allowed)

			claim := reconcileClaim(t, r).(*configbutleraiv1alpha3.ArgoCDClaim)

			configured := findCondition(claim.Status.Conditions, ApplierClaimConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(claim.Status.Conditions, ConditionTypeStalled))
			if tc.wantStatus == metav1.ConditionTrue {
				assert.Equal(t, &configbutleraiv1alpha3.ArgoCDClaimSource{
					RepoURL: "https://github.com/acme/platform", TargetRevision: "main",
				}, claim.Status.Source)
			}
		})
	}
}

// TestApplierClaimReconcile_AnUnreachableClusterKeepsThePublishedVerdict: a blip reading the
// applier's cluster says nothing about the applier, so it must not flip a True to Unknown and back on
// every hiccup, nor drop the source the GitTargets bind against.
func TestApplierClaimReconcile_AnUnreachableClusterKeepsThePublishedVerdict(t *testing.T) {
	claim := testFluxClaim(claimLocalProvider)
	c := claimTestClient(t, claim)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux, nil, errors.New("connection refused"), true)
	got := reconcileClaim(t, r)
	configured := findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionUnknown, configured.Status, "nothing published yet")

	published := testFluxClaim(claimLocalProvider)
	published.Status.Conditions = []metav1.Condition{{
		Type: ApplierClaimConditionConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
		LastTransitionTime: metav1.Now(),
	}}
	published.Status.Source = &configbutleraiv1alpha3.FluxClaimSource{URL: applierTestRepo, Branch: "main"}
	c = claimTestClient(t, published)
	r = claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux, nil, errors.New("connection refused"), true)
	got = reconcileClaim(t, r)
	configured = findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status, "the published verdict stands")
	url, branch, known := got.SourceBinding()
	assert.True(t, known, "and so does the source")
	assert.Equal(t, applierTestRepo, url)
	assert.Equal(t, "main", branch)
}

// TestApplierClaimReconcile_AHangingClusterIsBounded: the applier's client has no request timeout,
// so a cluster that accepts the connection and never answers would hold the reconcile for as long as
// it liked. The check gives up, and a check that could not be made keeps the published verdict.
func TestApplierClaimReconcile_AHangingClusterIsBounded(t *testing.T) {
	var sawDeadline bool
	source := applierFakeSource(t, applierFluxObjects()...)
	published := testFluxClaim(claimLocalProvider)
	published.Status.Conditions = []metav1.Condition{{
		Type: ApplierClaimConditionConfigured, Status: metav1.ConditionTrue, Reason: ReasonSucceeded,
		LastTransitionTime: metav1.Now(),
	}}
	r := &ApplierClaimReconciler{
		Client: claimTestClient(t, published),
		Kind:   configbutleraiv1alpha3.ApplierClaimKindFlux,
		ApplierClients: func(ctx context.Context, _ string) (dynamic.Interface, error) {
			_, sawDeadline = ctx.Deadline()
			return source, nil
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

	got := reconcileClaim(t, r)

	assert.True(t, sawDeadline, "the client lookup must run under a deadline")
	configured := findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status, "a timed-out check keeps the published verdict")
}

// TestApplierClaimReconcile_TheClaimsKubeProviderAdmitsIt: the applier is reached with the
// applier claim's KubeProvider's credential, so that provider's accessFrom gates the check before the
// cluster is ever asked.
func TestApplierClaimReconcile_TheClaimsKubeProviderAdmitsIt(t *testing.T) {
	cases := []struct {
		name        string
		cluster     string
		hub         *configbutleraiv1alpha3.KubeProvider
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantStalled bool
		wantClients []string
	}{
		{
			name:        "a provider that admits the namespace is asked",
			cluster:     "hub",
			hub:         admitting("hub", "team-a"),
			wantStatus:  metav1.ConditionTrue,
			wantReason:  ReasonSucceeded,
			wantClients: []string{claimProviderKey("hub")},
		},
		{name: "a provider that does not admit it is never asked", cluster: "hub", hub: admitting("hub", "team-b"),
			wantStatus: metav1.ConditionFalse, wantReason: authz.ReasonNamespaceNotAuthorized, wantStalled: true},
		{name: "a provider that does not exist is never asked, and may be on its way", cluster: "absent",
			wantStatus: metav1.ConditionFalse, wantReason: authz.ReasonKubeProviderNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := []ctrlclient.Object{testFluxClaim(tc.cluster)}
			if tc.hub != nil {
				objects = append(objects, tc.hub)
			}
			var asked []string
			r := claimReconciler(claimTestClient(t, objects...), configbutleraiv1alpha3.ApplierClaimKindFlux,
				applierFakeSource(t, applierFluxObjects()...), nil, true)
			source := r.ApplierClients
			r.ApplierClients = func(ctx context.Context, cluster string) (dynamic.Interface, error) {
				asked = append(asked, cluster)
				return source(ctx, cluster)
			}

			got := reconcileClaim(t, r)

			configured := findCondition(*got.StatusConditions(), ApplierClaimConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tc.wantStatus, configured.Status, configured.Message)
			assert.Equal(t, tc.wantReason, configured.Reason)
			assert.Equal(t, tc.wantStalled, conditionIsTrue(*got.StatusConditions(), ConditionTypeStalled))
			assert.Equal(t, tc.wantClients, slices.Compact(asked), "only the applier claim's own cluster is asked")
		})
	}
}

// TestApplierClaimReconcile_AnUnresolvedOperationIsNotReady is the third review's fourth finding,
// seen from the applier claim: an operation past its budget keeps its window open, and the applier claim says so
// in Ready until the operation completes. It clears on its own, so it is never Stalled.
func TestApplierClaimReconcile_AnUnresolvedOperationIsNotReady(t *testing.T) {
	claim := testFluxClaim(claimLocalProvider)
	claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{
		Phase: configbutleraiv1alpha3.ApplyOperationUnresolved, Reason: configbutleraiv1alpha3.ApplyReasonCatchUp,
		Commit: "4f2c1ab9d00d", Token: "T1", RequestedAt: metav1.Now(), Message: "still applying",
	}
	c := claimTestClient(t, claim)
	r := claimReconciler(c, configbutleraiv1alpha3.ApplierClaimKindFlux,
		applierFakeSource(t, applierFluxObjects()...), nil, true)

	got := reconcileClaim(t, r)

	ready := findCondition(*got.StatusConditions(), ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, ReasonOperationUnresolved, ready.Reason)
	assert.Contains(t, ready.Message, "the CatchUp apply of 4f2c1ab9")
	assert.False(t, conditionIsTrue(*got.StatusConditions(), ConditionTypeStalled))
	require.NotNil(t, got.Operation(), "the operation is the worker's, and left as it is")
	assert.Equal(t, configbutleraiv1alpha3.ApplyOperationUnresolved, got.Operation().Phase)
}

// TestApplierClaimSpecOrOperationChanged_ReadsTheOperationPhase: the worker's status writes reach the
// reconciler only when they move the phase Ready reads.
func TestApplierClaimSpecOrOperationChanged_ReadsTheOperationPhase(t *testing.T) {
	p := applierClaimSpecOrOperationChanged()
	at := func(phase configbutleraiv1alpha3.ApplyOperationPhase, generation int64) ctrlclient.Object {
		claim := testFluxClaim(claimLocalProvider)
		claim.Generation = generation
		if phase != "" {
			claim.Status.Operation = &configbutleraiv1alpha3.ApplyOperation{Phase: phase}
		}
		return claim
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

// TestApplierClaimReconcile_KubeProviderAndNamespaceEventsFindTheirClaims: a provider event
// re-runs every applier claim, since it admits the GitTargets naming applier claims too, and a namespace label
// change the applier claims in it.
func TestApplierClaimReconcile_KubeProviderAndNamespaceEventsFindTheirClaims(t *testing.T) {
	other := testFluxClaim("other")
	other.Name = "elsewhere"
	c := claimTestClient(t, testFluxClaim("hub"), other)
	r := &ApplierClaimReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplierClaimKindFlux}

	assert.Len(t, r.kubeProviderToClaims(context.Background(), admitting("hub")), 2)

	ns := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	assert.Len(t, r.namespaceToClaims(context.Background(), ns), 2)
}

// TestApplierClaimReconcile_ArgoCDEventsFindTheirClaims: the Argo CD kind lists its own.
func TestApplierClaimReconcile_ArgoCDEventsFindTheirClaims(t *testing.T) {
	c := claimTestClient(t, testArgoCDClaim("hub"), testFluxClaim("hub"))
	r := &ApplierClaimReconciler{Client: c, Kind: configbutleraiv1alpha3.ApplierClaimKindArgoCD}
	requests := r.kubeProviderToClaims(context.Background(), admitting("hub"))
	assert.Len(t, requests, 1, "a FluxClaim of the same name is another kind's")
}

// TestApplierClaimReconcile_AGoneClaimIsForgotten: an applier claim that is gone has nothing to publish.
func TestApplierClaimReconcile_AGoneClaimIsForgotten(t *testing.T) {
	r := &ApplierClaimReconciler{Client: claimTestClient(t), Kind: configbutleraiv1alpha3.ApplierClaimKindFlux}
	result, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: k8stypes.NamespacedName{Namespace: "team-a", Name: "gone"}})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	_, err = (&ApplierClaimReconciler{Kind: "WebhookClaim"}).Reconcile(context.Background(), ctrl.Request{})
	require.Error(t, err, "a kind this release does not serve")
}
