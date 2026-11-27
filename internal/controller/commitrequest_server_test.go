// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
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

	every.unreachable = errors.New("no route to host")
	report = every.report(false)
	assert.Equal(t, ReasonCommitRequestsUnreachable, report.Reason)
	assert.Contains(t, report.Message, "no route to host")
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

// fakeClusters serves one source cluster: its dynamic client, and its discovery, under one credential.
type fakeClusters struct {
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface
}

func (f fakeClusters) SourceDynamicClient(context.Context, string) (dynamic.Interface, string, error) {
	return f.dynamic, "v1", nil
}

func (f fakeClusters) SourceDiscovery(context.Context, string) (discovery.DiscoveryInterface, string, error) {
	return f.discovery, "v1", nil
}

// rotatingClusters serves one source cluster whose credential the test rotates. Each credential reaches
// the cluster through a client of its own, so a client built from an old one sees none of what the new
// one does.
type rotatingClusters struct {
	discovery discovery.DiscoveryInterface

	mu         sync.Mutex
	credential string
	clients    map[string]dynamic.Interface
	// built are the credentials a client was built from, in order.
	built []string
}

func (r *rotatingClusters) rotate(credential string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.credential = credential
}

func (r *rotatingClusters) SourceDynamicClient(context.Context, string) (dynamic.Interface, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.built = append(r.built, r.credential)
	return r.clients[r.credential], r.credential, nil
}

func (r *rotatingClusters) builtFrom() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.built)
}

func (r *rotatingClusters) SourceDiscovery(context.Context, string) (discovery.DiscoveryInterface, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discovery, r.credential, nil
}

// sourceCommitRequest is a CommitRequest as it is stored on a source cluster, in namespace default.
func sourceCommitRequest(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "configbutler.ai/v1alpha3",
		"kind":       "CommitRequest",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "uid": "uid-" + name, "resourceVersion": "1",
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
		map[schema.GroupVersionResource]string{gvr: "CommitRequestList"}, sourceCommitRequest("save"))
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

// A rotated kubeconfig moves a provider's watcher to the new credential: its watches, and the status it
// writes, run on a client built from it, rather than on the one the watcher started with.
func TestCommitRequestServer_ARotatedCredentialMovesTheWatcher(t *testing.T) {
	provider := kubeconfigProvider("edge", true)
	c := newCommitRequestClient(t, nil, mirroringTarget("edge"), provider)
	gvr := commitRequestGVR()
	listKinds := map[schema.GroupVersionResource]string{gvr: "CommitRequestList"}
	before := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	after := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds,
		sourceCommitRequest("save"))
	clusters := &rotatingClusters{
		discovery:  fakeDiscovery(nil, commitRequestResources("configbutler.ai/v1alpha3", "commitrequests")),
		credential: "v1",
		clients:    map[string]dynamic.Interface{"v1": before, "v2": after},
	}
	f := committed()
	s := &CommitRequestServer{Client: c, Finalizer: f, Clusters: clusters, Log: logr.Discard()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.syncProviders(ctx)
	report, ok := s.CommitRequestsReport(provider.Key())
	require.True(t, ok)
	require.Equal(t, ReasonCommitRequestsWatching, report.Reason)
	require.Eventually(t, func() bool { return len(clusters.builtFrom()) > 0 }, 10*time.Second,
		10*time.Millisecond, "the watcher runs on the first credential")

	clusters.rotate("v2")
	s.syncProviders(ctx)

	require.Eventually(t, func() bool {
		u, err := after.Resource(gvr).Namespace("default").Get(ctx, "save", metav1.GetOptions{})
		if err != nil {
			return false
		}
		cr, convErr := commitRequestFromUnstructured(u)
		if convErr != nil {
			return false
		}
		ready := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady)
		return ready != nil && ready.Reason == crReasonCommitted
	}, 10*time.Second, 50*time.Millisecond, "the request is seen, and served, through the new credential")
	assert.Equal(t, []string{"v1", "v2"}, clusters.builtFrom())
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

// Requests are served concurrently, and each step reads the server's fact streams: they are set
// once, never rewritten under a lock the readers do not take. Run with -race.
func TestCommitRequestServer_StepsShareTheFactStreams(t *testing.T) {
	s := &CommitRequestServer{FactStreams: queue.NewFactStreamSet()}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			s.core().streams.ensure([]string{"route-a", "route-b"}[i%2])
		})
	}
	wg.Wait()
	first := s.core().streams
	assert.Same(t, first, s.core().streams, "every step shares one set of streams")
}

// commitRequestWatchEnds are the ways a watch of a source cluster's CommitRequests ends without failing:
// cleanly, as every one does after commitRequestRelist, and with 410 Gone.
var commitRequestWatchEnds = []struct {
	name string
	end  func(*k8swatch.FakeWatcher)
}{
	{name: "clean end", end: func(w *k8swatch.FakeWatcher) { w.Stop() }},
	{name: "410 Gone", end: func(w *k8swatch.FakeWatcher) {
		w.Error(&apierrors.NewResourceExpired("too old resource version").ErrStatus)
	}},
}

// countCommitRequestLists counts the Lists of CommitRequests on source.
func countCommitRequestLists(source *dynamicfake.FakeDynamicClient) *atomic.Int32 {
	var lists atomic.Int32
	source.PrependReactor("list", "commitrequests", func(clienttesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	return &lists
}

// A watch that ends without failing lists again at once, rather than leave the namespace unwatched for
// commitRequestReconnect: a request created right after the end is served well within that wait.
func TestCommitRequestServer_AWatchThatEndsListsAgainAtOnce(t *testing.T) {
	for _, tc := range commitRequestWatchEnds {
		t.Run(tc.name, func(t *testing.T) {
			provider := kubeconfigProvider("edge", true)
			c := newCommitRequestClient(t, nil, mirroringTarget("edge"), provider)
			gvr := commitRequestGVR()
			source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{gvr: "CommitRequestList"})
			lists := countCommitRequestLists(source)
			first := k8swatch.NewFake()
			var watches atomic.Int32
			source.PrependWatchReactor("commitrequests",
				func(clienttesting.Action) (bool, k8swatch.Interface, error) {
					if watches.Add(1) == 1 {
						return true, first, nil
					}
					return false, nil, nil
				})
			s := &CommitRequestServer{
				Client: c, Finalizer: committed(), Log: logr.Discard(),
				Clusters: fakeClusters{dynamic: source, discovery: fakeDiscovery(nil,
					commitRequestResources("configbutler.ai/v1alpha3", "commitrequests"))},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.syncProviders(ctx)
			require.Eventually(t, func() bool { return watches.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
				"the first session watches")
			time.Sleep(commitRequestShortWatch) // the watch has run long enough to count as a whole one

			tc.end(first)
			request := sourceCommitRequest("relisted")
			request.SetResourceVersion("")
			_, err := source.Resource(gvr).Namespace("default").Create(ctx, request, metav1.CreateOptions{})
			require.NoError(t, err)

			require.Eventually(t, func() bool {
				u, getErr := source.Resource(gvr).Namespace("default").Get(ctx, "relisted", metav1.GetOptions{})
				if getErr != nil {
					return false
				}
				cr, convErr := commitRequestFromUnstructured(u)
				if convErr != nil {
					return false
				}
				ready := apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady)
				return ready != nil && ready.Reason == crReasonCommitted
			}, commitRequestReconnect/2, 20*time.Millisecond,
				"a request created as the watch ends is served without waiting commitRequestReconnect")
			assert.GreaterOrEqual(t, lists.Load(), int32(2), "the namespace was listed again")
		})
	}
}

// A watch a server closes at once, cleanly or with 410 Gone, waits commitRequestReconnect before listing
// again, so such a server is not listed in a loop: also when its list takes longer than
// commitRequestShortWatch, since only the watch's own run counts.
func TestCommitRequestServer_AWatchClosedAtOnceWaitsBeforeListingAgain(t *testing.T) {
	for _, tc := range commitRequestWatchEnds {
		for _, slowList := range []bool{false, true} {
			name := tc.name
			if slowList {
				name += ", slow list"
			}
			t.Run(name, func(t *testing.T) {
				source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
					map[schema.GroupVersionResource]string{commitRequestGVR(): "CommitRequestList"})
				lists := countCommitRequestLists(source)
				if slowList {
					source.PrependReactor("list", "commitrequests",
						func(clienttesting.Action) (bool, runtime.Object, error) {
							time.Sleep(commitRequestShortWatch + 100*time.Millisecond)
							return false, nil, nil
						})
				}
				source.PrependWatchReactor("commitrequests",
					func(clienttesting.Action) (bool, k8swatch.Interface, error) {
						w := k8swatch.NewFakeWithChanSize(1, false)
						tc.end(w)
						return true, w, nil
					})
				s := &CommitRequestServer{Log: logr.Discard()}
				watcher := &commitRequestWatcher{forbidden: map[string]bool{}, undeletable: map[string]bool{}}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					s.watchNamespace(ctx, logr.Discard(), watcher, source.Resource(commitRequestGVR()), "default")
				}()

				require.Eventually(t, func() bool { return lists.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
				time.Sleep(3 * commitRequestShortWatch)
				assert.Equal(t, int32(1), lists.Load(), "no second list before commitRequestReconnect")
				cancel()
				<-done
			})
		}
	}
}

// finishedSourceCommitRequest is a request on a source cluster whose save ended, kept until a
// delete-after time far away.
func finishedSourceCommitRequest(name string) *unstructured.Unstructured {
	u := sourceCommitRequest(name)
	u.SetAnnotations(map[string]string{
		CommitRequestDeleteAfterAnnotation: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
	})
	u.Object["status"] = map[string]any{"conditions": []any{map[string]any{
		"type": ConditionTypeReady, "status": string(metav1.ConditionTrue), "reason": crReasonCommitted,
		"message": "committed", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}}
	return u
}

// orderedGets rate-limits a source cluster's gets, outside the fake's own lock, so a list and watch
// never wait behind them. Each get takes the next turn, interval after the one before, in the order
// the gets were made, and is recorded by name once it holds its turn.
type orderedGets struct {
	interval time.Duration

	mu   sync.Mutex
	next time.Time
	took map[string]int
}

// turn records a get of name and waits for its turn.
func (o *orderedGets) turn(name string) {
	o.mu.Lock()
	at := time.Now()
	if o.next.After(at) {
		at = o.next
	}
	o.next = at.Add(o.interval)
	o.took[name]++
	o.mu.Unlock()
	time.Sleep(time.Until(at))
}

func (o *orderedGets) gets(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.took[name]
}

// throttledGets is a source cluster's client whose gets take turns from limit.
type throttledGets struct {
	dynamic.Interface

	limit *orderedGets
}

func (c throttledGets) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return throttledResource{NamespaceableResourceInterface: c.Interface.Resource(gvr), limit: c.limit}
}

type throttledResource struct {
	dynamic.NamespaceableResourceInterface

	limit *orderedGets
}

func (r throttledResource) Namespace(namespace string) dynamic.ResourceInterface {
	return throttledNamespace{ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace),
		limit: r.limit}
}

type throttledNamespace struct {
	dynamic.ResourceInterface

	limit *orderedGets
}

func (r throttledNamespace) Get(
	ctx context.Context, name string, options metav1.GetOptions, subresources ...string,
) (*unstructured.Unstructured, error) {
	r.limit.turn(name)
	return r.ResourceInterface.Get(ctx, name, options, subresources...)
}

// A relist serves each finished request from the listed object. Before, it got every one again through
// the source cluster's rate-limited client, and a request created meanwhile waited behind them all for
// its claim: here 40 finished requests at 10 gets a second would hold it about four seconds.
func TestCommitRequestServer_ARelistDoesNotQueueANewRequestBehindTheFinishedOnes(t *testing.T) {
	const finished = 40
	provider := kubeconfigProvider("edge", true)
	provider.Spec.CommitRequests = configv1alpha3.CommitRequestsReconcileAndDeleteFinished
	c := newCommitRequestClient(t, nil, mirroringTarget("edge"), provider)
	gvr := commitRequestGVR()
	stored := make([]runtime.Object, 0, finished)
	for i := range finished {
		stored = append(stored, finishedSourceCommitRequest(fmt.Sprintf("finished-%02d", i)))
	}
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "CommitRequestList"}, stored...)
	watching := make(chan struct{})
	var watchOnce sync.Once
	source.PrependWatchReactor("commitrequests", func(clienttesting.Action) (bool, k8swatch.Interface, error) {
		watchOnce.Do(func() { close(watching) })
		return false, nil, nil
	})
	limit := &orderedGets{interval: 100 * time.Millisecond, took: map[string]int{}}
	s := &CommitRequestServer{
		Client: c, Finalizer: committed(), TTL: time.Hour, Log: logr.Discard(),
		Clusters: fakeClusters{
			dynamic:   throttledGets{Interface: source, limit: limit},
			discovery: fakeDiscovery(nil, commitRequestResources("configbutler.ai/v1alpha3", "commitrequests")),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.syncProviders(ctx)

	// The session serves every listed request before it opens the watch. Wait until each has either
	// taken a turn for a get or finished its step, so the new request's turn comes after any of theirs.
	select {
	case <-watching:
	case <-time.After(5 * time.Second):
		t.Fatal("the session never watched")
	}
	s.mu.Lock()
	watcher := s.watchers[provider.Key()]
	s.mu.Unlock()
	require.NotNil(t, watcher)
	require.Eventually(t, func() bool {
		for _, u := range stored {
			name := u.(*unstructured.Unstructured).GetName()
			if _, serving := watcher.serving.Load(u.(*unstructured.Unstructured).GetUID()); serving &&
				limit.gets(name) == 0 {
				return false
			}
		}
		return true
	}, 5*time.Second, time.Millisecond, "every listed request has taken a turn or finished its step")

	created := time.Now()
	require.NoError(t, source.Tracker().Create(gvr, sourceCommitRequest("new"), "default"))
	claimed := func() bool {
		u, err := source.Tracker().Get(gvr, "default", "new")
		if err != nil {
			return false
		}
		cr, err := commitRequestFromUnstructured(u.(*unstructured.Unstructured))
		return err == nil && apimeta.FindStatusCondition(cr.Status.Conditions, ConditionTypeReady) != nil
	}
	assert.Eventually(t, claimed, 2*time.Second, 20*time.Millisecond,
		"the new request is claimed without waiting behind the finished ones")
	t.Logf("claimed %s after it was created", time.Since(created).Round(10*time.Millisecond))
	for _, u := range stored {
		name := u.(*unstructured.Unstructured).GetName()
		assert.Zero(t, limit.gets(name), "finished request %s is not read again after the list returned it", name)
	}
}

// An event can deliver a request older than the stored one: the write of its claim, arriving after the
// steps that pushed it. A claimed request is read again before it is attached, so an old object never
// attaches a request whose save has ended.
func TestCommitRequestServer_AnOldEventOfAClaimedRequestIsReadAgain(t *testing.T) {
	cr := newCommitRequest("save")
	cr.Spec.GitTargetRef.Namespace = "default"
	f := committed()
	c := newCommitRequestClient(t, nil, cr, mirroringTarget("edge"), kubeconfigProvider("edge", true))
	server := &CommitRequestServer{Client: c, Finalizer: f}
	watcher := &commitRequestWatcher{
		provider: types.NamespacedName{Namespace: "default", Name: "edge"}, path: watcherPath(c, "edge", false),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliver := func(cr *configv1alpha3.CommitRequest) {
		u, err := commitRequestToUnstructured(cr)
		require.NoError(t, err)
		server.consider(ctx, watcher, u)
		require.Eventually(t, func() bool {
			_, serving := watcher.serving.Load(cr.UID)
			return !serving
		}, 10*time.Second, time.Millisecond, "the delivered request's steps end")
	}

	stored := fetchCommitRequest(t, c, "save")
	claimEvent := stored.DeepCopy() // how the claim reads, as its event will deliver it
	var target configv1alpha3.GitTarget
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "team-a-config"}, &target))
	claimEvent.Status.GitTargetUID = string(target.UID)
	markCommitRequestProgressing(claimEvent, "", "")

	deliver(&stored)
	ready := commitRequestReady(t, c, "save")
	require.NotNil(t, ready)
	require.Equal(t, crReasonCommitted, ready.Reason, "the request is pushed")
	require.Len(t, f.calls, 1)

	deliver(claimEvent)
	assert.Len(t, f.calls, 1, "the old event attached nothing: the stored request had finished")
}
