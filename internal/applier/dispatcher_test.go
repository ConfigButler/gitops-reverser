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
	"k8s.io/apimachinery/pkg/runtime"
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

// gitTarget builds a GitTarget; a nil kustomizationRef declares no applier.
func gitTarget(name string, kustomizationRef *fluxmeta.NamespacedObjectReference, cluster string) *v1alpha3.GitTarget {
	target := &v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Namespace: targetNS, Name: name},
		Spec:       v1alpha3.GitTargetSpec{Branch: testBranch, Path: name},
	}
	if kustomizationRef != nil {
		target.Spec.Applier = &v1alpha3.GitTargetApplier{KustomizationRef: *kustomizationRef}
	}
	if cluster != "" {
		target.Spec.ClusterProviderRef = &fluxmeta.LocalObjectReference{Name: cluster}
	}
	return target
}

func ksRef(namespace, name string) *fluxmeta.NamespacedObjectReference {
	return &fluxmeta.NamespacedObjectReference{Namespace: namespace, Name: name}
}

func ref(name string) itypes.ResourceReference { return itypes.NewResourceReference(name, targetNS) }

// sourceStub hands out one fake source cluster's client, failing the first failFirst calls (or
// every call, with failFirst < 0), and remembers which cluster ids it was asked for.
type sourceStub struct {
	client    *dynamicfake.FakeDynamicClient
	mu        sync.Mutex
	failFirst int
	calls     int
	clusters  []string
}

func (s *sourceStub) get(_ context.Context, clusterID string) (dynamic.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.clusters = append(s.clusters, clusterID)
	if s.failFirst < 0 || s.calls <= s.failFirst {
		return nil, fmt.Errorf("source cluster unreachable (call %d)", s.calls)
	}
	return s.client, nil
}

func (s *sourceStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type dispatcherFixture struct {
	dispatcher *Dispatcher
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
	controlClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(targets...).Build()

	source := &sourceStub{client: newSourceClient(sourceObjs...)}
	recorder := record.NewFakeRecorder(64)
	d := NewDispatcher(controlClient, source.get, recorder, logr.Discard())
	d.Backoff = Backoff{Initial: time.Millisecond, Max: 4 * time.Millisecond, Attempts: 4}
	d.Now = func() time.Time { return stampTime }
	return &dispatcherFixture{dispatcher: d, source: source, recorder: recorder, reader: reader}
}

// start runs the dispatcher until the test ends.
func (f *dispatcherFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.dispatcher.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (f *dispatcherFixture) count(reason v1alpha3.ApplyReason, outcome string) int64 {
	value, _ := telemetry.CollectInt64Sum(f.reader, applyRequestsMetric, map[string]string{
		"applier_kind": applierKindKustomization, "reason": string(reason), "outcome": outcome,
	})
	return value
}

// waitForCount waits until the counter reaches want.
func (f *dispatcherFixture) waitForCount(t *testing.T, reason v1alpha3.ApplyReason, outcome string, want int64) {
	t.Helper()
	require.Eventually(t, func() bool { return f.count(reason, outcome) == want },
		5*time.Second, time.Millisecond, "apply_requests_total{reason=%s,outcome=%s} never reached %d (at %d)",
		reason, outcome, want, f.count(reason, outcome))
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

// TestDispatcher_AsksEachApplierOnce: targets sharing an applier ask it once; a target with no
// applier, or one that is gone, asks nothing; an omitted kustomizationRef namespace is the target's.
func TestDispatcher_AsksEachApplierOnce(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{
			gitTarget("a", ksRef(fluxSystemNS, "checkout"), ""),
			gitTarget("b", ksRef(fluxSystemNS, "checkout"), ""),
			gitTarget("c", ksRef("", "own"), "prod-eu-1"),
			gitTarget("no-applier", nil, ""),
		},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
		kustomizationObj(targetNS, "own", gitRepoSourceRef("own")),
		gitRepositoryObj(targetNS, "own", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a"), ref("b"), ref("c"), ref("no-applier"), ref("deleted")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeSent, 2)

	assert.ElementsMatch(t, []string{"gitrepositories " + fluxSystemNS, "gitrepositories " + targetNS}, f.patches(),
		"one source stamp per applier, however many targets share it")
	assert.ElementsMatch(t, []string{v1alpha3.DefaultClusterProviderName, "prod-eu-1"}, f.source.clusters,
		"each applier is looked up in its target's source cluster")

	want := v1alpha3.GitTargetApplierStatus{
		RequestedCommit: testCommit,
		RequestedAt:     &metav1.Time{Time: stampTime},
		Reason:          v1alpha3.ApplyReasonForeignCommit,
	}
	for _, name := range []string{"a", "b", "c"} {
		got, ok := f.dispatcher.LastRequest(ref(name))
		require.True(t, ok, name)
		assert.True(t, got.RequestedAt.Equal(want.RequestedAt), name)
		assert.Equal(t, want.RequestedCommit, got.RequestedCommit, name)
		assert.Equal(t, want.Reason, got.Reason, name)
	}
	_, ok := f.dispatcher.LastRequest(ref("no-applier"))
	assert.False(t, ok, "a target without an applier records no request")

	assert.ElementsMatch(t, []string{
		"Normal ApplyRequested asked flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
		"Normal ApplyRequested asked flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)",
		"Normal ApplyRequested asked team-a/own to apply 4f2c1ab9 (ForeignCommit)",
	}, f.events())
}

// TestDispatcher_RetriesThenDelivers: a failed attempt is retried on the backoff, and only the
// delivery counts.
func TestDispatcher_RetriesThenDelivers(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.source.failFirst = 2
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonRefusal, outcomeSent, 1)

	assert.Equal(t, 3, f.source.callCount(), "two failures, then the attempt that landed")
	assert.Equal(t, int64(0), f.count(v1alpha3.ApplyReasonRefusal, outcomeFailed))
	assert.Equal(t, []string{"kustomizations " + fluxSystemNS}, f.patches(),
		"a refusal at the artifact's commit stamps the kustomization")
	got, ok := f.dispatcher.LastRequest(ref("a"))
	require.True(t, ok)
	assert.Equal(t, v1alpha3.ApplyReasonRefusal, got.Reason)
}

// TestDispatcher_GivesUp: a request that keeps failing is given up on after its last attempt, with a
// Warning Event, and is not recorded as made.
func TestDispatcher_GivesUp(t *testing.T) {
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")})
	f.source.failFirst = -1
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeFailed, 1)

	assert.Equal(t, f.dispatcher.Backoff.Attempts, f.source.callCount())
	_, ok := f.dispatcher.LastRequest(ref("a"))
	assert.False(t, ok)
	events := f.events()
	require.Len(t, events, 1)
	const want = "Warning ApplyRequestFailed could not ask flux-system/checkout to apply 4f2c1ab9 (ForeignCommit)" +
		" after 4 attempts"
	assert.True(t, strings.HasPrefix(events[0], want), events[0])
}

// TestDispatcher_PermanentFailureIsNotRetried: a Kustomization a request cannot work with fails
// once rather than retrying to the limit.
func TestDispatcher_PermanentFailureIsNotRetried(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", map[string]any{"kind": "Bucket", "name": "platform"}),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonForeignCommit, outcomeFailed, 1)
	assert.Equal(t, 1, f.source.callCount())
}

// TestDispatcher_NewerRequestSupersedes: a request waiting on its retry is replaced by a newer one
// for the same applier, which is sent at once with the newer commit. The newer one is a CatchUp
// and the older a Refusal, so it keeps the Refusal's reason: the artifact already holds the newer
// commit, and a CatchUp would then stamp nothing and drop the revert.
func TestDispatcher_NewerRequestSupersedes(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.dispatcher.Backoff = Backoff{Initial: time.Hour, Max: time.Hour, Attempts: 4}
	f.source.failFirst = 1
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal, Commit: olderCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	require.Eventually(t, func() bool { return f.source.callCount() == 1 }, 5*time.Second, time.Millisecond)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonCatchUp, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonRefusal, outcomeSent, 1)

	assert.Equal(t, int64(1), f.count(v1alpha3.ApplyReasonRefusal, outcomeSuperseded),
		"the failed Refusal at the older commit was superseded, not retried")
	assert.Equal(t, int64(0), f.count(v1alpha3.ApplyReasonCatchUp, outcomeSent))
	assert.Equal(t, []string{"kustomizations " + fluxSystemNS}, f.patches())
	got, ok := f.dispatcher.LastRequest(ref("a"))
	require.True(t, ok)
	assert.Equal(t, testCommit, got.RequestedCommit, "the newer commit is the one sent")
	assert.Equal(t, v1alpha3.ApplyReasonRefusal, got.Reason)
}

// TestDispatcher_CatchUpAlreadyCurrent: a CatchUp the applier already satisfies counts as sent and
// records nothing, since nothing was asked of it.
func TestDispatcher_CatchUpAlreadyCurrent(t *testing.T) {
	f := newDispatcherFixture(t,
		[]client.Object{gitTarget("a", ksRef(fluxSystemNS, "checkout"), "")},
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+testCommit),
	)
	f.start(t)

	f.dispatcher.RequestApply(git.ApplyRequest{
		Branch: testBranchKey, Reason: v1alpha3.ApplyReasonCatchUp, Commit: testCommit,
		Targets: []itypes.ResourceReference{ref("a")},
	})
	f.waitForCount(t, v1alpha3.ApplyReasonCatchUp, outcomeSent, 1)

	assert.Empty(t, f.patches())
	assert.Empty(t, f.events())
	_, ok := f.dispatcher.LastRequest(ref("a"))
	assert.False(t, ok)
}

// TestDispatcher_RequestApplyNeverBlocks: with no loop running, requests queue without blocking; an
// identical scope is replaced in place, and past the bound the oldest is dropped.
func TestDispatcher_RequestApplyNeverBlocks(t *testing.T) {
	f := newDispatcherFixture(t, nil)
	d := f.dispatcher

	same := func(commit string) git.ApplyRequest {
		return git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit, Commit: commit,
			Targets: []itypes.ResourceReference{ref("a"), ref("b")}}
	}
	d.RequestApply(same(olderCommit))
	reordered := same(testCommit)
	reordered.Targets = []itypes.ResourceReference{ref("b"), ref("a")}
	d.RequestApply(reordered)
	d.RequestApply(git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonForeignCommit})

	d.inboxMu.Lock()
	require.Len(t, d.inbox, 1, "an identical scope is replaced, and an empty request is ignored")
	assert.Equal(t, testCommit, d.inbox[0].Commit)
	d.inboxMu.Unlock()

	for i := range maxInbox + 10 {
		d.RequestApply(git.ApplyRequest{Branch: testBranchKey, Reason: v1alpha3.ApplyReasonRefusal,
			Commit: testCommit, Targets: []itypes.ResourceReference{ref(fmt.Sprintf("t%d", i))}})
	}
	d.inboxMu.Lock()
	defer d.inboxMu.Unlock()
	assert.Len(t, d.inbox, maxInbox)
	assert.Equal(t, "t10", d.inbox[0].Targets[0].Name, "the oldest requests were dropped")
}

func TestBackoff_Delay(t *testing.T) {
	b := Backoff{Initial: 2 * time.Second, Max: time.Minute, Attempts: 8}
	for failures, want := range map[int]time.Duration{
		1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 5: 32 * time.Second, 6: time.Minute, 20: time.Minute,
	} {
		assert.Equal(t, want, b.delay(failures), "after %d failures", failures)
	}
}

func TestSupersedingReason(t *testing.T) {
	tests := []struct{ older, newer, want v1alpha3.ApplyReason }{
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonForeignCommit},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonCatchUp},
		{v1alpha3.ApplyReasonCatchUp, v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonRefusal},
		{v1alpha3.ApplyReasonRefusal, v1alpha3.ApplyReasonForeignCommit, v1alpha3.ApplyReasonForeignCommit},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, supersedingReason(tt.older, tt.newer), "%s then %s", tt.older, tt.newer)
	}
}

func TestDescribeCommit(t *testing.T) {
	assert.Equal(t, "4f2c1ab9", describeCommit(testCommit))
	assert.Equal(t, "abc", describeCommit("abc"))
	assert.Equal(t, "the branch, which the remote does not carry", describeCommit(""))
}
