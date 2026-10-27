// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// fakeDiscovery serves resources, and fails with err when it is set.
func fakeDiscovery(err error, resources ...*metav1.APIResourceList) discovery.DiscoveryInterface {
	fake := &clienttesting.Fake{Resources: resources}
	if err != nil {
		fake.AddReactor("get", "group", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, err
		})
	}
	return &fakediscovery.FakeDiscovery{Fake: fake}
}

func commitRequestResources(groupVersion string, names ...string) *metav1.APIResourceList {
	list := &metav1.APIResourceList{GroupVersion: groupVersion}
	for _, name := range names {
		list.APIResources = append(list.APIResources, metav1.APIResource{Name: name, Namespaced: true})
	}
	return list
}

// The CRD's state is read by discovery alone: no permission on CRDs is needed, so a credential scoped to
// namespaces reports it too. Each reason that is not True says how to fix it.
func TestCommitRequestCRDReport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		discovery  discovery.DiscoveryInterface
		wantReason string
		wantFix    string
	}{
		{name: "no configbutler.ai group", discovery: fakeDiscovery(nil),
			wantReason: ReasonCommitRequestsCRDNotInstalled, wantFix: "kubectl apply --server-side"},
		{name: "the group without commitrequests",
			discovery:  fakeDiscovery(nil, commitRequestResources("configbutler.ai/v1alpha3", "gittargets")),
			wantReason: ReasonCommitRequestsCRDNotInstalled, wantFix: "commitrequest-crd.yaml"},
		{name: "another version only",
			discovery:  fakeDiscovery(nil, commitRequestResources("configbutler.ai/v1alpha2", "commitrequests")),
			wantReason: ReasonCommitRequestsVersionNotServed, wantFix: "commitrequest-crd.yaml"},
		{name: "served", discovery: fakeDiscovery(nil,
			commitRequestResources("configbutler.ai/v1alpha3", "commitrequests"))},
		{name: "unreadable", discovery: fakeDiscovery(errors.New("connection refused")),
			wantReason: ReasonCommitRequestsUnreachable, wantFix: "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := commitRequestCRDReport(tc.discovery)
			assert.Equal(t, tc.wantReason, report.Reason)
			assert.Contains(t, report.Message, tc.wantFix)
		})
	}
}

// A provider watches the namespaces it lends the GitTargets that mirror through it, "*" alone once any
// grant lends every namespace.
func TestCommitRequestServer_LentNamespaces(t *testing.T) {
	provider := kubeconfigProvider("edge", true)
	provider.Spec.AllowedGitTargets = []configv1alpha3.KubeProviderGitTargetGrant{{
		ConsumerNamespaces: configv1alpha3.ConsumerNamespaces{From: configv1alpha3.GrantFromAll},
		SourceNamespaces:   []configv1alpha3.ScopeNamespace{configv1alpha3.ConsumerNamespaceVariable, "shared"},
	}}
	target := func(namespace, provider string) configv1alpha3.GitTarget {
		return configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: namespace},
			Spec: configv1alpha3.GitTargetSpec{KubeProviderRef: meta.NamespacedObjectReference{
				Name: provider, Namespace: "default",
			}},
		}
	}
	c := newCommitRequestClient(t, nil)
	s := &CommitRequestServer{Client: c}

	lent, err := s.lentNamespaces(context.Background(), provider, []configv1alpha3.GitTarget{
		target("team-b", "edge"), target("team-a", "edge"), target("team-c", "other"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "team-a", "team-b"}, lent, "only its own GitTargets, sorted, once each")

	provider.Spec.AllowedGitTargets[0].SourceNamespaces = []configv1alpha3.ScopeNamespace{"*"}
	lent, err = s.lentNamespaces(context.Background(), provider, []configv1alpha3.GitTarget{target("team-a", "edge")})
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, lent)
}

// A watcher reports what it watches, the namespaces it may not list or watch, and the ones where a
// finished request could not be deleted, which leaves serving True.
func TestCommitRequestWatcher_Report(t *testing.T) {
	watcher := &commitRequestWatcher{namespaces: []string{"shop"}, forbidden: map[string]bool{},
		undeletable: map[string]bool{}}
	report := watcher.report(true)
	assert.Equal(t, metav1.ConditionTrue, report.Status)
	assert.Equal(t, ReasonCommitRequestsWatching, report.Reason)
	assert.Contains(t, report.Message, "1 namespaces: shop")

	watcher.markUndeletable("shop")
	report = watcher.report(true)
	assert.Equal(t, metav1.ConditionTrue, report.Status, "serving still works")
	assert.Contains(t, report.Message, "cannot be deleted")

	watcher.mark(watcher.forbidden, "shop", true)
	report = watcher.report(true)
	assert.Equal(t, metav1.ConditionFalse, report.Status)
	assert.Equal(t, ReasonCommitRequestsForbidden, report.Reason)
	assert.Contains(t, report.Message, "namespace shop")
	assert.Contains(t, report.Message, "patch and delete", "the verbs ReconcileAndDeleteFinished needs")

	every := &commitRequestWatcher{namespaces: []string{"*"}, forbidden: map[string]bool{},
		undeletable: map[string]bool{}}
	assert.Contains(t, every.report(false).Message, "every namespace")
}

// On a source cluster a request is admitted by a grant's sourceNamespaces, and only in a namespace the
// KubeProvider lends the GitTarget; on the control cluster by consumerNamespaces. Each refusal says
// which field applies.
func TestCommitRequestCore_AdmissionBySide(t *testing.T) {
	t.Run("a source cluster request with no sourceNamespaces grant", func(t *testing.T) {
		cr := newCommitRequest("save")
		cr.Spec.GitTargetRef.Namespace = "default"
		target := mirroringTarget("edge")
		target.Spec.AllowedCommitRequests = nil
		c := newCommitRequestClient(t, nil, cr, target, kubeconfigProvider("edge", true))
		f := committed()
		serveStep(t, &commitRequestCore{reader: c, finalizer: f}, watcherPath(c, "edge", false), "save")

		assert.Empty(t, f.calls)
		ready := commitRequestReady(t, c, "save")
		require.NotNil(t, ready)
		assert.Equal(t, crReasonNotAllowed, ready.Reason)
		assert.Contains(t, ready.Message, "sourceNamespaces")
	})
	t.Run("a source cluster request in a namespace that is not lent", func(t *testing.T) {
		cr := newCommitRequest("save")
		cr.Namespace = "elsewhere"
		cr.Spec.GitTargetRef.Namespace = "default"
		c := newCommitRequestClient(t, nil, cr, mirroringTarget("edge"), kubeconfigProvider("edge", true))
		f := committed()
		_, err := (&commitRequestCore{reader: c, finalizer: f}).serve(context.Background(),
			watcherPath(c, "edge", false), types.NamespacedName{Namespace: "elsewhere", Name: "save"})
		require.NoError(t, err)

		assert.Empty(t, f.calls)
		var got configv1alpha3.CommitRequest
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "elsewhere", Name: "save"}, &got))
		ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady)
		require.NotNil(t, ready)
		assert.Equal(t, crReasonNotAllowed, ready.Reason)
		assert.Contains(t, ready.Message, "does not lend namespace")
	})
	t.Run("a control-cluster request no consumerNamespaces grant admits", func(t *testing.T) {
		target := mirroringTarget(localProvider)
		target.Spec.AllowedCommitRequests = []configv1alpha3.GitTargetCommitRequestGrant{
			{SourceNamespaces: []configv1alpha3.SourceNamespace{"*"}},
		}
		c := newCommitRequestClient(t, nil, newCommitRequest("save"), target)
		serveStep(t, &commitRequestCore{reader: c, finalizer: committed()}, controlPath(c), "save")

		ready := commitRequestReady(t, c, "save")
		require.NotNil(t, ready)
		assert.Equal(t, crReasonNotAllowed, ready.Reason)
		assert.Contains(t, ready.Message, "consumerNamespaces")
	})
}

// fakeClusters serves one source cluster: its dynamic client, and its discovery.
type fakeClusters struct {
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface
}

func (f fakeClusters) ApplierDynamicClient(context.Context, string) (dynamic.Interface, error) {
	return f.dynamic, nil
}

func (f fakeClusters) SourceDiscovery(context.Context, string) (discovery.DiscoveryInterface, error) {
	return f.discovery, nil
}

// sourceCommitRequest is a CommitRequest as it is stored on a source cluster.
func sourceCommitRequest(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "CommitRequest",
		"metadata": map[string]any{
			"namespace": namespace, "name": name, "uid": "uid-" + name, "resourceVersion": "1",
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"gitTargetRef": map[string]any{"namespace": "default", "name": "team-a-config"},
			"message":      "save: from the workspace",
		},
	}}
	return u
}

// End to end through the server: a KubeProvider that reconciles its CommitRequests watches the namespace
// it lends, serves a request created there through the core, writes its status back on that cluster,
// and reports what it watches. With ReconcileAndDeleteFinished the finished request is stamped for
// deletion.
func TestCommitRequestServer_ServesARequestOnASourceCluster(t *testing.T) {
	provider := kubeconfigProvider("edge", true)
	provider.Spec.CommitRequests = configv1alpha3.CommitRequestsReconcileAndDeleteFinished
	c := newCommitRequestClient(t, nil, mirroringTarget("edge"), provider)
	gvr := commitRequestGVR()
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "CommitRequestList"}, sourceCommitRequest("default", "save"))
	f := committed()
	s := &CommitRequestServer{
		Client: c, Finalizer: f, TTL: time.Hour, Log: logr.Discard(),
		Clusters: fakeClusters{dynamic: source, discovery: fakeDiscovery(nil,
			commitRequestResources("configbutler.ai/v1alpha3", "commitrequests"))},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.syncProviders(ctx)

	report, ok := s.CommitRequestsReport(provider.Key())
	require.True(t, ok)
	assert.Equal(t, ReasonCommitRequestsWatching, report.Reason)
	assert.Equal(t, []string{"default"}, report.Namespaces, "the namespace the provider lends its GitTarget")
	select {
	case ev := <-s.CommitRequestsEvents():
		assert.Equal(t, provider.Name, ev.Object.GetName(), "the KubeProvider is told its report changed")
	default:
		t.Fatal("no event for the changed report")
	}

	require.Eventually(t, func() bool {
		u, err := source.Resource(gvr).Namespace("default").Get(ctx, "save", metav1.GetOptions{})
		if err != nil {
			return false
		}
		cr, convErr := commitRequestFromUnstructured(u)
		if convErr != nil {
			return false
		}
		ready := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady)
		_, stamped := cr.Annotations[CommitRequestDeleteAfterAnnotation]
		return ready != nil && ready.Reason == crReasonCommitted && stamped
	}, 10*time.Second, 50*time.Millisecond, "the request is served on its own cluster, and stamped for deletion")
	require.Len(t, f.calls, 1)
	assert.Equal(t, git.AttributionNotAttempted, f.calls[0].Attribution, "attribution is off")

	provider.Spec.CommitRequests = configv1alpha3.CommitRequestsIgnore
	require.NoError(t, c.Update(ctx, provider))
	s.syncProviders(ctx)
	_, ok = s.CommitRequestsReport(provider.Key())
	assert.False(t, ok, "a provider that ignores CommitRequests reports nothing")
}

// A KubeProvider carries CommitRequestsServed and status.commitRequests only while it serves its
// cluster's CommitRequests.
func TestKubeProviderReconciler_CommitRequestsServed(t *testing.T) {
	reporter := &fixedCommitRequestsReporter{report: CommitRequestsReport{
		Status: metav1.ConditionTrue, Reason: ReasonCommitRequestsWatching, Message: "watching",
		Namespaces: []string{"shop"},
	}}
	r := &KubeProviderReconciler{CommitRequests: reporter}

	provider := kubeconfigProvider("edge", true)
	st := beginStatus(nil, nil, provider)
	r.applyCommitRequestsServed(st, provider)
	served := apimeta.FindStatusCondition(provider.Status.Conditions, KubeProviderConditionCommitRequestsServed)
	require.NotNil(t, served)
	assert.Equal(t, ReasonCommitRequestsWatching, served.Reason)
	require.NotNil(t, provider.Status.CommitRequests)
	assert.Equal(t, []string{"shop"}, provider.Status.CommitRequests.Namespaces)

	provider.Spec.CommitRequests = configv1alpha3.CommitRequestsIgnore
	r.applyCommitRequestsServed(st, provider)
	assert.Nil(t, apimeta.FindStatusCondition(provider.Status.Conditions, KubeProviderConditionCommitRequestsServed),
		"removed while the provider ignores CommitRequests")
	assert.Nil(t, provider.Status.CommitRequests)
}

type fixedCommitRequestsReporter struct {
	report CommitRequestsReport
}

func (f *fixedCommitRequestsReporter) CommitRequestsReport(types.NamespacedName) (CommitRequestsReport, bool) {
	return f.report, true
}

func (f *fixedCommitRequestsReporter) CommitRequestsEvents() <-chan event.GenericEvent { return nil }
