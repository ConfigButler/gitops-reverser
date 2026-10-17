// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	applyRequestsMetric = "gitopsreverser_apply_requests_total"
	targetNS            = "team-a"
)

var testBranchKey = git.BranchKey{RepoNamespace: targetNS, RepoName: "platform", Branch: testBranch}

// gitTarget builds a GitTarget on testBranchKey; a nil kustomizationRef declares no applier.
func gitTarget(name string, kustomizationRef *fluxmeta.NamespacedObjectReference, cluster string) *v1alpha3.GitTarget {
	target := &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: name, UID: uid(name)},
		Spec: v1alpha3.GitTargetSpec{
			GitProviderRef: fluxmeta.LocalObjectReference{Name: testBranchKey.RepoName},
			Branch:         testBranch,
			Path:           name,
		},
	}
	if kustomizationRef != nil {
		target.Spec.Applier = &v1alpha3.GitTargetApplier{KustomizationRef: kustomizationRef}
	}
	if cluster != "" {
		target.Spec.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: cluster}
	}
	return target
}

// reapplying makes a GitTarget consent to a Refusal request.
func reapplying(target *v1alpha3.GitTarget) *v1alpha3.GitTarget {
	target.Spec.OnRefusal = v1alpha3.RefusalActionReapply
	return target
}

// clusterProvider admits the GitTargets in the named namespaces.
func clusterProvider(name string, admits ...string) *v1alpha3.ClusterProvider {
	return &v1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha3.ClusterProviderSpec{AccessFrom: &v1alpha3.NamespaceMatcher{Names: admits}},
	}
}

// controlPlane is what every dispatcher test reads besides its GitTargets: the GitProvider of
// testBranchKey, and the ClusterProviders the tests name, admitting targetNS.
func controlPlane() []client.Object {
	return []client.Object{
		&v1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: testBranchKey.RepoNamespace, Name: testBranchKey.RepoName},
			Spec:       v1alpha3.GitProviderSpec{URL: testRepoURL},
		},
		clusterProvider(v1alpha3.DefaultClusterProviderName, targetNS),
		clusterProvider("prod-eu-1", targetNS),
	}
}

// checkoutRef names the Kustomization flux-system/checkout.
func checkoutRef() *fluxmeta.NamespacedObjectReference {
	return &fluxmeta.NamespacedObjectReference{Namespace: fluxSystemNS, Name: "checkout"}
}

func ref(name string) itypes.ResourceReference { return itypes.NewResourceReference(name, targetNS) }

// uid is the uid gitTarget gives the GitTarget of that name.
func uid(name string) types.UID { return types.UID("uid-" + name) }

// kustomizationApplier is a Kustomization applier in a cluster.
func kustomizationApplier(cluster, namespace, name string) git.ApplierRef {
	return git.ApplierRef{Cluster: cluster, Kind: v1alpha3.ApplierKindKustomization, Namespace: namespace, Name: name}
}

// checkout is the applier most tests ask: flux-system/checkout in the default cluster.
var checkout = kustomizationApplier(v1alpha3.DefaultClusterProviderName, fluxSystemNS, "checkout")

// request is one attempt for an applier to apply testCommit, for the named targets, all asking for
// reason.
func request(applier git.ApplierRef, reason v1alpha3.ApplyReason, names ...string) git.ApplyRequest {
	req := git.ApplyRequest{ID: 1, Branch: testBranchKey, Applier: applier, Reason: reason, Commit: testCommit}
	for _, name := range names {
		req.Targets = append(req.Targets, git.ApplyTarget{Ref: ref(name), UID: uid(name), Reason: reason})
	}
	return req
}

// sourceStub hands out one fake source cluster's client, failing every call while failing is set,
// and remembers which cluster ids it was asked for.
type sourceStub struct {
	client   *dynamicfake.FakeDynamicClient
	mu       sync.Mutex
	failing  bool
	clusters []string
}

func (s *sourceStub) get(_ context.Context, clusterID string) (dynamic.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters = append(s.clusters, clusterID)
	if s.failing {
		return nil, fmt.Errorf("source cluster %s unreachable", clusterID)
	}
	return s.client, nil
}

type dispatcherFixture struct {
	dispatcher *Dispatcher
	control    client.Client
	source     *sourceStub
	recorder   *record.FakeRecorder
	reader     *sdkmetric.ManualReader
}

func newDispatcherFixture(t *testing.T, targets []client.Object, sourceObjs ...runtime.Object) *dispatcherFixture {
	t.Helper()
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	controlClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(controlPlane(), targets...)...).Build()

	source := &sourceStub{client: newSourceClient(sourceObjs...)}
	recorder := record.NewFakeRecorder(64)
	d := NewDispatcher(controlClient, source.get, recorder, logr.Discard())
	d.Now = func() time.Time { return stampTime }
	d.ApplyWindowTimeout = 0 // the tests that wait for Flux set their own
	return &dispatcherFixture{dispatcher: d, control: controlClient, source: source, recorder: recorder, reader: reader}
}

// attempt carries out one request synchronously, as the dispatcher's goroutine does.
func (f *dispatcherFixture) attempt(req git.ApplyRequest) git.ApplyResult {
	return f.dispatcher.attempt(context.Background(), req)
}

func (f *dispatcherFixture) count(reason v1alpha3.ApplyReason, outcome string) int64 {
	return f.countKind(applierKindKustomization, reason, outcome)
}

func (f *dispatcherFixture) countKind(kind string, reason v1alpha3.ApplyReason, outcome string) int64 {
	value, _ := telemetry.CollectInt64Sum(f.reader, applyRequestsMetric, map[string]string{
		"applier_kind": kind, "reason": string(reason), "outcome": outcome,
	})
	return value
}

// events drains the recorded Events.
func (f *dispatcherFixture) events() []string {
	var out []string
	for {
		select {
		case event := <-f.recorder.Events:
			out = append(out, event)
		default:
			return out
		}
	}
}

func (f *dispatcherFixture) patches() []string {
	var out []string
	for _, action := range f.source.client.Actions() {
		if action.GetVerb() == "patch" {
			out = append(out, action.GetResource().Resource+" "+action.GetNamespace())
		}
	}
	return out
}

// behindSource is a Kustomization flux-system/checkout whose GitRepository on testBranch holds an
// older commit than testCommit, so any request that is sent stamps the GitRepository.
func behindSource() []runtime.Object {
	return []runtime.Object{
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
	}
}

// TestDispatcher_AsksOnceForEveryTarget: one attempt asks its applier once for all its targets,
// records each target's own reason, and looks the applier up in its source cluster.
func TestDispatcher_AsksOnceForEveryTarget(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{
			reapplying(gitTarget("a", checkoutRef(), "")),
			gitTarget("b", checkoutRef(), ""),
		},
		behindSource()...,
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Targets = append(
		req.Targets,
		git.ApplyTarget{Ref: ref("b"), UID: uid("b"), Reason: v1alpha3.ApplyReasonCatchUp},
	)

	res := f.attempt(req)

	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyDelivered}, res)
	assert.Equal(t, []string{"gitrepositories " + fluxSystemNS}, f.patches(), "one stamp for the applier")
	assert.Equal(t, []string{v1alpha3.DefaultClusterProviderName}, f.source.clusters)
	for name, want := range map[string]v1alpha3.ApplyReason{
		"a": v1alpha3.ApplyReasonRefusal, "b": v1alpha3.ApplyReasonCatchUp,
	} {
		got, ok := f.dispatcher.LastRequest(ref(name), uid(name))
		require.True(t, ok, name)
		assert.Equal(t, testCommit, got.RequestedCommit, name)
		assert.Equal(t, want, got.Reason, name)
		assert.True(t, got.RequestedAt.Equal(&metav1.Time{Time: stampTime}), name)
	}
	assert.ElementsMatch(t, []string{
		"Normal ApplyRequested asked Kustomization flux-system/checkout to apply 4f2c1ab9 (Refusal)",
		"Normal ApplyRequested asked Kustomization flux-system/checkout to apply 4f2c1ab9 (CatchUp)",
	}, f.events())
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeSent))
}

// TestDispatcher_LooksTheApplierUpInItsCluster: the applier is the request's, in the source cluster
// it names, under its own namespace and name; here a GitTarget whose kustomizationRef omits the
// namespace and so names one in its own.
func TestDispatcher_LooksTheApplierUpInItsCluster(t *testing.T) {
	own := kustomizationApplier("prod-eu-1", targetNS, "own")
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("c", &fluxmeta.NamespacedObjectReference{Name: "own"}, "prod-eu-1")},
		kustomizationObj(targetNS, "own", gitRepoSourceRef("own-source")),
		gitRepositoryObj(targetNS, "own-source", testRepoURL, branchRef(testBranch), ""),
	)

	assert.Equal(t, git.ApplyDelivered, f.attempt(request(own, v1alpha3.ApplyReasonForeignCommit, "c")).Outcome)
	assert.Equal(t, []string{"prod-eu-1"}, f.source.clusters)
	assert.Equal(t, []string{"gitrepositories " + targetNS}, f.patches())
}

// appliedFrom puts a GitTarget's applier in the cluster another ClusterProvider names.
func appliedFrom(target *v1alpha3.GitTarget, cluster string) *v1alpha3.GitTarget {
	target.Spec.Applier.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: cluster}
	return target
}

// TestDispatcher_AsksAnApplierInItsOwnCluster: a GitTarget mirroring prod-eu-1 whose Kustomization
// lives in a management cluster is asked through that cluster's client, never the source's.
func TestDispatcher_AsksAnApplierInItsOwnCluster(t *testing.T) {
	hub := kustomizationApplier("hub", fluxSystemNS, "checkout")
	f := newDispatcherFixture(
		t,
		[]client.Object{
			appliedFrom(gitTarget("a", checkoutRef(), "prod-eu-1"), "hub"),
			clusterProvider("hub", targetNS),
		},
		behindSource()...,
	)

	assert.Equal(t, git.ApplyDelivered, f.attempt(request(hub, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Equal(t, []string{"hub"}, f.source.clusters)

	// The same Kustomization asked as if it lived in the source cluster is not this target's applier.
	f.source.clusters = nil
	sourceSide := kustomizationApplier("prod-eu-1", fluxSystemNS, "checkout")
	assert.Equal(t, git.ApplyDropped, f.attempt(request(sourceSide, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.source.clusters)
}

// TestDispatcher_AnApplierClusterThatDoesNotAdmitAsksNothing: the applier's ClusterProvider gates who
// may wield its credential exactly as a source's does, so a GitTarget its accessFrom does not admit
// asks nothing, though its own source admits it.
func TestDispatcher_AnApplierClusterThatDoesNotAdmitAsksNothing(t *testing.T) {
	hub := kustomizationApplier("hub", fluxSystemNS, "checkout")
	f := newDispatcherFixture(t,
		[]client.Object{
			appliedFrom(gitTarget("a", checkoutRef(), "prod-eu-1"), "hub"),
			clusterProvider("hub", "some-other-namespace"),
		},
		behindSource()...,
	)

	assert.Equal(t, git.ApplyDropped, f.attempt(request(hub, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.source.clusters, "the hub's client is never even asked for")
}

// TestDispatcher_AFailureIsReportedAndNeverRetried: retrying is the branch worker's decision, on a
// quiet branch. The dispatcher reports the failure, and records ApplyRequestFailed only for the
// first failure of a run.
func TestDispatcher_AFailureIsReportedAndNeverRetried(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")})
	f.source.failing = true

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))
	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.Error(t, res.Err)
	assert.Len(t, f.source.clusters, 1, "one attempt, no retry")
	events := f.events()
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(
		events[0],
		"Warning ApplyRequestFailed could not ask Kustomization flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
	), events[0])

	retry := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	retry.PriorFailures = 1
	assert.Equal(t, git.ApplyFailed, f.attempt(retry).Outcome)
	assert.Empty(t, f.events(), "a failure already reported is not reported again")
	assert.Equal(t, int64(2), f.count(v1alpha3.ApplyReasonForeignCommit, outcomeFailed))
	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok)
}

// TestDispatcher_ASourceMismatchIsRejectedAndNeverStamped is the first review's finding 2:
// ApplierConfigured said SourceMismatch, and a foreign commit stamped the other branch's source.
func TestDispatcher_ASourceMismatchIsRejectedAndNeverStamped(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef("release"), "release@sha1:"+olderCommit),
	)

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRejected, res.Outcome, "only a change to the applier can make it succeed")
	assert.Empty(t, f.patches())
}

// TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing is the first review's finding 1: the
// source client is shared by every GitTarget that declared the cluster, so a GitTarget its
// ClusterProvider does not admit drove the applier on another GitTarget's admission.
func TestDispatcher_ATargetItsClusterProviderDeniesAsksNothing(t *testing.T) {
	restricted := kustomizationApplier("restricted", fluxSystemNS, "checkout")
	f := newDispatcherFixture(t,
		[]client.Object{
			gitTarget("denied", checkoutRef(), "restricted"),
			clusterProvider("restricted", "some-other-namespace"),
		},
		behindSource()...,
	)

	res := f.attempt(request(restricted, v1alpha3.ApplyReasonForeignCommit, "denied"))

	assert.Equal(t, git.ApplyDropped, res.Outcome)
	assert.Empty(t, f.source.clusters, "the denied GitTarget's cluster client is never even asked for")
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonForeignCommit, outcomeDropped))
}

// TestDispatcher_WithdrawnConsentAsksNothing is the executor's half of the second review's first
// finding. A Refusal whose target withdrew consent used to become a CatchUp sent at once, which
// fetched a behind artifact without the quiet period. Now it asks nothing, even with the artifact
// behind, and the worker decides what is still owed.
func TestDispatcher_WithdrawnConsentAsksNothing(t *testing.T) {
	suspended := reapplying(gitTarget("a", checkoutRef(), ""))
	suspended.Spec.Suspend = true
	f := newDispatcherFixture(t,
		[]client.Object{suspended, gitTarget("b", checkoutRef(), "")},
		behindSource()...,
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Targets = append(
		req.Targets,
		git.ApplyTarget{Ref: ref("b"), UID: uid("b"), Reason: v1alpha3.ApplyReasonCatchUp},
	)

	res := f.attempt(req)

	assert.Equal(t, git.ApplyDropped, res.Outcome, "never a weaker request in its place")
	assert.Empty(t, f.patches())
}

// TestDispatcher_ATargetThatChangedAsksNothing: the attempt asks only for targets that are still the
// incarnation the worker decided for and still name the applier on its branch. The second review's
// third finding is the first case: a recreated GitTarget executed its predecessor's request.
func TestDispatcher_ATargetThatChangedAsksNothing(t *testing.T) {
	for name, change := range map[string]func(*v1alpha3.GitTarget){
		"it was deleted and recreated":   func(g *v1alpha3.GitTarget) { g.UID = "uid-recreated" },
		"it moved to another branch":     func(g *v1alpha3.GitTarget) { g.Spec.Branch = "release" },
		"it names another Kustomization": func(g *v1alpha3.GitTarget) { g.Spec.Applier.KustomizationRef.Name = "other" },
		"it names no applier any more":   func(g *v1alpha3.GitTarget) { g.Spec.Applier = nil },
		"it applies through another cluster": func(g *v1alpha3.GitTarget) {
			g.Spec.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: "prod-eu-1"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := gitTarget("a", checkoutRef(), "")
			change(target)
			f := newDispatcherFixture(t, []client.Object{target}, behindSource()...)

			res := f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a"))

			assert.Equal(t, git.ApplyDropped, res.Outcome)
			assert.Empty(t, f.patches())
			_, ok := f.dispatcher.LastRequest(ref("a"), target.UID)
			assert.False(t, ok, "and records nothing for the incarnation it finds")
		})
	}
}

// TestDispatcher_AStaleDecisionAsksNothing: the worker's decision is checked again just before the
// patch, and an attempt whose branch moved on or became busy reports Stale and stamps nothing.
func TestDispatcher_AStaleDecisionAsksNothing(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{reapplying(gitTarget("a", checkoutRef(), ""))},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	req := request(checkout, v1alpha3.ApplyReasonRefusal, "a")
	req.Current = func() bool { return false }

	assert.Equal(t, git.ApplyStale, f.attempt(req).Outcome)
	assert.Empty(t, f.patches())
	assert.Empty(t, f.events(), "a stale decision is no failure")
	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeStale))
}

// TestDispatcher_CatchUpAlreadyCurrent: a CatchUp the applier already satisfies counts as sent and
// records nothing, since nothing was asked of it.
func TestDispatcher_CatchUpAlreadyCurrent(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)

	res := f.attempt(request(checkout, v1alpha3.ApplyReasonCatchUp, "a"))

	assert.Equal(t, git.ApplyCurrent, res.Outcome)
	assert.Empty(t, f.patches())
	assert.Empty(t, f.events())
	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok)
}

// TestDispatcher_TheLedgerIsPerIncarnation: a GitTarget deleted and recreated under the same name
// does not inherit its predecessor's last request, and a forgotten one holds nothing.
func TestDispatcher_TheLedgerIsPerIncarnation(t *testing.T) {
	f := newDispatcherFixture(
		t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		behindSource()...)
	require.Equal(t, git.ApplyDelivered,
		f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)

	_, ok := f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.True(t, ok)
	_, ok = f.dispatcher.LastRequest(ref("a"), "the-recreated-one")
	assert.False(t, ok, "a recreated GitTarget does not inherit the last request")
	f.dispatcher.Forget(ref("a"))
	_, ok = f.dispatcher.LastRequest(ref("a"), uid("a"))
	assert.False(t, ok)
}

// TestDispatcher_RequestApplyReportsFromItsOwnGoroutine: RequestApply returns at once and the
// attempt reports when it ends; before Start, or after it, an attempt fails at once.
func TestDispatcher_RequestApplyReportsFromItsOwnGoroutine(t *testing.T) {
	f := newDispatcherFixture(
		t,
		[]client.Object{gitTarget("a", checkoutRef(), "")},
		behindSource()...)
	reports := make(chan git.ApplyResult, 1)
	req := request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")
	req.Report = func(res git.ApplyResult) { reports <- res }

	f.dispatcher.RequestApply(req)
	res := <-reports
	assert.Equal(t, git.ApplyFailed, res.Outcome, "not running yet")
	require.ErrorIs(t, res.Err, errNotRunning)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.dispatcher.Start(ctx)
	}()
	require.Eventually(t, func() bool {
		f.dispatcher.runMu.RLock()
		defer f.dispatcher.runMu.RUnlock()
		return f.dispatcher.run != nil
	}, 5*time.Second, time.Millisecond)

	f.dispatcher.RequestApply(req)
	assert.Equal(t, git.ApplyDelivered, (<-reports).Outcome)
	cancel()
	<-done
}

func TestStrongerApplyReason(t *testing.T) {
	tests := []struct{ older, newer, want v1alpha3.ApplyReason }{
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit},
		{"", v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, git.StrongerApplyReason(tt.older, tt.newer), "%s then %s", tt.older, tt.newer)
	}
}

func TestDescribeCommit(t *testing.T) {
	assert.Equal(t, "4f2c1ab9", describeCommit(testCommit))
	assert.Equal(t, "abc", describeCommit("abc"))
	assert.Equal(t, "the branch, which the remote does not carry", describeCommit(""))
}

// TestDispatcher_ADeliveredAttemptStaysOutUntilFluxFinished: the worker's attempt is the apply
// window, so a delivered one is reported only once Flux has finished the apply, or by time.
func TestDispatcher_ADeliveredAttemptStaysOutUntilFluxFinished(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	f.dispatcher.ApplyWindowTimeout = time.Minute
	f.dispatcher.HandledPollInterval = 5 * time.Millisecond

	done := make(chan git.ApplyResult, 1)
	go func() { done <- f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")) }()

	require.Eventually(t, func() bool { return len(f.patches()) == 1 }, 5*time.Second, 5*time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("reported %s before Flux finished the apply", res.Outcome)
	case <-time.After(50 * time.Millisecond):
	}

	setStatus(t, f.source.client, kustomizationGVR(), types.NamespacedName{Namespace: fluxSystemNS, Name: "checkout"},
		"main@sha1:"+testCommit, "lastAttemptedRevision")
	select {
	case res := <-done:
		assert.Equal(t, git.ApplyDelivered, res.Outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("not reported after Flux finished the apply")
	}
}

// TestDispatcher_AnApplyFluxNeverFinishesClosesByTime: Flux down, or a fetch that never lands,
// must not hold the applier's attempt out forever.
func TestDispatcher_AnApplyFluxNeverFinishesClosesByTime(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	f.dispatcher.ApplyWindowTimeout = 30 * time.Millisecond
	f.dispatcher.HandledPollInterval = 5 * time.Millisecond

	assert.Equal(t, git.ApplyDelivered, f.attempt(request(checkout, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
}

// argoTarget is the GitTarget "a", whose applier is the Application argocd/platform.
func argoTarget() *v1alpha3.GitTarget {
	target := gitTarget("a", nil, "")
	target.Spec.Applier = &v1alpha3.GitTargetApplier{
		ApplicationRef: &fluxmeta.NamespacedObjectReference{Namespace: argoNS, Name: "platform"},
	}
	return target
}

// argoPlatform is the applier argoTarget names.
var argoPlatform = git.ApplierRef{
	Cluster: v1alpha3.DefaultClusterProviderName, Kind: v1alpha3.ApplierKindApplication,
	Namespace: argoNS, Name: "platform",
}

func (f *dispatcherFixture) updates() []string {
	var out []string
	for _, action := range f.source.client.Actions() {
		if action.GetVerb() == "update" {
			out = append(out, action.GetResource().Resource+" "+action.GetNamespace())
		}
	}
	return out
}

// TestDispatcher_AsksAnApplicationToSync: an Application applier is asked through the Argo CD
// client, with the commit pinned, counted and recorded under its own kind.
func TestDispatcher_AsksAnApplicationToSync(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{reapplying(argoTarget())}, manualApp())

	res := f.attempt(request(argoPlatform, v1alpha3.ApplyReasonRefusal, "a"))

	assert.Equal(t, git.ApplyResult{ID: 1, Outcome: git.ApplyDelivered}, res)
	assert.Equal(t, []string{"applications " + argoNS}, f.updates())
	assert.Empty(t, f.patches(), "no Flux object is stamped")
	app, err := f.source.client.Resource(applicationGVR()).Namespace(argoNS).
		Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	revision, _, _ := unstructured.NestedString(app.Object, "operation", "sync", "revision")
	assert.Equal(t, testCommit, revision)
	assert.Equal(t, []string{
		"Normal ApplyRequested asked Application argocd/platform to apply 4f2c1ab9 (Refusal)",
	}, f.events())
	assert.Equal(t, int64(1), f.countKind(applierKindApplication, v1alpha3.ApplyReasonRefusal, outcomeSent))
	assert.Zero(t, f.count(v1alpha3.ApplyReasonRefusal, outcomeSent))
}

// TestDispatcher_AnApplicationWithAutomatedSyncIsRejectedAndNeverSynced: the standard is full
// control, so an Application that syncs on its own is never asked to, whatever its verdict says.
func TestDispatcher_AnApplicationWithAutomatedSyncIsRejectedAndNeverSynced(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()},
		applicationObj(testRepoURL, testBranch, map[string]any{"automated": map[string]any{"prune": true}}))

	res := f.attempt(request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyRejected, res.Outcome)
	require.ErrorContains(t, res.Err, "has automated sync on")
	assert.Empty(t, f.updates())
	assert.Equal(t, int64(1), f.countKind(applierKindApplication, v1alpha3.ApplyReasonForeignCommit, outcomeFailed))
}

// TestDispatcher_AnApplicationIsNotTheKustomizationOfItsName: the kind is part of which applier a
// GitTarget names, so a request for a Kustomization of the same name asks nothing for it.
func TestDispatcher_AnApplicationIsNotTheKustomizationOfItsName(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, manualApp())
	sameName := kustomizationApplier(v1alpha3.DefaultClusterProviderName, argoNS, "platform")

	assert.Equal(t, git.ApplyDropped, f.attempt(request(sameName, v1alpha3.ApplyReasonForeignCommit, "a")).Outcome)
	assert.Empty(t, f.updates())
}

// TestDispatcher_AnApplicationAttemptWaitsWithinItsWindow: an Argo CD attempt may wait out an
// operation already running, so it is bounded by the apply window on top of the attempt timeout.
func TestDispatcher_AnApplicationAttemptWaitsWithinItsWindow(t *testing.T) {
	running := manualApp()
	_ = unstructured.SetNestedMap(running.Object, theirOperation(), "operation")
	f := newDispatcherFixture(t, []client.Object{argoTarget()}, running)
	f.dispatcher.AttemptTimeout = 30 * time.Millisecond
	f.dispatcher.ApplyWindowTimeout = 100 * time.Millisecond
	f.dispatcher.HandledPollInterval = time.Millisecond
	f.dispatcher.Now = time.Now

	res := f.attempt(request(argoPlatform, v1alpha3.ApplyReasonForeignCommit, "a"))

	assert.Equal(t, git.ApplyFailed, res.Outcome)
	require.ErrorContains(t, res.Err, "has an operation in progress")
	require.NotErrorIs(t, res.Err, context.DeadlineExceeded,
		"the wait ends by the operation wait, not by the shorter attempt timeout")
	assert.Empty(t, f.updates())
}
